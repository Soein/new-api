package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAdminSessionAuthTest(t *testing.T) (*gorm.DB, *gin.Engine, *model.User) {
	t.Helper()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMainDBType := common.MainDatabaseType()
	previousLogDBType := common.LogDatabaseType()
	previousRedisEnabled := common.RedisEnabled

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Log{}))

	user := &model.User{
		Username: "session-admin",
		Role:     common.RoleAdminUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
	}
	require.NoError(t, db.Create(user).Error)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("auth-session-test"))))
	router.GET("/login", func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("username", user.Username)
		session.Set("role", user.Role)
		session.Set("id", user.Id)
		session.Set("status", user.Status)
		session.Set("group", user.Group)
		session.Set(constant.SessionKeyUserGeneration, user.SessionGeneration)
		require.NoError(t, session.Save())
		c.Status(http.StatusNoContent)
	})
	router.GET("/admin", AdminAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMainDBType, previousLogDBType)
		common.RedisEnabled = previousRedisEnabled
		sqlDB, sqlErr := db.DB()
		if sqlErr == nil {
			_ = sqlDB.Close()
		}
	})

	return db, router, user
}

func adminSessionCookies(t *testing.T, router *gin.Engine) []*http.Cookie {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code)
	return recorder.Result().Cookies()
}

func performAdminSessionRequest(t *testing.T, router *gin.Engine, userID int, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	request.Header.Set("New-Api-User", fmt.Sprintf("%d", userID))
	for _, sessionCookie := range cookies {
		request.AddCookie(sessionCookie)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminAuthRejectsDeletedSessionUser(t *testing.T) {
	db, router, user := setupAdminSessionAuthTest(t)
	cookies := adminSessionCookies(t, router)

	require.NoError(t, db.Unscoped().Delete(&model.User{}, user.Id).Error)
	recorder := performAdminSessionRequest(t, router, user.Id, cookies)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestAdminAuthRejectsReusedUserID(t *testing.T) {
	db, router, user := setupAdminSessionAuthTest(t)
	cookies := adminSessionCookies(t, router)

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
	recorder := performAdminSessionRequest(t, router, user.Id, cookies)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestAdminAuthUsesCurrentUserRole(t *testing.T) {
	db, router, user := setupAdminSessionAuthTest(t)
	cookies := adminSessionCookies(t, router)

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("role", common.RoleCommonUser).Error)
	recorder := performAdminSessionRequest(t, router, user.Id, cookies)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"success":false`)
}

func TestAdminAuthAllowsExistingAdminSession(t *testing.T) {
	_, router, user := setupAdminSessionAuthTest(t)
	cookies := adminSessionCookies(t, router)

	recorder := performAdminSessionRequest(t, router, user.Id, cookies)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"success":true`)
}
