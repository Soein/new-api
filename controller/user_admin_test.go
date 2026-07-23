package controller

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUserAdminControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTokenControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.UserOAuthBinding{},
		&model.ExternalIdentityClaim{},
		&model.UserSession{},
		&model.AuthFlow{},
		&model.PasskeyCredential{},
		&model.TwoFA{},
		&model.TwoFABackupCode{},
		&model.Log{},
	))
	return db
}

func seedUserAdminControllerTestUser(
	t *testing.T,
	db *gorm.DB,
	username string,
	role int,
	quota int,
) *model.User {
	t.Helper()
	user := &model.User{
		Username: username,
		Password: "password",
		Role:     role,
		Status:   common.UserStatusEnabled,
		Quota:    quota,
		Group:    "default",
		AffCode:  username,
	}
	require.NoError(t, db.Create(user).Error)
	return user
}

func TestSearchUsersFiltersQuotaLessThanZero(t *testing.T) {
	db := setupUserAdminControllerTestDB(t)
	seedUserAdminControllerTestUser(t, db, "negative", common.RoleCommonUser, -1)
	seedUserAdminControllerTestUser(t, db, "zero", common.RoleCommonUser, 0)

	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodGet,
		"/api/user/search?quota_operator=lt&quota_value=0",
		nil,
		1,
	)
	SearchUsers(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)

	var page struct {
		Items []model.User `json:"items"`
		Total int          `json:"total"`
	}
	require.NoError(t, common.Unmarshal(response.Data, &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, "negative", page.Items[0].Username)
	assert.Equal(t, 1, page.Total)
}

func TestSearchUsersRejectsIncompleteQuotaFilter(t *testing.T) {
	setupUserAdminControllerTestDB(t)
	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodGet,
		"/api/user/search?quota_operator=lt",
		nil,
		1,
	)

	SearchUsers(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)
}

func TestDeleteUsersBatchDeletesAuthorizedTargets(t *testing.T) {
	db := setupUserAdminControllerTestDB(t)
	actor := seedUserAdminControllerTestUser(t, db, "root", common.RoleRootUser, 0)
	first := seedUserAdminControllerTestUser(t, db, "first", common.RoleCommonUser, -1)
	second := seedUserAdminControllerTestUser(t, db, "second", common.RoleCommonUser, -2)

	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodPost,
		"/api/user/batch",
		map[string]any{"users": []model.UserDeletionTarget{
			{Id: first.Id, IdentityGeneration: first.SessionGeneration},
			{Id: second.Id, IdentityGeneration: second.SessionGeneration},
		}},
		actor.Id,
	)
	ctx.Set("role", common.RoleRootUser)
	ctx.Set("username", actor.Username)
	common.SetContextKey(ctx, constant.ContextKeyUserSessionGeneration, actor.SessionGeneration)
	DeleteUsersBatch(ctx)

	response := decodeAPIResponse(t, recorder)
	require.True(t, response.Success, response.Message)

	var deletedCount int
	require.NoError(t, common.Unmarshal(response.Data, &deletedCount))
	assert.Equal(t, 2, deletedCount)

	var remaining int64
	require.NoError(t, db.Model(&model.User{}).Count(&remaining).Error)
	assert.EqualValues(t, 1, remaining)
}

func TestDeleteUsersBatchRejectsUnauthorizedTargetAtomically(t *testing.T) {
	db := setupUserAdminControllerTestDB(t)
	actor := seedUserAdminControllerTestUser(t, db, "admin", common.RoleAdminUser, 0)
	commonUser := seedUserAdminControllerTestUser(t, db, "common", common.RoleCommonUser, -1)
	peerAdmin := seedUserAdminControllerTestUser(t, db, "peer", common.RoleAdminUser, -1)

	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodPost,
		"/api/user/batch",
		map[string]any{"users": []model.UserDeletionTarget{
			{Id: commonUser.Id, IdentityGeneration: commonUser.SessionGeneration},
			{Id: peerAdmin.Id, IdentityGeneration: peerAdmin.SessionGeneration},
		}},
		actor.Id,
	)
	ctx.Set("role", common.RoleAdminUser)
	ctx.Set("username", actor.Username)
	common.SetContextKey(ctx, constant.ContextKeyUserSessionGeneration, actor.SessionGeneration)
	DeleteUsersBatch(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)

	var remaining int64
	require.NoError(t, db.Model(&model.User{}).Count(&remaining).Error)
	assert.EqualValues(t, 3, remaining)

	var auditLogs []model.Log
	require.NoError(t, db.Order("id asc").Find(&auditLogs).Error)
	require.Len(t, auditLogs, 2)
	type auditPayload struct {
		Op struct {
			Action string `json:"action"`
			Params struct {
				Ids    []int  `json:"ids"`
				Reason string `json:"reason"`
			} `json:"params"`
		} `json:"op"`
	}
	var requested auditPayload
	var failed auditPayload
	require.NoError(t, common.Unmarshal([]byte(auditLogs[0].Other), &requested))
	require.NoError(t, common.Unmarshal([]byte(auditLogs[1].Other), &failed))
	assert.Equal(t, "user.delete_batch_request", requested.Op.Action)
	assert.ElementsMatch(t, []int{commonUser.Id, peerAdmin.Id}, requested.Op.Params.Ids)
	assert.Equal(t, "user.delete_batch_failure", failed.Op.Action)
	assert.ElementsMatch(t, []int{commonUser.Id, peerAdmin.Id}, failed.Op.Params.Ids)
	assert.NotEmpty(t, failed.Op.Params.Reason)
}

func TestDeleteUsersBatchDoesNotDeleteWhenAuditIsUnavailable(t *testing.T) {
	db := setupUserAdminControllerTestDB(t)
	actor := seedUserAdminControllerTestUser(t, db, "audit-root", common.RoleRootUser, 0)
	target := seedUserAdminControllerTestUser(t, db, "audit-target", common.RoleCommonUser, -1)
	require.NoError(t, db.Migrator().DropTable(&model.Log{}))

	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodPost,
		"/api/user/batch",
		map[string]any{"users": []model.UserDeletionTarget{
			{Id: target.Id, IdentityGeneration: target.SessionGeneration},
		}},
		actor.Id,
	)
	ctx.Set("role", common.RoleRootUser)
	ctx.Set("username", actor.Username)
	common.SetContextKey(ctx, constant.ContextKeyUserSessionGeneration, actor.SessionGeneration)
	DeleteUsersBatch(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)
	assert.Equal(t, i18n.MsgDatabaseError, response.Message)

	var remaining int64
	require.NoError(t, db.Model(&model.User{}).Count(&remaining).Error)
	assert.EqualValues(t, 2, remaining)
}

func TestTokenAuthRejectsTokenWhoseUserWasDeleted(t *testing.T) {
	initModelListColumnNames(t)
	db := setupUserAdminControllerTestDB(t)
	user := seedUserAdminControllerTestUser(t, db, "deleted-token-user", common.RoleCommonUser, 0)
	token := &model.Token{
		UserId:         user.Id,
		Key:            "deletedusertoken",
		Name:           "deleted user token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, db.Create(token).Error)
	require.NoError(t, db.Unscoped().Delete(&model.User{}, user.Id).Error)

	router := gin.New()
	router.GET("/token", middleware.TokenAuth(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true})
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/token", nil)
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestTokenAuthReadOnlyUsesPrimaryUserStatus(t *testing.T) {
	initModelListColumnNames(t)
	testCases := []struct {
		name       string
		mutateUser func(t *testing.T, db *gorm.DB, user *model.User)
		wantStatus int
	}{
		{
			name: "deleted user",
			mutateUser: func(t *testing.T, db *gorm.DB, user *model.User) {
				require.NoError(t, db.Unscoped().Delete(&model.User{}, user.Id).Error)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "disabled user",
			mutateUser: func(t *testing.T, db *gorm.DB, user *model.User) {
				require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
			},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			db := setupUserAdminControllerTestDB(t)
			user := seedUserAdminControllerTestUser(t, db, "readonly-token-user", common.RoleCommonUser, 0)
			token := &model.Token{
				UserId:         user.Id,
				Key:            "readonlyusertoken",
				Name:           "read-only user token",
				Status:         common.TokenStatusEnabled,
				ExpiredTime:    -1,
				UnlimitedQuota: true,
			}
			require.NoError(t, db.Create(token).Error)
			testCase.mutateUser(t, db, user)

			router := gin.New()
			router.GET("/token", middleware.TokenAuthReadOnly(), func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"success": true})
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/token", nil)
			request.Header.Set("Authorization", "Bearer sk-"+token.Key)
			router.ServeHTTP(recorder, request)

			require.Equal(t, testCase.wantStatus, recorder.Code)
		})
	}
}

func TestDeleteUserRevalidatesActorInTransaction(t *testing.T) {
	db := setupUserAdminControllerTestDB(t)
	actor := seedUserAdminControllerTestUser(t, db, "stale-role-actor", common.RoleCommonUser, 0)
	target := seedUserAdminControllerTestUser(t, db, "single-delete-target", common.RoleCommonUser, -1)
	ctx, recorder := newAuthenticatedContext(
		t,
		http.MethodDelete,
		"/api/user/"+strconv.Itoa(target.Id)+"?identity_generation="+target.SessionGeneration,
		nil,
		actor.Id,
	)
	ctx.Params = gin.Params{{Key: "id", Value: strconv.Itoa(target.Id)}}
	ctx.Set("role", common.RoleRootUser)
	ctx.Set("username", actor.Username)
	common.SetContextKey(ctx, constant.ContextKeyUserSessionGeneration, actor.SessionGeneration)

	DeleteUser(ctx)

	response := decodeAPIResponse(t, recorder)
	assert.False(t, response.Success)
	var targetCount int64
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", target.Id).Count(&targetCount).Error)
	assert.EqualValues(t, 1, targetCount)
}
