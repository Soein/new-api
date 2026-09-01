package model

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestUserQuotaSchemaDeclaresBigIntColumns(t *testing.T) {
	statement := &gorm.Statement{DB: DB}
	require.NoError(t, statement.Parse(&User{}))

	for _, fieldName := range []string{"Quota", "UsedQuota", "AffQuota", "AffHistoryQuota"} {
		field := statement.Schema.LookUpField(fieldName)
		require.NotNil(t, field)
		assert.Equal(t, "bigint", strings.ToLower(field.TagSettings["TYPE"]), fieldName)
	}
}

func testFreshUserQuotaSchema(t *testing.T, db *gorm.DB, recorder *migrationSQLRecorder, dialect string) {
	t.Helper()
	tableName := fmt.Sprintf("user_quota_schema_%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = db.Migrator().DropTable(tableName) })

	require.NoError(t, db.Table(tableName).AutoMigrate(&User{}))
	columnTypes, err := db.Table(tableName).Migrator().ColumnTypes(&User{})
	require.NoError(t, err)

	found := make(map[string]bool, len(userQuotaColumns))
	for _, columnType := range columnTypes {
		for _, columnName := range userQuotaColumns {
			if !strings.EqualFold(columnType.Name(), columnName) {
				continue
			}
			found[columnName] = true
			if dialect != "sqlite" {
				assert.True(t, is64BitIntegerType(databaseTypeForDialect(dialect), columnType.DatabaseTypeName()),
					"%s uses %s", columnName, columnType.DatabaseTypeName())
			}
		}
	}
	for _, columnName := range userQuotaColumns {
		assert.True(t, found[columnName], "%s was not created", columnName)
	}

	require.NoError(t, db.Table(tableName).Create(map[string]any{
		"id":          1,
		"username":    "quota-schema-user",
		"password":    "quota-schema-password",
		"quota":       common.MaxWalletQuota,
		"used_quota":  common.MaxWalletQuota,
		"aff_quota":   common.MaxWalletQuota,
		"aff_history": common.MaxWalletQuota,
	}).Error)
	var stored struct {
		Quota      int
		UsedQuota  int
		AffQuota   int
		AffHistory int
	}
	require.NoError(t, db.Table(tableName).
		Select("quota", "used_quota", "aff_quota", "aff_history").
		Where("id = ?", 1).
		Scan(&stored).Error)
	assert.Equal(t, common.MaxWalletQuota, stored.Quota)
	assert.Equal(t, common.MaxWalletQuota, stored.UsedQuota)
	assert.Equal(t, common.MaxWalletQuota, stored.AffQuota)
	assert.Equal(t, common.MaxWalletQuota, stored.AffHistory)

	recorder.reset()
	require.NoError(t, db.Table(tableName).AutoMigrate(&User{}))
	if dialect != "sqlite" {
		quotaColumnMutations := make([]string, 0)
		for _, mutation := range recorder.schemaMutations() {
			for _, columnName := range userQuotaColumns {
				if strings.Contains(mutation, "`"+columnName+"`") ||
					strings.Contains(mutation, `"`+columnName+`"`) {
					quotaColumnMutations = append(quotaColumnMutations, mutation)
					break
				}
			}
		}
		assert.Empty(t, quotaColumnMutations, "a second migration must not rewrite quota column types")
	}
	stored = struct {
		Quota      int
		UsedQuota  int
		AffQuota   int
		AffHistory int
	}{}
	require.NoError(t, db.Table(tableName).
		Select("quota", "used_quota", "aff_quota", "aff_history").
		Where("id = ?", 1).
		Scan(&stored).Error)
	assert.Equal(t, common.MaxWalletQuota, stored.Quota)
	assert.Equal(t, common.MaxWalletQuota, stored.UsedQuota)
	assert.Equal(t, common.MaxWalletQuota, stored.AffQuota)
	assert.Equal(t, common.MaxWalletQuota, stored.AffHistory)
}

func databaseTypeForDialect(dialect string) common.DatabaseType {
	switch dialect {
	case "mysql":
		return common.DatabaseTypeMySQL
	case "postgres":
		return common.DatabaseTypePostgreSQL
	default:
		return common.DatabaseTypeSQLite
	}
}

func TestFreshUserQuotaSchemaSQLite(t *testing.T) {
	recorder := &migrationSQLRecorder{}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: recorder})
	require.NoError(t, err)
	testFreshUserQuotaSchema(t, db, recorder, "sqlite")
}

func TestFreshUserQuotaSchemaConfiguredDatabases(t *testing.T) {
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
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			testFreshUserQuotaSchema(t, db, recorder, test.name)
		})
	}
}
