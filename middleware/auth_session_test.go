package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAdminSessionAuthTest(t *testing.T) (*gorm.DB, *gin.Engine, *model.User, string) {
	t.Helper()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMainDBType := common.MainDatabaseType()
	previousLogDBType := common.LogDatabaseType()
	previousRedisEnabled := common.RedisEnabled
	previousSessionSecret := common.SessionSecret

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.SessionSecret = "admin-session-auth-test-secret"
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.Log{}))

	user := &model.User{
		Username: "session-admin",
		Role:     common.RoleAdminUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
	}
	require.NoError(t, db.Create(user).Error)
	bundle, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "auth-session-test")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin", AdminAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainDBType, previousLogDBType)
		common.RedisEnabled = previousRedisEnabled
		common.SessionSecret = previousSessionSecret
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	return db, router, user, bundle.AccessToken
}

func performAdminSessionRequest(t *testing.T, router *gin.Engine, accessToken string) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminAuthRejectsDeletedSessionUser(t *testing.T) {
	db, router, user, accessToken := setupAdminSessionAuthTest(t)

	require.NoError(t, db.Unscoped().Delete(&model.User{}, user.Id).Error)
	recorder := performAdminSessionRequest(t, router, accessToken)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestAdminAuthRejectsReusedUserID(t *testing.T) {
	db, router, user, accessToken := setupAdminSessionAuthTest(t)

	require.NoError(t, db.Unscoped().Delete(&model.User{}, user.Id).Error)
	require.NoError(t, db.Create(&model.User{
		Id:       user.Id,
		Username: "replacement-admin",
		Password: "password",
		Role:     common.RoleAdminUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-admin",
	}).Error)
	recorder := performAdminSessionRequest(t, router, accessToken)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestAdminAuthUsesCurrentUserRole(t *testing.T) {
	db, router, user, accessToken := setupAdminSessionAuthTest(t)

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("role", common.RoleCommonUser).Error)
	recorder := performAdminSessionRequest(t, router, accessToken)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"AUTH_INSUFFICIENT_PRIVILEGE"`)
}

func TestAdminAuthAllowsExistingAdminSession(t *testing.T) {
	_, router, _, accessToken := setupAdminSessionAuthTest(t)

	recorder := performAdminSessionRequest(t, router, accessToken)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}
