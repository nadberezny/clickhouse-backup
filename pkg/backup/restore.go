package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/AlexAkulov/clickhouse-backup/pkg/common"
	"github.com/AlexAkulov/clickhouse-backup/pkg/config"

	"github.com/mattn/go-shellwords"

	"github.com/AlexAkulov/clickhouse-backup/pkg/clickhouse"
	"github.com/AlexAkulov/clickhouse-backup/pkg/filesystemhelper"
	"github.com/AlexAkulov/clickhouse-backup/pkg/metadata"
	"github.com/AlexAkulov/clickhouse-backup/pkg/utils"
	apexLog "github.com/apex/log"
	recursive_copy "github.com/otiai10/copy"
	"github.com/yargevad/filepathx"
)

// Restore - restore tables matched by tablePattern from backupName
func Restore(cfg *config.Config, backupName, tablePattern string, databaseMapping, partitions []string, schemaOnly, dataOnly, dropTable, rbacOnly, configsOnly bool) error {
	for i := 0; i < len(databaseMapping); i++ {
		splitByCommas := strings.Split(databaseMapping[i], ",")
		for _, m := range splitByCommas {
			splitByColon := strings.Split(m, ":")
			if len(splitByColon) != 2 {
				return fmt.Errorf("restore-database-mapping %s should only have srcDatabase:destinationDatabase format for each map rule", m)
			}
			cfg.General.RestoreDatabaseMapping[splitByColon[0]] = splitByColon[1]
		}
	}

	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})
	doRestoreData := !schemaOnly || dataOnly

	ch := &clickhouse.ClickHouse{
		Config: &cfg.ClickHouse,
	}
	if backupName == "" {
		_ = PrintLocalBackups(cfg, "all")
		return fmt.Errorf("select backup for restore")
	}
	if err := ch.Connect(); err != nil {
		return fmt.Errorf("can't connect to clickhouse: %v", err)
	}
	defer ch.Close()
	disks, err := ch.GetDisks()
	if err != nil {
		return err
	}
	defaultDataPath, err := ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	backupMetafileLocalPath := path.Join(defaultDataPath, "backup", backupName, "metadata.json")
	backupMetadataBody, err := ioutil.ReadFile(backupMetafileLocalPath)
	if err == nil {
		backupMetadata := metadata.BackupMetadata{}
		if err := json.Unmarshal(backupMetadataBody, &backupMetadata); err != nil {
			return err
		}
		if schemaOnly || doRestoreData {
			for _, database := range backupMetadata.Databases {
				if targetDB, isMapped := cfg.General.RestoreDatabaseMapping[database.Name]; isMapped {
					// When create database, try to substitute the database by following the database mapping rule.
					if !IsInformationSchema(targetDB) {
						substitution := fmt.Sprintf("CREATE DATABASE ${1}%v${3}", targetDB)
						if err := ch.CreateDatabaseFromQuery(clickhouse.CreateDatabaseRE.ReplaceAllString(database.Query, substitution)); err != nil {
							return err
						}
					}
				} else {
					if !IsInformationSchema(database.Name) {
						if err := ch.CreateDatabaseFromQuery(database.Query); err != nil {
							return err
						}
					}
				}
			}
			for _, function := range backupMetadata.Functions {
				if err := ch.CreateUserDefinedFunction(function.Name, function.CreateQuery); err != nil {
					return err
				}
			}
		}
		if len(backupMetadata.Tables) == 0 {
			log.Warnf("'%s' doesn't contains tables for restore", backupName)
			if (!rbacOnly) && (!configsOnly) {
				return nil
			}
		}
	} else if !os.IsNotExist(err) { // Legacy backups don't contain metadata.json
		return err
	}
	needRestart := false
	if rbacOnly {
		if err := restoreRBAC(ch, backupName, disks); err != nil {
			return err
		}
		needRestart = true
	}
	if configsOnly {
		if err := restoreConfigs(ch, backupName, disks); err != nil {
			return err
		}
		needRestart = true
	}

	if needRestart {
		log.Warnf("%s contains `access` or `configs` directory, so we need exec %s", backupName, ch.Config.RestartCommand)
		cmd, err := shellwords.Parse(ch.Config.RestartCommand)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		log.Infof("run %s", ch.Config.RestartCommand)
		var out []byte
		if len(cmd) > 1 {
			out, err = exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		} else {
			out, err = exec.CommandContext(ctx, cmd[0]).CombinedOutput()
		}
		cancel()
		log.Debug(string(out))
		return err
	}

	if schemaOnly || (schemaOnly == dataOnly) {
		if err := RestoreSchema(cfg, ch, backupName, tablePattern, dropTable, disks); err != nil {
			return err
		}
	}
	if dataOnly || (schemaOnly == dataOnly) {
		partitionsToRestore := filesystemhelper.CreatePartitionsToBackupMap(partitions)
		if err := RestoreData(cfg, ch, backupName, tablePattern, partitionsToRestore, disks); err != nil {
			return err
		}
	}
	log.Info("done")
	return nil
}

// restoreRBAC - copy backup_name>/rbac folder to access_data_path
func restoreRBAC(ch *clickhouse.ClickHouse, backupName string, disks []clickhouse.Disk) error {
	accessPath, err := ch.GetAccessManagementPath(nil)
	if err != nil {
		return err
	}
	if err = restoreBackupRelatedDir(ch, backupName, "access", accessPath, disks); err == nil {
		markFile := path.Join(accessPath, "need_rebuild_lists.mark")
		apexLog.Infof("create %s for properly rebuild RBAC after restart clickhouse-server", markFile)
		file, err := os.Create(markFile)
		if err != nil {
			return err
		}
		_ = file.Close()
		_ = filesystemhelper.Chown(markFile, ch, disks)
		listFilesPattern := path.Join(accessPath, "*.list")
		apexLog.Infof("remove %s for properly rebuild RBAC after restart clickhouse-server", listFilesPattern)
		if listFiles, err := filepathx.Glob(listFilesPattern); err != nil {
			return err
		} else {
			for _, f := range listFiles {
				if err := os.Remove(f); err != nil {
					return err
				}
			}
		}
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// restoreConfigs - copy backup_name/configs folder to /etc/clickhouse-server/
func restoreConfigs(ch *clickhouse.ClickHouse, backupName string, disks []clickhouse.Disk) error {
	if err := restoreBackupRelatedDir(ch, backupName, "configs", ch.Config.ConfigDir, disks); err != nil && os.IsNotExist(err) {
		return nil
	} else {
		return err
	}
}

func restoreBackupRelatedDir(ch *clickhouse.ClickHouse, backupName, backupPrefixDir, destinationDir string, disks []clickhouse.Disk) error {
	defaultDataPath, err := ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	srcBackupDir := path.Join(defaultDataPath, "backup", backupName, backupPrefixDir)
	info, err := os.Stat(srcBackupDir)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return fmt.Errorf("%s is not a dir", srcBackupDir)
	}
	apexLog.Debugf("copy %s -> %s", srcBackupDir, destinationDir)
	copyOptions := recursive_copy.Options{OnDirExists: func(src, dest string) recursive_copy.DirExistsAction {
		return recursive_copy.Merge
	}}
	if err := recursive_copy.Copy(srcBackupDir, destinationDir, copyOptions); err != nil {
		return err
	}

	files, err := filepathx.Glob(path.Join(destinationDir, "**"))
	if err != nil {
		return err
	}
	files = append(files, destinationDir)
	for _, localFile := range files {
		if err := filesystemhelper.Chown(localFile, ch, disks); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSchema - restore schemas matched by tablePattern from backupName
func RestoreSchema(cfg *config.Config, ch *clickhouse.ClickHouse, backupName string, tablePattern string, dropTable bool, disks []clickhouse.Disk) error {
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})

	defaultDataPath, err := ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	version, err := ch.GetVersion()
	if err != nil {
		return err
	}
	metadataPath := path.Join(defaultDataPath, "backup", backupName, "metadata")
	info, err := os.Stat(metadataPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a dir", metadataPath)
	}
	if tablePattern == "" {
		tablePattern = "*"
	}
	tablesForRestore, err := getTableListByPatternLocal(metadataPath, tablePattern, ch.Config.SkipTables, dropTable, nil)
	if err != nil {
		return err
	}
	// if restore-database-mapping specified, create database in mapping rules instead of in backup files.
	if len(cfg.General.RestoreDatabaseMapping) > 0 {
		err = changeTableQueryToAdjustDatabaseMapping(&tablesForRestore, cfg.General.RestoreDatabaseMapping)
		if err != nil {
			return err
		}
	}
	if len(tablesForRestore) == 0 {
		return fmt.Errorf("no have found schemas by %s in %s", tablePattern, backupName)
	}

	if dropErr := dropExistsTables(cfg, ch, tablesForRestore, version, log); dropErr != nil {
		return dropErr
	}

	if restoreErr := createTables(cfg, ch, tablesForRestore, version, log); restoreErr != nil {
		return restoreErr
	}
	return nil
}

func createTables(cfg *config.Config, ch *clickhouse.ClickHouse, tablesForRestore ListOfTables, version int, log *apexLog.Entry) error {
	totalRetries := len(tablesForRestore)
	restoreRetries := 0
	var restoreErr error
	for restoreRetries < totalRetries {
		var notRestoredTables ListOfTables
		for _, schema := range tablesForRestore {
			// if metadata.json doesn't contains "databases", we will re-create tables with default engine
			if err := ch.CreateDatabase(schema.Database); err != nil {
				return fmt.Errorf("can't create database '%s': %v", schema.Database, err)
			}
			//materialized and window views should restore via ATTACH
			schema.Query = strings.Replace(
				schema.Query, "CREATE MATERIALIZED VIEW", "ATTACH MATERIALIZED VIEW", 1,
			)
			schema.Query = strings.Replace(
				schema.Query, "CREATE WINDOW VIEW", "ATTACH WINDOW VIEW", 1,
			)
			restoreErr = ch.CreateTable(clickhouse.Table{
				Database: schema.Database,
				Name:     schema.Table,
			}, schema.Query, false, cfg.General.RestoreSchemaOnCluster, version)

			if restoreErr != nil {
				restoreRetries++
				if restoreRetries >= totalRetries {
					return fmt.Errorf(
						"can't create table `%s`.`%s`: %v after %d times, please check your schema dependencies",
						schema.Database, schema.Table, restoreErr, restoreRetries,
					)
				} else {
					log.Warnf(
						"can't create table '%s.%s': %v, will try again", schema.Database, schema.Table, restoreErr,
					)
				}
				notRestoredTables = append(notRestoredTables, schema)
			}
		}
		tablesForRestore = notRestoredTables
		if len(tablesForRestore) == 0 {
			break
		}
	}
	return nil
}

func dropExistsTables(cfg *config.Config, ch *clickhouse.ClickHouse, tablesForDrop ListOfTables, version int, log *apexLog.Entry) error {
	var dropErr error
	dropRetries := 0
	totalRetries := len(tablesForDrop)
	for dropRetries < totalRetries {
		var notDroppedTables ListOfTables
		for _, schema := range tablesForDrop {
			dropErr = ch.DropTable(clickhouse.Table{
				Database: schema.Database,
				Name:     schema.Table,
			}, schema.Query, cfg.General.RestoreSchemaOnCluster, version)

			if dropErr != nil {
				dropRetries++
				if dropRetries >= totalRetries {
					return fmt.Errorf(
						"can't drop table `%s`.`%s`: %v after %d times, please check your schema dependencies",
						schema.Database, schema.Table, dropErr, dropRetries,
					)
				} else {
					log.Warnf(
						"can't drop table '%s.%s': %v, will try again", schema.Database, schema.Table, dropErr,
					)
				}
				notDroppedTables = append(notDroppedTables, schema)
			}
		}
		tablesForDrop = notDroppedTables
		if len(tablesForDrop) == 0 {
			break
		}
	}
	return nil
}

// RestoreData - restore data for tables matched by tablePattern from backupName
func RestoreData(cfg *config.Config, ch *clickhouse.ClickHouse, backupName string, tablePattern string, partitionsToRestore common.EmptyMap, disks []clickhouse.Disk) error {
	startRestore := time.Now()
	log := apexLog.WithFields(apexLog.Fields{
		"backup":    backupName,
		"operation": "restore",
	})
	defaultDataPath, err := ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	if clickhouse.IsClickhouseShadow(path.Join(defaultDataPath, "backup", backupName, "shadow")) {
		return fmt.Errorf("backups created in v0.0.1 is not supported now")
	}
	backup, _, err := getLocalBackup(cfg, backupName, disks)
	if err != nil {
		return fmt.Errorf("can't restore: %v", err)
	}
	var tablesForRestore ListOfTables
	if backup.Legacy {
		tablesForRestore, err = ch.GetBackupTablesLegacy(backupName, disks)
	} else {
		metadataPath := path.Join(defaultDataPath, "backup", backupName, "metadata")
		tablesForRestore, err = getTableListByPatternLocal(metadataPath, tablePattern, ch.Config.SkipTables, false, partitionsToRestore)
	}
	if err != nil {
		return err
	}
	if len(tablesForRestore) == 0 {
		return fmt.Errorf("no have found schemas by %s in %s", tablePattern, backupName)
	}
	log.Debugf("found %d tables with data in backup", len(tablesForRestore))
	chTables, err := ch.GetTables(tablePattern)
	if err != nil {
		return err
	}
	diskMap := map[string]string{}
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
	}
	for _, t := range tablesForRestore {
		for disk := range t.Parts {
			if _, diskExists := diskMap[disk]; !diskExists {
				log.Warnf("table '%s.%s' require disk '%s' that not found in clickhouse table system.disks, you can add nonexistent disks to `disk_mapping` in  `clickhouse` config section, data will restored to %s", t.Database, t.Table, disk, diskMap["default"])
				newDisk := clickhouse.Disk{
					Name: disk,
					Path: diskMap["default"],
					Type: "local",
				}
				found := false
				for _, d := range disks {
					if d.Name == disk {
						found = true
						break
					}
				}
				if !found {
					disks = append(disks, newDisk)
				}
			}
		}
	}
	dstTablesMap := map[metadata.TableTitle]clickhouse.Table{}
	for i := range chTables {
		dstTablesMap[metadata.TableTitle{
			Database: chTables[i].Database,
			Table:    chTables[i].Name,
		}] = chTables[i]
	}

	var missingTables []string
	for _, tableForRestore := range tablesForRestore {
		found := false
		if len(cfg.General.RestoreDatabaseMapping) > 0 {
			if targetDB, isMapped := cfg.General.RestoreDatabaseMapping[tableForRestore.Database]; isMapped {
				tableForRestore.Database = targetDB
			}
		}
		for _, chTable := range chTables {
			if (tableForRestore.Database == chTable.Database) && (tableForRestore.Table == chTable.Name) {
				found = true
				break
			}
		}
		if !found {
			missingTables = append(missingTables, fmt.Sprintf("'%s.%s'", tableForRestore.Database, tableForRestore.Table))
		}
	}
	if len(missingTables) > 0 {
		return fmt.Errorf("%s is not created. Restore schema first or create missing tables manually", strings.Join(missingTables, ", "))
	}

	for i, table := range tablesForRestore {
		// need mapped database path and original table.Database for CopyDataToDetached
		dstDatabase := table.Database
		if len(cfg.General.RestoreDatabaseMapping) > 0 {
			if targetDB, isMapped := cfg.General.RestoreDatabaseMapping[table.Database]; isMapped {
				dstDatabase = targetDB
				tablesForRestore[i].Database = targetDB
			}
		}
		log := log.WithField("table", fmt.Sprintf("%s.%s", dstDatabase, table.Table))
		dstTable, ok := dstTablesMap[metadata.TableTitle{
			Database: dstDatabase,
			Table:    table.Table}]
		if !ok {
			return fmt.Errorf("can't find '%s.%s' in current system.tables", dstDatabase, table.Table)
		}
		if err := filesystemhelper.CopyDataToDetached(backupName, table, disks, dstTable.DataPaths, ch); err != nil {
			return fmt.Errorf("can't restore '%s.%s': %v", table.Database, table.Table, err)
		}
		log.Debugf("copied data to 'detached'")
		if err := ch.AttachPartitions(tablesForRestore[i], disks); err != nil {
			return fmt.Errorf("can't attach partitions for table '%s.%s': %v", tablesForRestore[i].Database, tablesForRestore[i].Table, err)
		}
		log.Info("done")
	}
	log.WithField("duration", utils.HumanizeDuration(time.Since(startRestore))).Info("done")
	return nil
}
