package model

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func testTaskBillingUsageSchemaPersistence(t *testing.T, db *gorm.DB, recorder *migrationSQLRecorder) {
	t.Helper()
	tableName := fmt.Sprintf("task_usage_schema_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = db.Migrator().DropTable(tableName) })
	tableDB := db.Table(tableName)

	require.NoError(t, tableDB.AutoMigrate(&Task{}))
	expectedSchema := map[string]jsplugin.UsageFieldSchema{
		"seconds": {
			Type:        "number",
			Unit:        "second",
			Description: jsplugin.LocalizedText{"en": "Settled duration.", "zh": "结算时长。"},
		},
		"mode": {
			Enum:        []string{"standard", "pro"},
			Description: jsplugin.LocalizedText{"en": "Settled quality tier."},
		},
	}
	task := &Task{
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		TaskID:    fmt.Sprintf("task_usage_schema_%d", time.Now().UnixNano()),
		Platform:  "usage-schema-test",
		UserId:    1,
		Group:     "default",
		Status:    TaskStatusInProgress,
		Properties: Properties{
			OriginModelName: "usage-schema-model",
		},
		PrivateData: TaskPrivateData{
			BillingSource: "wallet",
			BillingContext: &TaskBillingContext{
				OriginModelName: "usage-schema-model",
				GroupRatio:      1,
				UsageSchema:     expectedSchema,
			},
		},
		Data: json.RawMessage(`{}`),
	}
	require.NoError(t, tableDB.Create(task).Error)

	var firstRead Task
	require.NoError(t, tableDB.Where("task_id = ?", task.TaskID).First(&firstRead).Error)
	require.NotNil(t, firstRead.PrivateData.BillingContext)
	assert.Equal(t, expectedSchema, firstRead.PrivateData.BillingContext.UsageSchema)

	recorder.reset()
	require.NoError(t, tableDB.AutoMigrate(&Task{}))
	assert.Empty(t, recorder.schemaMutations(), "a second migration must not mutate the temporary tasks schema")

	var secondRead Task
	require.NoError(t, tableDB.Where("task_id = ?", task.TaskID).First(&secondRead).Error)
	require.NotNil(t, secondRead.PrivateData.BillingContext)
	assert.Equal(t, expectedSchema, secondRead.PrivateData.BillingContext.UsageSchema)
}

func TestTaskBillingUsageSchemaPersistenceSQLite(t *testing.T) {
	recorder := &migrationSQLRecorder{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: recorder})
	require.NoError(t, err)
	var version string
	require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&version).Error)
	t.Logf("sqlite version: %s", version)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	testTaskBillingUsageSchemaPersistence(t, db, recorder)
}

func TestTaskBillingUsageSchemaPersistenceConfiguredDatabases(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		dialector func(string) gorm.Dialector
	}{
		{name: "mysql", env: "TEST_MYSQL_DSN", dialector: func(dsn string) gorm.Dialector { return mysql.Open(dsn) }},
		{name: "postgres", env: "TEST_POSTGRES_DSN", dialector: func(dsn string) gorm.Dialector {
			return postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skip(test.env + " is not configured")
			}
			recorder := &migrationSQLRecorder{}
			db, err := gorm.Open(test.dialector(dsn), &gorm.Config{Logger: recorder})
			require.NoError(t, err)
			versionQuery := "SELECT VERSION()"
			if test.name == "postgres" {
				versionQuery = "SHOW server_version"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("%s version: %s", test.name, version)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			testTaskBillingUsageSchemaPersistence(t, db, recorder)
		})
	}
}
