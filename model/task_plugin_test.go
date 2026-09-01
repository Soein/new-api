package model

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"gorm.io/gorm/utils/tests"
)

func setupTaskPluginModelTest(t *testing.T) {
	t.Helper()
	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	var err error
	DB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, DB.AutoMigrate(&TaskPluginState{}, &TaskPlugin{}))
}

func TestConcurrentFirstTaskPluginUploadsCreateSingleActiveVersion(t *testing.T) {
	databasePath := t.TempDir() + "/task-plugin-first-upload.db"
	database, err := gorm.Open(sqlite.Open(databasePath+"?_busy_timeout=5000"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, database.AutoMigrate(&TaskPluginState{}, &TaskPlugin{}))

	originalDB := DB
	DB = database
	t.Cleanup(func() { DB = originalDB })
	start := make(chan struct{})
	errorsByVersion := make(chan error, 2)
	var group sync.WaitGroup
	for _, version := range []string{"1.0.0", "2.0.0"} {
		version := version
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errorsByVersion <- SaveTaskPlugin(&TaskPlugin{
				Key: "first-upload", APIVersion: 1, Version: version,
				Source: version, SourceHash: "hash-" + version, Enabled: true,
			})
		}()
	}
	close(start)
	group.Wait()
	close(errorsByVersion)
	for saveErr := range errorsByVersion {
		require.NoError(t, saveErr)
	}

	versions, err := ListTaskPluginVersions("first-upload")
	require.NoError(t, err)
	require.Len(t, versions, 2)
	active := 0
	for i := range versions {
		if versions[i].Active {
			active++
		}
	}
	assert.Equal(t, 1, active)
	var state TaskPluginState
	require.NoError(t, database.Where(&TaskPluginState{Key: "first-upload"}).First(&state).Error)
	assert.Equal(t, int64(2), state.Revision)
}

func TestTaskPluginVersionActivationAndSourceImmutability(t *testing.T) {
	setupTaskPluginModelTest(t)

	v1 := TaskPlugin{Key: "mock", APIVersion: 1, Version: "1.0.0", Source: "v1", SourceHash: "hash-v1", Enabled: true}
	require.NoError(t, SaveTaskPlugin(&v1))
	assert.True(t, v1.Active)

	v2 := TaskPlugin{Key: "mock", APIVersion: 1, Version: "2.0.0", Source: "v2", SourceHash: "hash-v2", Enabled: true}
	require.NoError(t, SaveTaskPlugin(&v2))
	assert.False(t, v2.Active)
	require.NoError(t, ActivateTaskPlugin("mock", "2.0.0"))

	active, err := ListActiveTaskPlugins()
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "2.0.0", active[0].Version)

	conflict := TaskPlugin{Key: "mock", APIVersion: 1, Version: "2.0.0", Source: "changed", SourceHash: "different", Enabled: true}
	err = SaveTaskPlugin(&conflict)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "different source")

	require.NoError(t, SetTaskPluginEnabled("mock", false))
	active, err = ListActiveTaskPlugins()
	require.NoError(t, err)
	assert.Empty(t, active)

	all, err := ListTaskPlugins()
	require.NoError(t, err)
	assert.Len(t, all, 2)
	deleteResult, err := DeleteTaskPluginVersion("mock", "2.0.0")
	require.NoError(t, err)
	assert.True(t, deleteResult.DeletedActive)
	require.NotNil(t, deleteResult.Promoted)
	assert.Equal(t, "1.0.0", deleteResult.Promoted.Version)
	versions, err := ListTaskPluginVersions("mock")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, "1.0.0", versions[0].Version)
	assert.True(t, versions[0].Active)
}

func TestRestoreTaskPluginActivationDoesNotOverwriteNewerActivation(t *testing.T) {
	databasePath := t.TempDir() + "/task-plugin-activation.db"
	openNode := func() *gorm.DB {
		database, err := gorm.Open(sqlite.Open(databasePath+"?_busy_timeout=5000"), &gorm.Config{})
		require.NoError(t, err)
		sqlDB, err := database.DB()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
		return database
	}

	nodeA := openNode()
	nodeB := openNode()
	require.NoError(t, nodeA.AutoMigrate(&TaskPluginState{}, &TaskPlugin{}))

	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	DB = nodeA
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		require.NoError(t, SaveTaskPlugin(&TaskPlugin{
			Key: "cas-plugin", APIVersion: 1, Version: version,
			Source: version, SourceHash: "hash-" + version, Enabled: true,
		}))
	}

	activation, err := ActivateTaskPluginWithState("cas-plugin", "2.0.0")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", activation.Previous.Version)
	require.Equal(t, "2.0.0", activation.Target.Version)

	DB = nodeB
	_, err = ActivateTaskPluginWithState("cas-plugin", "3.0.0")
	require.NoError(t, err)

	DB = nodeA
	restored, err := RestoreTaskPluginActivation(activation)
	require.NoError(t, err)
	assert.False(t, restored)
	active, err := GetTaskPluginVersion("cas-plugin", "")
	require.NoError(t, err)
	assert.Equal(t, "3.0.0", active.Version)
}

func TestRestoreTaskPluginActivationDoesNotOverwriteConcurrentDisable(t *testing.T) {
	setupTaskPluginModelTest(t)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		require.NoError(t, SaveTaskPlugin(&TaskPlugin{
			Key: "disable-wins", APIVersion: 1, Version: version,
			Source: version, SourceHash: "hash-" + version, Enabled: true,
		}))
	}

	activation, err := ActivateTaskPluginWithState("disable-wins", "2.0.0")
	require.NoError(t, err)
	require.NoError(t, SetTaskPluginEnabled("disable-wins", false))

	restored, err := RestoreTaskPluginActivation(activation)
	require.NoError(t, err)
	assert.False(t, restored)
	active, err := GetTaskPluginVersion("disable-wins", "")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", active.Version)
	assert.False(t, active.Enabled)
}

func TestRestoreTaskPluginActivationRejectsABAReactivation(t *testing.T) {
	databasePath := t.TempDir() + "/task-plugin-aba.db"
	openNode := func() *gorm.DB {
		database, err := gorm.Open(sqlite.Open(databasePath+"?_busy_timeout=5000"), &gorm.Config{})
		require.NoError(t, err)
		sqlDB, err := database.DB()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
		return database
	}
	nodeA := openNode()
	nodeB := openNode()
	require.NoError(t, nodeA.AutoMigrate(&TaskPluginState{}, &TaskPlugin{}))
	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	DB = nodeA
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		require.NoError(t, SaveTaskPlugin(&TaskPlugin{
			Key: "aba-plugin", APIVersion: 1, Version: version,
			Source: version, SourceHash: "hash-" + version, Enabled: true,
		}))
	}

	staleActivation, err := ActivateTaskPluginWithState("aba-plugin", "2.0.0")
	require.NoError(t, err)
	DB = nodeB
	require.NoError(t, ActivateTaskPlugin("aba-plugin", "3.0.0"))
	require.NoError(t, ActivateTaskPlugin("aba-plugin", "2.0.0"))

	DB = nodeA
	restored, err := RestoreTaskPluginActivation(staleActivation)
	require.NoError(t, err)
	assert.False(t, restored)
	active, err := GetTaskPluginVersion("aba-plugin", "")
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", active.Version)
	assert.True(t, active.Enabled)
}

func TestDeleteActiveTaskPluginPromotesNewestRemainingVersion(t *testing.T) {
	setupTaskPluginModelTest(t)

	plugins := []*TaskPlugin{
		{Key: "promote", APIVersion: 1, Version: "1.0.0", Source: "v1", SourceHash: "hash-v1", Enabled: true},
		{Key: "promote", APIVersion: 1, Version: "2.0.0", Source: "v2", SourceHash: "hash-v2", Enabled: false},
		{Key: "promote", APIVersion: 1, Version: "3.0.0", Source: "v3", SourceHash: "hash-v3", Enabled: true},
		{Key: "promote", APIVersion: 1, Version: "4.0.0", Source: "v4", SourceHash: "hash-v4", Enabled: true},
	}
	for _, plugin := range plugins {
		require.NoError(t, SaveTaskPlugin(plugin))
	}
	require.NoError(t, DB.Model(plugins[1]).Update("created_at", 200).Error)
	require.NoError(t, DB.Model(plugins[2]).Update("created_at", 100).Error)
	require.NoError(t, DB.Model(plugins[3]).Update("created_at", 100).Error)

	deleteResult, err := DeleteTaskPluginVersion("promote", "1.0.0")
	require.NoError(t, err)
	assert.True(t, deleteResult.DeletedActive)
	require.NotNil(t, deleteResult.Promoted)
	assert.Equal(t, "2.0.0", deleteResult.Promoted.Version)
	assert.False(t, deleteResult.Promoted.Enabled)

	deleteResult, err = DeleteTaskPluginVersion("promote", "2.0.0")
	require.NoError(t, err)
	assert.True(t, deleteResult.DeletedActive)
	require.NotNil(t, deleteResult.Promoted)
	assert.Equal(t, "4.0.0", deleteResult.Promoted.Version)

	active, err := GetTaskPluginVersion("promote", "")
	require.NoError(t, err)
	assert.Equal(t, "4.0.0", active.Version)
}

func TestDeleteTaskPluginVersionRetainsHistoricalSourceAsTombstone(t *testing.T) {
	setupTaskPluginModelTest(t)
	v1 := TaskPlugin{Key: "tombstone", APIVersion: 1, Version: "1.0.0", Source: "v1", SourceHash: "hash-v1", Enabled: true}
	v2 := TaskPlugin{Key: "tombstone", APIVersion: 1, Version: "2.0.0", Source: "v2", SourceHash: "hash-v2", Enabled: true}
	require.NoError(t, SaveTaskPlugin(&v1))
	require.NoError(t, SaveTaskPlugin(&v2))
	require.NoError(t, ActivateTaskPlugin("tombstone", "2.0.0"))

	_, err := DeleteTaskPluginVersion("tombstone", "1.0.0")
	require.NoError(t, err)
	_, err = GetTaskPluginVersion("tombstone", "1.0.0")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	versions, err := ListTaskPluginVersions("tombstone")
	require.NoError(t, err)
	require.Len(t, versions, 1)
	assert.Equal(t, "2.0.0", versions[0].Version)

	historical, err := GetTaskPluginVersionForExecution("tombstone", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "v1", historical.Source)
	assert.Equal(t, "hash-v1", historical.SourceHash)
	assert.NotNil(t, historical.DeletedAt)
	assert.True(t, historical.Enabled)
}

func TestTaskPluginSyncSnapshotRevisionTracksDesiredRuntimeState(t *testing.T) {
	setupTaskPluginModelTest(t)

	empty, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Empty(t, empty.Plugins)
	require.NotEmpty(t, empty.Revision)

	v1 := TaskPlugin{
		Key: "revision-probe", APIVersion: 1, Version: "1.0.0",
		Source: "v1", SourceHash: "hash-v1", Enabled: true,
	}
	require.NoError(t, SaveTaskPlugin(&v1))
	v1Snapshot, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.Len(t, v1Snapshot.Plugins, 1)
	assert.NotEqual(t, empty.Revision, v1Snapshot.Revision)

	v2 := TaskPlugin{
		Key: "revision-probe", APIVersion: 1, Version: "2.0.0",
		Source: "v2", SourceHash: "hash-v2", Enabled: true,
	}
	require.NoError(t, SaveTaskPlugin(&v2))
	inactiveAdded, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Equal(t, v1Snapshot.Revision, inactiveAdded.Revision)

	require.NoError(t, DB.Model(&v1).Update("remark", "operator note").Error)
	remarkChanged, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Equal(t, v1Snapshot.Revision, remarkChanged.Revision)

	require.NoError(t, ActivateTaskPlugin("revision-probe", "2.0.0"))
	v2Snapshot, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	require.Len(t, v2Snapshot.Plugins, 1)
	assert.Equal(t, "2.0.0", v2Snapshot.Plugins[0].Version)
	assert.NotEqual(t, v1Snapshot.Revision, v2Snapshot.Revision)

	require.NoError(t, SetTaskPluginEnabled("revision-probe", false))
	disabled, err := GetTaskPluginSyncSnapshot()
	require.NoError(t, err)
	assert.Empty(t, disabled.Plugins)
	assert.NotEqual(t, v2Snapshot.Revision, disabled.Revision)
}

func TestTaskPluginOrderSQLQuotesMySQLKeyColumn(t *testing.T) {
	db, err := gorm.Open(tests.DummyDialector{}, &gorm.Config{DryRun: true})
	require.NoError(t, err)

	var sqls []string
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:capture_task_plugin_sql", func(tx *gorm.DB) {
		sqls = append(sqls, tx.Statement.SQL.String())
	}))

	originalDB := DB
	t.Cleanup(func() { DB = originalDB })
	DB = db

	_, err = ListTaskPlugins()
	require.NoError(t, err)
	_, err = GetTaskPluginSyncSnapshot()
	require.NoError(t, err)

	require.Len(t, sqls, 2)
	for _, sql := range sqls {
		assert.Contains(t, sql, "`key`")
		assert.NotRegexp(t, `(?i)ORDER BY[[:space:]]+key([[:space:],]|$)`, sql)
	}
}

// taskPluginPreviousSchema mirrors the table before routing revisions and
// durable deletion tombstones were introduced.
type taskPluginPreviousSchema struct {
	Id         int64  `gorm:"primaryKey"`
	Key        string `gorm:"size:128;not null;uniqueIndex:uk_task_plugin_key_version,priority:1"`
	APIVersion int    `gorm:"not null"`
	Version    string `gorm:"size:64;not null;uniqueIndex:uk_task_plugin_key_version,priority:2"`
	Source     string `gorm:"type:text;not null"`
	SourceHash string `gorm:"size:64;not null"`
	Enabled    bool   `gorm:"not null"`
	Active     bool   `gorm:"not null;index"`
	CreatedAt  int64  `gorm:"not null"`
	Remark     string `gorm:"type:text"`
}

func TestTaskPluginActivationRestoreDatabaseMatrix(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
		version   string
	}{
		{name: "sqlite", dialector: func(string) gorm.Dialector { return sqlite.Open(":memory:") }, version: "SELECT sqlite_version()"},
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }, version: "SELECT VERSION()"},
		{name: "postgres", env: "TEST_POSTGRES_DSN", dialector: func(dsn string) gorm.Dialector {
			return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		}, version: "SHOW server_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := ""
			if test.env != "" {
				dsn = strings.TrimSpace(os.Getenv(test.env))
				if dsn == "" {
					t.Skip(test.env + " is not configured")
				}
			}
			prefix := fmt.Sprintf("tpact_%d_", time.Now().UnixNano())
			database, err := gorm.Open(test.dialector(dsn), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: prefix},
			})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			if test.name == "sqlite" {
				sqlDB.SetMaxOpenConns(1)
			} else {
				sqlDB.SetMaxOpenConns(4)
			}
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

			originalDB := DB
			DB = database
			t.Cleanup(func() { DB = originalDB })
			dropped := false
			t.Cleanup(func() {
				if !dropped {
					_ = database.Migrator().DropTable(&TaskPlugin{}, &TaskPluginState{})
				}
			})
			require.NoError(t, database.AutoMigrate(&TaskPluginState{}, &TaskPlugin{}))

			start := make(chan struct{})
			saveErrors := make(chan error, 2)
			var uploads sync.WaitGroup
			for _, version := range []string{"1.0.0", "2.0.0"} {
				version := version
				uploads.Add(1)
				go func() {
					defer uploads.Done()
					<-start
					saveErrors <- SaveTaskPlugin(&TaskPlugin{
						Key: "matrix-first-upload", APIVersion: 1, Version: version,
						Source: version, SourceHash: "first-" + version, Enabled: true,
					})
				}()
			}
			close(start)
			uploads.Wait()
			close(saveErrors)
			for saveErr := range saveErrors {
				require.NoError(t, saveErr)
			}
			var activeCount int64
			require.NoError(t, database.Model(&TaskPlugin{}).
				Where(&TaskPlugin{Key: "matrix-first-upload", Active: true}).
				Where("deleted_at IS NULL").Count(&activeCount).Error)
			assert.Equal(t, int64(1), activeCount)
			var uploadState TaskPluginState
			require.NoError(t, database.Where(&TaskPluginState{Key: "matrix-first-upload"}).First(&uploadState).Error)
			assert.Equal(t, int64(2), uploadState.Revision)

			var databaseVersion string
			require.NoError(t, database.Raw(test.version).Scan(&databaseVersion).Error)
			t.Logf("database=%s version=%s table_prefix=%s", test.name, databaseVersion, prefix)

			v1 := TaskPlugin{Key: "matrix-plugin", APIVersion: 1, Version: "1.0.0", Source: "v1", SourceHash: "hash-v1", Enabled: false}
			v2 := TaskPlugin{Key: "matrix-plugin", APIVersion: 1, Version: "2.0.0", Source: "v2", SourceHash: "hash-v2", Enabled: true}
			require.NoError(t, SaveTaskPlugin(&v1))
			require.NoError(t, SaveTaskPlugin(&v2))

			for range 2 {
				activation, activateErr := ActivateTaskPluginWithState(v2.Key, v2.Version)
				require.NoError(t, activateErr)
				_, activateErr = ActivateTaskPluginWithState(v2.Key, v2.Version)
				require.NoError(t, activateErr, "repeated activation must be idempotent")
				active, lookupErr := GetTaskPluginVersion(v2.Key, "")
				require.NoError(t, lookupErr)
				assert.Equal(t, v2.Version, active.Version)
				assert.True(t, active.Enabled)

				restored, restoreErr := RestoreTaskPluginActivation(activation)
				require.NoError(t, restoreErr)
				assert.True(t, restored)
				restored, restoreErr = RestoreTaskPluginActivation(activation)
				require.NoError(t, restoreErr, "repeated restoration must be idempotent")
				assert.False(t, restored)
				active, lookupErr = GetTaskPluginVersion(v1.Key, "")
				require.NoError(t, lookupErr)
				assert.Equal(t, v1.Version, active.Version)
				assert.False(t, active.Enabled)
			}

			var rows []TaskPlugin
			require.NoError(t, database.Where(&TaskPlugin{Key: "matrix-plugin"}).Order("version").Find(&rows).Error)
			require.Len(t, rows, 2)
			assert.True(t, rows[0].Active)
			assert.False(t, rows[0].Enabled)
			assert.False(t, rows[1].Active)
			assert.True(t, rows[1].Enabled)

			deleteResult, deleteErr := DeleteTaskPluginVersion(v2.Key, v2.Version)
			require.NoError(t, deleteErr)
			assert.False(t, deleteResult.DeletedActive)
			_, lookupErr := GetTaskPluginVersion(v2.Key, v2.Version)
			assert.ErrorIs(t, lookupErr, gorm.ErrRecordNotFound)
			historical, lookupErr := GetTaskPluginVersionForExecution(v2.Key, v2.Version)
			require.NoError(t, lookupErr)
			assert.NotNil(t, historical.DeletedAt)
			assert.Equal(t, v2.SourceHash, historical.SourceHash)

			require.NoError(t, database.Migrator().DropTable(&TaskPlugin{}, &TaskPluginState{}))
			assert.False(t, database.Migrator().HasTable(&TaskPlugin{}))

			legacyTable := prefix + "task_plugins"
			require.NoError(t, database.Table(legacyTable).AutoMigrate(&taskPluginPreviousSchema{}))
			require.NoError(t, database.Table(legacyTable).Create(&taskPluginPreviousSchema{
				Key: "legacy-plugin", APIVersion: 1, Version: "1.0.0",
				Source: "legacy", SourceHash: "legacy-hash", Enabled: true,
				Active: true, CreatedAt: time.Now().Unix(),
			}).Error)
			for range 2 {
				require.NoError(t, database.AutoMigrate(&TaskPluginState{}))
				require.NoError(t, database.Table(legacyTable).AutoMigrate(&TaskPlugin{}))
			}
			legacy, lookupErr := GetTaskPluginVersion("legacy-plugin", "")
			require.NoError(t, lookupErr)
			assert.Equal(t, int64(0), legacy.Revision)
			assert.Nil(t, legacy.DeletedAt)
			assert.Equal(t, "legacy", legacy.Source)
			require.NoError(t, SetTaskPluginEnabled("legacy-plugin", false))
			var state TaskPluginState
			require.NoError(t, database.Where(&TaskPluginState{Key: "legacy-plugin"}).First(&state).Error)
			assert.Equal(t, int64(1), state.Revision)

			require.NoError(t, database.Migrator().DropTable(&TaskPlugin{}, &TaskPluginState{}))
			dropped = true
			assert.False(t, database.Migrator().HasTable(&TaskPlugin{}))
		})
	}
}

func TestGetByTaskIdsForUserDatabaseMatrix(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
		version   string
	}{
		{name: "sqlite", dialector: func(string) gorm.Dialector { return sqlite.Open(":memory:") }, version: "SELECT sqlite_version()"},
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }, version: "SELECT VERSION()"},
		{name: "postgres", env: "TEST_POSTGRES_DSN", dialector: func(dsn string) gorm.Dialector {
			return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		}, version: "SHOW server_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := ""
			if test.env != "" {
				dsn = strings.TrimSpace(os.Getenv(test.env))
				if dsn == "" {
					t.Skip(test.env + " is not configured")
				}
			}
			prefix := fmt.Sprintf("tqown_%d_", time.Now().UnixNano())
			database, err := gorm.Open(test.dialector(dsn), &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: prefix},
			})
			require.NoError(t, err)
			sqlDB, err := database.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

			originalDB := DB
			DB = database
			t.Cleanup(func() { DB = originalDB })
			require.NoError(t, database.AutoMigrate(&Task{}))
			t.Cleanup(func() { _ = database.Migrator().DropTable(&Task{}) })

			tasks := []*Task{
				{TaskID: "owned-a", UserId: 7, Platform: "651", PrivateData: TaskPrivateData{Execution: &TaskExecutionSnapshot{TaskPlugin: &TaskPluginSnapshot{Key: "plugin-a", Version: "1.0.0"}}}},
				{TaskID: "owned-b", UserId: 7, Platform: "plugin-a"},
				{TaskID: "owned-a", UserId: 8, Platform: "plugin-a"},
			}
			for _, task := range tasks {
				require.NoError(t, database.Create(task).Error)
			}

			owned, queryErr := GetByTaskIdsForUser(7, []string{"owned-a", "owned-b"})
			require.NoError(t, queryErr)
			require.Len(t, owned, 2)
			for _, task := range owned {
				assert.Equal(t, 7, task.UserId)
			}

			var databaseVersion string
			require.NoError(t, database.Raw(test.version).Scan(&databaseVersion).Error)
			t.Logf("database=%s version=%s table_prefix=%s", test.name, databaseVersion, prefix)
		})
	}
}
