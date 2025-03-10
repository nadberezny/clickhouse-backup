package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Altinity/clickhouse-backup/pkg/clickhouse"
	"github.com/Altinity/clickhouse-backup/pkg/common"
	"github.com/Altinity/clickhouse-backup/pkg/config"
	"github.com/Altinity/clickhouse-backup/pkg/filesystemhelper"
	"github.com/Altinity/clickhouse-backup/pkg/keeper"
	"github.com/Altinity/clickhouse-backup/pkg/metadata"
	"github.com/Altinity/clickhouse-backup/pkg/partition"
	"github.com/Altinity/clickhouse-backup/pkg/status"
	"github.com/Altinity/clickhouse-backup/pkg/storage"
	"github.com/Altinity/clickhouse-backup/pkg/storage/object_disk"
	"github.com/Altinity/clickhouse-backup/pkg/utils"

	apexLog "github.com/apex/log"
	"github.com/google/uuid"
	recursiveCopy "github.com/otiai10/copy"
)

const (
	// TimeFormatForBackup - default backup name format
	TimeFormatForBackup = "2006-01-02T15-04-05"
	MetaFileName        = "metadata.json"
)

var (
	// ErrUnknownClickhouseDataPath -
	ErrUnknownClickhouseDataPath = errors.New("clickhouse data path is unknown, you can set data_path in config file")
)

type LocalBackup struct {
	metadata.BackupMetadata
	Legacy bool
	Broken string
}

// NewBackupName - return default backup name
func NewBackupName() string {
	return time.Now().UTC().Format(TimeFormatForBackup)
}

// CreateBackup - create new backup of all tables matched by tablePattern
// If backupName is empty string will use default backup name
func (b *Backuper) CreateBackup(backupName, tablePattern string, partitions []string, schemaOnly, createRBAC, rbacOnly, createConfigs, configsOnly, skipCheckPartsColumns bool, version string, commandId int) error {
	ctx, cancel, err := status.Current.GetContextWithCancel(commandId)
	if err != nil {
		return err
	}
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	startBackup := time.Now()
	doBackupData := !schemaOnly && !rbacOnly && !configsOnly
	if backupName == "" {
		backupName = NewBackupName()
	}
	backupName = utils.CleanBackupNameRE.ReplaceAllString(backupName, "")
	log := b.log.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "create",
	})
	if err := b.ch.Connect(); err != nil {
		return fmt.Errorf("can't connect to clickhouse: %v", err)
	}
	defer b.ch.Close()

	if skipCheckPartsColumns && b.cfg.ClickHouse.CheckPartsColumns {
		b.cfg.ClickHouse.CheckPartsColumns = false
	}

	allDatabases, err := b.ch.GetDatabases(ctx, b.cfg, tablePattern)
	if err != nil {
		return fmt.Errorf("can't get database engines from clickhouse: %v", err)
	}
	tables, err := b.GetTables(ctx, tablePattern)
	if err != nil {
		return fmt.Errorf("can't get tables from clickhouse: %v", err)
	}
	i := 0
	for _, table := range tables {
		if table.Skip {
			continue
		}
		i++
	}
	if i == 0 && !b.cfg.General.AllowEmptyBackups {
		return fmt.Errorf("no tables for backup")
	}

	allFunctions, err := b.ch.GetUserDefinedFunctions(ctx)
	if err != nil {
		return fmt.Errorf("GetUserDefinedFunctions return error: %v", err)
	}

	disks, err := b.ch.GetDisks(ctx, false)
	if err != nil {
		return err
	}

	diskMap := make(map[string]string, len(disks))
	diskTypes := make(map[string]string, len(disks))
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
		diskTypes[disk.Name] = disk.Type
	}
	partitionsIdMap, partitionsNameList := partition.ConvertPartitionsToIdsMapAndNamesList(ctx, b.ch, tables, nil, partitions)
	// create
	if b.cfg.ClickHouse.UseEmbeddedBackupRestore {
		err = b.createBackupEmbedded(ctx, backupName, tablePattern, partitionsNameList, partitionsIdMap, schemaOnly, createRBAC, createConfigs, tables, allDatabases, allFunctions, disks, diskMap, diskTypes, log, startBackup, version)
	} else {
		err = b.createBackupLocal(ctx, backupName, partitionsIdMap, tables, doBackupData, schemaOnly, createRBAC, rbacOnly, createConfigs, configsOnly, version, disks, diskMap, diskTypes, allDatabases, allFunctions, log, startBackup)
	}
	if err != nil {
		return err
	}

	// Clean
	if err := b.RemoveOldBackupsLocal(ctx, true, disks); err != nil {
		return err
	}
	return nil
}

func (b *Backuper) createBackupLocal(ctx context.Context, backupName string, partitionsIdMap map[metadata.TableTitle]common.EmptyMap, tables []clickhouse.Table, doBackupData bool, schemaOnly bool, createRBAC, rbacOnly bool, createConfigs, configsOnly bool, version string, disks []clickhouse.Disk, diskMap, diskTypes map[string]string, allDatabases []clickhouse.Database, allFunctions []clickhouse.Function, log *apexLog.Entry, startBackup time.Time) error {
	// Create backup dir on all clickhouse disks
	for _, disk := range disks {
		if err := filesystemhelper.Mkdir(path.Join(disk.Path, "backup"), b.ch, disks); err != nil {
			return err
		}
	}
	defaultPath, err := b.ch.GetDefaultPath(disks)
	if err != nil {
		return err
	}
	backupPath := path.Join(defaultPath, "backup", backupName)
	if _, err := os.Stat(path.Join(backupPath, "metadata.json")); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("'%s' medatata.json already exists", backupName)
	}
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		if err = filesystemhelper.Mkdir(backupPath, b.ch, disks); err != nil {
			log.Errorf("can't create directory %s: %v", backupPath, err)
			return err
		}
	}
	var backupDataSize, backupMetadataSize uint64

	var tableMetas []metadata.TableTitle
	for _, table := range tables {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			log := log.WithField("table", fmt.Sprintf("%s.%s", table.Database, table.Name))
			if table.Skip {
				continue
			}
			var realSize map[string]int64
			var disksToPartsMap map[string][]metadata.Part
			if doBackupData && table.BackupType == clickhouse.ShardBackupFull {
				log.Debug("create data")
				shadowBackupUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
				disksToPartsMap, realSize, err = b.AddTableToBackup(ctx, backupName, shadowBackupUUID, disks, &table, partitionsIdMap[metadata.TableTitle{Database: table.Database, Table: table.Name}])
				if err != nil {
					log.Error(err.Error())
					if removeBackupErr := b.RemoveBackupLocal(ctx, backupName, disks); removeBackupErr != nil {
						log.Error(removeBackupErr.Error())
					}
					// fix corner cases after https://github.com/Altinity/clickhouse-backup/issues/379
					if cleanShadowErr := b.Clean(ctx); cleanShadowErr != nil {
						log.Error(cleanShadowErr.Error())
					}
					return err
				}
				// more precise data size calculation
				for _, size := range realSize {
					backupDataSize += uint64(size)
				}
			}
			// https://github.com/Altinity/clickhouse-backup/issues/529
			log.Debug("get in progress mutations list")
			inProgressMutations := make([]metadata.MutationMetadata, 0)
			if b.cfg.ClickHouse.BackupMutations && !schemaOnly && !rbacOnly && !configsOnly {
				inProgressMutations, err = b.ch.GetInProgressMutations(ctx, table.Database, table.Name)
				if err != nil {
					log.Error(err.Error())
					if removeBackupErr := b.RemoveBackupLocal(ctx, backupName, disks); removeBackupErr != nil {
						log.Error(removeBackupErr.Error())
					}
					return err
				}
			}
			log.Debug("create metadata")
			if schemaOnly || doBackupData {
				metadataSize, err := b.createTableMetadata(path.Join(backupPath, "metadata"), metadata.TableMetadata{
					Table:        table.Name,
					Database:     table.Database,
					Query:        table.CreateTableQuery,
					TotalBytes:   table.TotalBytes,
					Size:         realSize,
					Parts:        disksToPartsMap,
					Mutations:    inProgressMutations,
					MetadataOnly: schemaOnly || table.BackupType == clickhouse.ShardBackupSchema,
				}, disks)
				if err != nil {
					if removeBackupErr := b.RemoveBackupLocal(ctx, backupName, disks); removeBackupErr != nil {
						log.Error(removeBackupErr.Error())
					}
					return err
				}
				backupMetadataSize += metadataSize
				tableMetas = append(tableMetas, metadata.TableTitle{
					Database: table.Database,
					Table:    table.Name,
				})
			}
			log.Infof("done")
		}
	}
	backupRBACSize, backupConfigSize := uint64(0), uint64(0)

	if createRBAC || rbacOnly {
		if backupRBACSize, err = b.createBackupRBAC(ctx, backupPath, disks); err != nil {
			log.Fatalf("error during do RBAC backup: %v", err)
		} else {
			log.WithField("size", utils.FormatBytes(backupRBACSize)).Info("done createBackupRBAC")
		}
	}
	if createConfigs || configsOnly {
		if backupConfigSize, err = b.createBackupConfigs(ctx, backupPath); err != nil {
			log.Fatalf("error during do CONFIG backup: %v", err)
		} else {
			log.WithField("size", utils.FormatBytes(backupConfigSize)).Info("done createBackupConfigs")
		}
	}

	backupMetaFile := path.Join(defaultPath, "backup", backupName, "metadata.json")
	if err := b.createBackupMetadata(ctx, backupMetaFile, backupName, version, "regular", diskMap, diskTypes, disks, backupDataSize, backupMetadataSize, backupRBACSize, backupConfigSize, tableMetas, allDatabases, allFunctions, log); err != nil {
		return err
	}
	log.WithField("duration", utils.HumanizeDuration(time.Since(startBackup))).Info("done")
	return nil
}

func (b *Backuper) createBackupEmbedded(ctx context.Context, backupName, tablePattern string, partitionsNameList map[metadata.TableTitle][]string, partitionsIdMap map[metadata.TableTitle]common.EmptyMap, schemaOnly, createRBAC, createConfigs bool, tables []clickhouse.Table, allDatabases []clickhouse.Database, allFunctions []clickhouse.Function, disks []clickhouse.Disk, diskMap, diskTypes map[string]string, log *apexLog.Entry, startBackup time.Time, backupVersion string) error {
	// TODO: Implement sharded backup operations for embedded backups
	if doesShard(b.cfg.General.ShardedOperationMode) {
		return fmt.Errorf("cannot perform embedded backup: %w", errShardOperationUnsupported)
	}
	if _, isBackupDiskExists := diskMap[b.cfg.ClickHouse.EmbeddedBackupDisk]; !isBackupDiskExists {
		return fmt.Errorf("backup disk `%s` not exists in system.disks", b.cfg.ClickHouse.EmbeddedBackupDisk)
	}
	if createRBAC || createConfigs {
		return fmt.Errorf("`use_embedded_backup_restore: true` doesn't support --rbac, --configs parameters")
	}
	l := 0
	for _, table := range tables {
		if !table.Skip {
			l += 1
		}
	}
	if l == 0 {
		return fmt.Errorf("`use_embedded_backup_restore: true` doesn't allow empty backups, check your parameter --tables=%v", tablePattern)
	}
	tableMetas := make([]metadata.TableTitle, l)
	tablesSQL := ""
	tableSizeSQL := ""
	i := 0
	backupMetadataSize := uint64(0)
	backupPath := path.Join(diskMap[b.cfg.ClickHouse.EmbeddedBackupDisk], backupName)
	for _, table := range tables {
		if table.Skip {
			continue
		}
		tableMetas[i] = metadata.TableTitle{
			Database: table.Database,
			Table:    table.Name,
		}
		i += 1

		tablesSQL += "TABLE `" + table.Database + "`.`" + table.Name + "`"
		tableSizeSQL += "'" + table.Database + "." + table.Name + "'"
		if nameList, exists := partitionsNameList[metadata.TableTitle{Database: table.Database, Table: table.Name}]; exists && len(nameList) > 0 {
			tablesSQL += fmt.Sprintf(" PARTITIONS '%s'", strings.Join(nameList, "','"))
		}
		if i < l {
			tablesSQL += ", "
			tableSizeSQL += ", "
		}
	}
	backupSQL := fmt.Sprintf("BACKUP %s TO Disk(?,?)", tablesSQL)
	if schemaOnly {
		backupSQL += " SETTINGS structure_only=1, show_table_uuid_in_table_create_query_if_not_nil=1"
	}
	backupResult := make([]clickhouse.SystemBackups, 0)
	if err := b.ch.SelectContext(ctx, &backupResult, backupSQL, b.cfg.ClickHouse.EmbeddedBackupDisk, backupName); err != nil {
		return fmt.Errorf("backup error: %v", err)
	}
	if len(backupResult) != 1 || (backupResult[0].Status != "BACKUP_COMPLETE" && backupResult[0].Status != "BACKUP_CREATED") {
		return fmt.Errorf("backup return wrong results: %+v", backupResult)
	}
	backupDataSize := make([]struct {
		Size uint64 `ch:"backup_data_size"`
	}, 0)
	if !schemaOnly {
		if backupResult[0].CompressedSize == 0 {
			chVersion, err := b.ch.GetVersion(ctx)
			if err != nil {
				return err
			}
			backupSizeSQL := fmt.Sprintf("SELECT sum(bytes_on_disk) AS backup_data_size FROM system.parts WHERE active AND concat(database,'.',table) IN (%s)", tableSizeSQL)
			if chVersion >= 20005000 {
				backupSizeSQL = fmt.Sprintf("SELECT sum(total_bytes) AS backup_data_size FROM system.tables WHERE concat(database,'.',name) IN (%s)", tableSizeSQL)
			}
			if err := b.ch.SelectContext(ctx, &backupDataSize, backupSizeSQL); err != nil {
				return err
			}
		} else {
			backupDataSize = append(backupDataSize, struct {
				Size uint64 `ch:"backup_data_size"`
			}{Size: backupResult[0].CompressedSize})
		}
	} else {
		backupDataSize = append(backupDataSize, struct {
			Size uint64 `ch:"backup_data_size"`
		}{Size: 0})
	}

	log.Debug("calculate parts list from embedded backup disk")
	for _, table := range tables {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if table.Skip {
				continue
			}
			disksToPartsMap, err := b.getPartsFromBackupDisk(backupPath, table, partitionsIdMap[metadata.TableTitle{Database: table.Database, Table: table.Name}])
			if err != nil {
				if removeBackupErr := b.RemoveBackupLocal(ctx, backupName, disks); removeBackupErr != nil {
					log.Error(removeBackupErr.Error())
				}
				return err
			}
			metadataSize, err := b.createTableMetadata(path.Join(backupPath, "metadata"), metadata.TableMetadata{
				Table:        table.Name,
				Database:     table.Database,
				Query:        table.CreateTableQuery,
				TotalBytes:   table.TotalBytes,
				Size:         map[string]int64{b.cfg.ClickHouse.EmbeddedBackupDisk: 0},
				Parts:        disksToPartsMap,
				MetadataOnly: schemaOnly,
			}, disks)
			if err != nil {
				if removeBackupErr := b.RemoveBackupLocal(ctx, backupName, disks); removeBackupErr != nil {
					log.Error(removeBackupErr.Error())
				}
				return err
			}
			backupMetadataSize += metadataSize
		}
	}
	backupMetaFile := path.Join(diskMap[b.cfg.ClickHouse.EmbeddedBackupDisk], backupName, "metadata.json")
	if err := b.createBackupMetadata(ctx, backupMetaFile, backupName, backupVersion, "embedded", diskMap, diskTypes, disks, backupDataSize[0].Size, backupMetadataSize, 0, 0, tableMetas, allDatabases, allFunctions, log); err != nil {
		return err
	}

	log.WithFields(apexLog.Fields{
		"operation": "create_embedded",
		"duration":  utils.HumanizeDuration(time.Since(startBackup)),
	}).Info("done")

	return nil
}

func (b *Backuper) getPartsFromBackupDisk(backupPath string, table clickhouse.Table, partitionsIdsMap common.EmptyMap) (map[string][]metadata.Part, error) {
	parts := map[string][]metadata.Part{}
	dirList, err := os.ReadDir(path.Join(backupPath, "data", common.TablePathEncode(table.Database), common.TablePathEncode(table.Name)))
	if err != nil {
		if os.IsNotExist(err) {
			return parts, nil
		}
		return nil, err
	}
	if len(partitionsIdsMap) == 0 {
		parts[b.cfg.ClickHouse.EmbeddedBackupDisk] = make([]metadata.Part, len(dirList))
		for i, d := range dirList {
			parts[b.cfg.ClickHouse.EmbeddedBackupDisk][i] = metadata.Part{
				Name: d.Name(),
			}
		}
	} else {
		parts[b.cfg.ClickHouse.EmbeddedBackupDisk] = make([]metadata.Part, 0)
		for _, d := range dirList {
			found := false
			for prefix := range partitionsIdsMap {
				if strings.HasPrefix(d.Name(), prefix+"_") {
					found = true
					break
				}
			}
			if found {
				parts[b.cfg.ClickHouse.EmbeddedBackupDisk] = append(parts[b.cfg.ClickHouse.EmbeddedBackupDisk], metadata.Part{
					Name: d.Name(),
				})
			}
		}
	}
	return parts, nil
}

func (b *Backuper) createBackupConfigs(ctx context.Context, backupPath string) (uint64, error) {
	log := b.log.WithField("logger", "createBackupConfigs")
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		backupConfigSize := uint64(0)
		configBackupPath := path.Join(backupPath, "configs")
		log.Debugf("copy %s -> %s", b.cfg.ClickHouse.ConfigDir, configBackupPath)
		copyErr := recursiveCopy.Copy(b.cfg.ClickHouse.ConfigDir, configBackupPath, recursiveCopy.Options{
			Skip: func(srcinfo os.FileInfo, src, dest string) (bool, error) {
				backupConfigSize += uint64(srcinfo.Size())
				return false, nil
			},
		})
		return backupConfigSize, copyErr
	}
}

func (b *Backuper) createBackupRBAC(ctx context.Context, backupPath string, disks []clickhouse.Disk) (uint64, error) {
	log := b.log.WithField("logger", "createBackupRBAC")
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		rbacDataSize := uint64(0)
		rbacBackup := path.Join(backupPath, "access")
		accessPath, err := b.ch.GetAccessManagementPath(ctx, disks)
		if err != nil {
			return 0, err
		}
		accessPathInfo, err := os.Stat(accessPath)
		if err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err == nil && !accessPathInfo.IsDir() {
			return 0, fmt.Errorf("%s is not directory", accessPath)
		}
		if err == nil {
			log.Debugf("copy %s -> %s", accessPath, rbacBackup)
			copyErr := recursiveCopy.Copy(accessPath, rbacBackup, recursiveCopy.Options{
				Skip: func(srcinfo os.FileInfo, src, dest string) (bool, error) {
					rbacDataSize += uint64(srcinfo.Size())
					return false, nil
				},
			})
			if copyErr != nil {
				return 0, copyErr
			}
		} else {
			if err = os.MkdirAll(rbacBackup, 0755); err != nil {
				return 0, err
			}
		}
		replicatedRBACDataSize, err := b.createBackupRBACReplicated(ctx, rbacBackup)
		if err != nil {
			return 0, err
		}
		return rbacDataSize + replicatedRBACDataSize, nil
	}
}

func (b *Backuper) createBackupRBACReplicated(ctx context.Context, rbacBackup string) (replicatedRBACDataSize uint64, err error) {
	replicatedRBAC := make([]struct {
		Name string `ch:"name"`
	}, 0)
	rbacDataSize := uint64(0)
	if err = b.ch.SelectContext(ctx, &replicatedRBAC, "SELECT name FROM system.user_directories WHERE type='replicated'"); err == nil && len(replicatedRBAC) > 0 {
		k := keeper.Keeper{Log: b.log.WithField("logger", "keeper")}
		if err = k.Connect(ctx, b.ch, b.cfg); err != nil {
			return 0, err
		}
		defer k.Close()
		for _, userDirectory := range replicatedRBAC {
			replicatedAccessPath, err := k.GetReplicatedAccessPath(userDirectory.Name)
			if err != nil {
				return 0, err
			}
			dumpFile := path.Join(rbacBackup, userDirectory.Name+".jsonl")
			b.log.WithField("logger", "createBackupRBACReplicated").Infof("keeper.Dump %s -> %s", replicatedAccessPath, dumpFile)
			dumpRBACSize, dumpErr := k.Dump(replicatedAccessPath, dumpFile)
			if dumpErr != nil {
				return 0, dumpErr
			}
			rbacDataSize += uint64(dumpRBACSize)
		}
	}
	return rbacDataSize, nil
}

func (b *Backuper) AddTableToBackup(ctx context.Context, backupName, shadowBackupUUID string, diskList []clickhouse.Disk, table *clickhouse.Table, partitionsIdsMap common.EmptyMap) (map[string][]metadata.Part, map[string]int64, error) {
	log := b.log.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "create",
		"table":     fmt.Sprintf("%s.%s", table.Database, table.Name),
	})
	if backupName == "" {
		return nil, nil, fmt.Errorf("backupName is not defined")
	}

	if !strings.HasSuffix(table.Engine, "MergeTree") && table.Engine != "MaterializedMySQL" && table.Engine != "MaterializedPostgreSQL" {
		if table.Engine != "MaterializedView" {
			log.WithField("engine", table.Engine).Warnf("supports only schema backup")
		}
		return nil, nil, nil
	}
	if b.cfg.ClickHouse.CheckPartsColumns {
		if err := b.ch.CheckSystemPartsColumns(ctx, table); err != nil {
			return nil, nil, err
		}
	}
	// backup data
	if err := b.ch.FreezeTable(ctx, table, shadowBackupUUID); err != nil {
		return nil, nil, err
	}
	log.Debug("frozen")
	version, err := b.ch.GetVersion(ctx)
	if err != nil {
		return nil, nil, err
	}
	realSize := map[string]int64{}
	disksToPartsMap := map[string][]metadata.Part{}

	for _, disk := range diskList {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
			shadowPath := path.Join(disk.Path, "shadow", shadowBackupUUID)
			if _, err := os.Stat(shadowPath); err != nil && os.IsNotExist(err) {
				continue
			}
			backupPath := path.Join(disk.Path, "backup", backupName)
			encodedTablePath := path.Join(common.TablePathEncode(table.Database), common.TablePathEncode(table.Name))
			backupShadowPath := path.Join(backupPath, "shadow", encodedTablePath, disk.Name)
			if err := filesystemhelper.MkdirAll(backupShadowPath, b.ch, diskList); err != nil && !os.IsExist(err) {
				return nil, nil, err
			}
			// If partitionsIdsMap is not empty, only parts in this partition will back up.
			parts, size, err := filesystemhelper.MoveShadow(shadowPath, backupShadowPath, partitionsIdsMap)
			if err != nil {
				return nil, nil, err
			}
			realSize[disk.Name] = size
			disksToPartsMap[disk.Name] = parts
			log.WithField("disk", disk.Name).Debug("shadow moved")
			if disk.Type == "s3" || disk.Type == "azure_blob_storage" && len(parts) > 0 {
				if err = config.ValidateObjectDiskConfig(b.cfg); err != nil {
					return nil, nil, err
				}
				start := time.Now()
				if b.dst == nil {
					b.dst, err = storage.NewBackupDestination(ctx, b.cfg, b.ch, false, backupName)
					if err != nil {
						return nil, nil, err
					}
				}
				if err := b.dst.Connect(ctx); err != nil {
					return nil, nil, fmt.Errorf("can't connect to %s: %v", b.dst.Kind(), err)
				}
				if size, err = b.uploadObjectDiskParts(ctx, backupName, backupShadowPath, disk); err != nil {
					return disksToPartsMap, realSize, err
				}
				realSize[disk.Name] += size
				log.WithField("disk", disk.Name).WithField("duration", utils.HumanizeDuration(time.Since(start))).Info("object_disk data uploaded")
			}
			// Clean all the files under the shadowPath, cause UNFREEZE unavailable
			if version < 21004000 {
				if err := os.RemoveAll(shadowPath); err != nil {
					return disksToPartsMap, realSize, err
				}
			}
		}
	}
	// Unfreeze to unlock data on S3 disks, https://github.com/Altinity/clickhouse-backup/issues/423
	if version > 21004000 {
		if err := b.ch.QueryContext(ctx, fmt.Sprintf("ALTER TABLE `%s`.`%s` UNFREEZE WITH NAME '%s'", table.Database, table.Name, shadowBackupUUID)); err != nil {
			if (strings.Contains(err.Error(), "code: 60") || strings.Contains(err.Error(), "code: 81") || strings.Contains(err.Error(), "code: 218")) && b.cfg.ClickHouse.IgnoreNotExistsErrorDuringFreeze {
				b.ch.Log.Warnf("can't unfreeze table: %v", err)
			} else {
				return disksToPartsMap, realSize, err
			}

		}
	}
	if b.dst != nil {
		if err := b.dst.Close(ctx); err != nil {
			b.log.Warnf("uploadObjectDiskParts: can't close BackupDestination error: %v", err)
		}
	}
	log.Debug("done")
	return disksToPartsMap, realSize, nil
}

func (b *Backuper) uploadObjectDiskParts(ctx context.Context, backupName, backupShadowPath string, disk clickhouse.Disk) (int64, error) {
	var size int64
	var err error
	if err = object_disk.InitCredentialsAndConnections(ctx, b.ch, b.cfg, disk.Name); err != nil {
		return 0, err
	}

	if err := filepath.Walk(backupShadowPath, func(fPath string, fInfo os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fInfo.IsDir() {
			return nil
		}
		objPartFileMeta, err := object_disk.ReadMetadataFromFile(fPath)
		if err != nil {
			return err
		}
		var realSize, objSize int64
		// @TODO think about parallelization here after test pass
		for _, storageObject := range objPartFileMeta.StorageObjects {
			srcDiskConnection, exists := object_disk.DisksConnections[disk.Name]
			if !exists {
				return fmt.Errorf("uploadObjectDiskParts: %s not present in object_disk.DisksConnections", disk.Name)
			}
			if objSize, err = b.dst.CopyObject(
				ctx,
				srcDiskConnection.GetRemoteBucket(),
				path.Join(srcDiskConnection.GetRemotePath(), storageObject.ObjectRelativePath),
				path.Join(backupName, disk.Name, storageObject.ObjectRelativePath),
			); err != nil {
				return err
			}
			realSize += objSize
		}
		if realSize > objPartFileMeta.TotalSize {
			size += realSize
		} else {
			size += objPartFileMeta.TotalSize
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return size, nil
}

func (b *Backuper) createBackupMetadata(ctx context.Context, backupMetaFile, backupName, version, tags string, diskMap, diskTypes map[string]string, disks []clickhouse.Disk, backupDataSize, backupMetadataSize, backupRBACSize, backupConfigSize uint64, tableMetas []metadata.TableTitle, allDatabases []clickhouse.Database, allFunctions []clickhouse.Function, log *apexLog.Entry) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		backupMetadata := metadata.BackupMetadata{
			BackupName:              backupName,
			Disks:                   diskMap,
			DiskTypes:               diskTypes,
			ClickhouseBackupVersion: version,
			CreationDate:            time.Now().UTC(),
			Tags:                    tags,
			ClickHouseVersion:       b.ch.GetVersionDescribe(ctx),
			DataSize:                backupDataSize,
			MetadataSize:            backupMetadataSize,
			RBACSize:                backupRBACSize,
			ConfigSize:              backupConfigSize,
			Tables:                  tableMetas,
			Databases:               []metadata.DatabasesMeta{},
			Functions:               []metadata.FunctionsMeta{},
		}
		for _, database := range allDatabases {
			backupMetadata.Databases = append(backupMetadata.Databases, metadata.DatabasesMeta(database))
		}
		for _, function := range allFunctions {
			backupMetadata.Functions = append(backupMetadata.Functions, metadata.FunctionsMeta(function))
		}
		content, err := json.MarshalIndent(&backupMetadata, "", "\t")
		if err != nil {
			_ = b.RemoveBackupLocal(ctx, backupName, disks)
			return fmt.Errorf("can't marshal backup metafile json: %v", err)
		}
		if err := os.WriteFile(backupMetaFile, content, 0640); err != nil {
			_ = b.RemoveBackupLocal(ctx, backupName, disks)
			return err
		}
		if err := filesystemhelper.Chown(backupMetaFile, b.ch, disks, false); err != nil {
			log.Warnf("can't chown %s: %v", backupMetaFile, err)
		}
		return nil
	}
}

func (b *Backuper) createTableMetadata(metadataPath string, table metadata.TableMetadata, disks []clickhouse.Disk) (uint64, error) {
	if err := filesystemhelper.Mkdir(metadataPath, b.ch, disks); err != nil {
		return 0, err
	}
	metadataDatabasePath := path.Join(metadataPath, common.TablePathEncode(table.Database))
	if err := filesystemhelper.Mkdir(metadataDatabasePath, b.ch, disks); err != nil {
		return 0, err
	}
	metadataFile := path.Join(metadataDatabasePath, fmt.Sprintf("%s.json", common.TablePathEncode(table.Table)))
	metadataBody, err := json.MarshalIndent(&table, "", " ")
	if err != nil {
		return 0, fmt.Errorf("can't marshal %s: %v", MetaFileName, err)
	}
	if err := os.WriteFile(metadataFile, metadataBody, 0644); err != nil {
		return 0, fmt.Errorf("can't create %s: %v", MetaFileName, err)
	}
	if err := filesystemhelper.Chown(metadataFile, b.ch, disks, false); err != nil {
		return 0, err
	}
	return uint64(len(metadataBody)), nil
}
