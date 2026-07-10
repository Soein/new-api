package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedAdminTestUser(t *testing.T, username string, role int, quota int) *User {
	t.Helper()
	user := &User{
		Username: username,
		Password: "password",
		Role:     role,
		Status:   common.UserStatusEnabled,
		Quota:    quota,
		Group:    "default",
		AffCode:  username,
	}
	require.NoError(t, DB.Create(user).Error)
	return user
}

func deletionTarget(user *User) UserDeletionTarget {
	return UserDeletionTarget{Id: user.Id, IdentityGeneration: user.SessionGeneration}
}

func TestSearchUsersFiltersByQuotaComparison(t *testing.T) {
	truncateTables(t)
	seedAdminTestUser(t, "negative", common.RoleCommonUser, -1)
	seedAdminTestUser(t, "zero", common.RoleCommonUser, 0)
	seedAdminTestUser(t, "positive", common.RoleCommonUser, 1)

	quotaValue := 0
	tests := []struct {
		name     string
		operator string
		want     []string
	}{
		{name: "less than", operator: "lt", want: []string{"negative"}},
		{name: "less than or equal", operator: "lte", want: []string{"zero", "negative"}},
		{name: "equal", operator: "eq", want: []string{"zero"}},
		{name: "greater than or equal", operator: "gte", want: []string{"positive", "zero"}},
		{name: "greater than", operator: "gt", want: []string{"positive"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users, total, err := SearchUsers("", "", nil, nil, tt.operator, &quotaValue, 0, 20)
			require.NoError(t, err)
			assert.EqualValues(t, len(tt.want), total)

			usernames := make([]string, 0, len(users))
			for _, user := range users {
				usernames = append(usernames, user.Username)
			}
			assert.Equal(t, tt.want, usernames)
		})
	}
}

func TestHardDeleteUsersByIdsDeletesTargetsAndBindings(t *testing.T) {
	truncateTables(t)
	actor := seedAdminTestUser(t, "root", common.RoleRootUser, 0)
	first := seedAdminTestUser(t, "first", common.RoleCommonUser, -1)
	second := seedAdminTestUser(t, "second", common.RoleCommonUser, -2)
	untouched := seedAdminTestUser(t, "untouched", common.RoleCommonUser, 10)
	firstToken := &Token{
		UserId:         first.Id,
		Key:            "deleted-first-user-token",
		Name:           "deleted first user token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, DB.Create(firstToken).Error)
	require.NoError(t, DB.Create(&UserOAuthBinding{
		UserId:         first.Id,
		ProviderId:     1,
		ProviderUserId: "external",
	}).Error)
	require.NoError(t, DB.Create(&PasskeyCredential{
		UserID:       first.Id,
		CredentialID: "deleted-user-credential",
		PublicKey:    "deleted-user-public-key",
	}).Error)
	require.NoError(t, DB.Create(&TwoFA{
		UserId:    first.Id,
		Secret:    "deleted-user-secret",
		IsEnabled: true,
	}).Error)
	require.NoError(t, DB.Create(&TwoFABackupCode{
		UserId:   first.Id,
		CodeHash: "deleted-user-backup-code",
	}).Error)

	deleted, err := HardDeleteUsersByIds(
		[]UserDeletionTarget{deletionTarget(first), deletionTarget(second)},
		actor.Id,
		actor.SessionGeneration,
	)
	require.NoError(t, err)
	require.Len(t, deleted, 2)

	var remainingUsers int64
	require.NoError(t, DB.Model(&User{}).Count(&remainingUsers).Error)
	assert.EqualValues(t, 2, remainingUsers)

	var remainingBindings int64
	require.NoError(t, DB.Model(&UserOAuthBinding{}).Count(&remainingBindings).Error)
	assert.Zero(t, remainingBindings)

	var remainingTokens int64
	require.NoError(t, DB.Model(&Token{}).Where("user_id IN ?", []int{first.Id, second.Id}).Count(&remainingTokens).Error)
	assert.Zero(t, remainingTokens)

	var remainingPasskeys int64
	require.NoError(t, DB.Unscoped().Model(&PasskeyCredential{}).Where("user_id = ?", first.Id).Count(&remainingPasskeys).Error)
	assert.Zero(t, remainingPasskeys)

	var remainingTwoFA int64
	require.NoError(t, DB.Unscoped().Model(&TwoFA{}).Where("user_id = ?", first.Id).Count(&remainingTwoFA).Error)
	assert.Zero(t, remainingTwoFA)

	var remainingBackupCodes int64
	require.NoError(t, DB.Unscoped().Model(&TwoFABackupCode{}).Where("user_id = ?", first.Id).Count(&remainingBackupCodes).Error)
	assert.Zero(t, remainingBackupCodes)

	reusedIdentity := &User{
		Id:       first.Id,
		Username: "replacement-user",
		Password: "password",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-user",
	}
	require.NoError(t, DB.Create(reusedIdentity).Error)
	validatedToken, validateErr := ValidateUserToken(firstToken.Key)
	require.ErrorIs(t, validateErr, ErrTokenInvalid)
	assert.Nil(t, validatedToken)

	var remaining User
	require.NoError(t, DB.First(&remaining, untouched.Id).Error)
	assert.Equal(t, untouched.Username, remaining.Username)
}

func TestTokenInsertRejectsDeletedOwner(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "deleted-before-token-create", common.RoleCommonUser, 0)
	require.NoError(t, DB.Unscoped().Delete(&User{}, user.Id).Error)
	replacement := &User{
		Id:       user.Id,
		Username: "replacement-token-owner",
		Password: "password",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-token-owner",
	}
	require.NoError(t, DB.Create(replacement).Error)
	token := &Token{
		UserId:         user.Id,
		Key:            "orphan-token",
		Name:           "orphan token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}

	err := token.Insert(user.SessionGeneration)

	require.Error(t, err)
	var tokenCount int64
	require.NoError(t, DB.Model(&Token{}).Where("user_id = ?", user.Id).Count(&tokenCount).Error)
	assert.Zero(t, tokenCount)
}

func TestPasskeyUpsertRejectsDeletedOwner(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "deleted-before-passkey-create", common.RoleCommonUser, 0)
	require.NoError(t, DB.Unscoped().Delete(&User{}, user.Id).Error)
	replacement := &User{
		Id:       user.Id,
		Username: "replacement-passkey-owner",
		Password: "password",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-passkey-owner",
	}
	require.NoError(t, DB.Create(replacement).Error)
	credential := &PasskeyCredential{
		UserID:       user.Id,
		CredentialID: "orphan-passkey-credential",
		PublicKey:    "orphan-passkey-public-key",
	}

	err := UpsertPasskeyCredential(credential, user.SessionGeneration)

	require.Error(t, err)
	var credentialCount int64
	require.NoError(t, DB.Unscoped().Model(&PasskeyCredential{}).
		Where("user_id = ?", user.Id).
		Count(&credentialCount).Error)
	assert.Zero(t, credentialCount)
}

func TestAccessTokenUpdateRejectsReusedUserID(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "deleted-before-access-token", common.RoleCommonUser, 0)
	require.NoError(t, DB.Unscoped().Delete(&User{}, user.Id).Error)
	replacement := &User{
		Id:       user.Id,
		Username: "replacement-access-token-owner",
		Password: "password",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-access-token-owner",
	}
	require.NoError(t, DB.Create(replacement).Error)

	err := UpdateUserAccessToken(user.Id, user.SessionGeneration, "stale-access-token")

	require.ErrorIs(t, err, ErrUserNotFound)
	var stored User
	require.NoError(t, DB.First(&stored, replacement.Id).Error)
	assert.Empty(t, stored.GetAccessToken())
}

func TestEnsureUserSessionGenerationUpgradesLegacyUser(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "legacy-session-user", common.RoleCommonUser, 0)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).UpdateColumn("session_generation", "").Error)
	user.SessionGeneration = ""

	err := EnsureUserSessionGeneration(user)

	require.NoError(t, err)
	require.NotEmpty(t, user.SessionGeneration)
	var storedGeneration string
	require.NoError(t, DB.Model(&User{}).
		Where("id = ?", user.Id).
		Pluck("session_generation", &storedGeneration).Error)
	assert.Equal(t, user.SessionGeneration, storedGeneration)
}

func TestHardDeleteUsersByIdsRejectsUnauthorizedBatchAtomically(t *testing.T) {
	truncateTables(t)
	actor := seedAdminTestUser(t, "actor", common.RoleAdminUser, 0)
	commonUser := seedAdminTestUser(t, "common", common.RoleCommonUser, -1)
	adminUser := seedAdminTestUser(t, "admin", common.RoleAdminUser, -1)

	deleted, err := HardDeleteUsersByIds(
		[]UserDeletionTarget{deletionTarget(commonUser), deletionTarget(adminUser)},
		actor.Id,
		actor.SessionGeneration,
	)
	require.ErrorIs(t, err, ErrUserNoPermission)
	assert.Nil(t, deleted)

	var remainingUsers int64
	require.NoError(t, DB.Model(&User{}).Count(&remainingUsers).Error)
	assert.EqualValues(t, 3, remainingUsers)
}

func TestHardDeleteUsersByIdsRejectsMissingTargetAtomically(t *testing.T) {
	truncateTables(t)
	actor := seedAdminTestUser(t, "missing-root", common.RoleRootUser, 0)
	user := seedAdminTestUser(t, "existing", common.RoleCommonUser, -1)

	deleted, err := HardDeleteUsersByIds(
		[]UserDeletionTarget{
			deletionTarget(user),
			{Id: user.Id + 999, IdentityGeneration: common.GetUUID()},
		},
		actor.Id,
		actor.SessionGeneration,
	)
	require.ErrorIs(t, err, ErrUserNotFound)
	assert.Nil(t, deleted)

	var remainingUsers int64
	require.NoError(t, DB.Model(&User{}).Count(&remainingUsers).Error)
	assert.EqualValues(t, 2, remainingUsers)
}

func TestHardDeleteUsersByIdsRevalidatesActorInTransaction(t *testing.T) {
	tests := []struct {
		name        string
		actorRole   int
		actorStatus int
		deleteActor bool
	}{
		{
			name:        "demoted actor",
			actorRole:   common.RoleCommonUser,
			actorStatus: common.UserStatusEnabled,
		},
		{
			name:        "disabled actor",
			actorRole:   common.RoleRootUser,
			actorStatus: common.UserStatusDisabled,
		},
		{
			name:        "deleted actor",
			actorRole:   common.RoleRootUser,
			actorStatus: common.UserStatusEnabled,
			deleteActor: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			truncateTables(t)
			actor := seedAdminTestUser(t, "transaction-actor", tt.actorRole, 0)
			target := seedAdminTestUser(t, "transaction-target", common.RoleCommonUser, -1)
			require.NoError(t, DB.Model(&User{}).Where("id = ?", actor.Id).Update("status", tt.actorStatus).Error)
			if tt.deleteActor {
				require.NoError(t, DB.Unscoped().Delete(&User{}, actor.Id).Error)
			}

			deleted, err := HardDeleteUsersByIds(
				[]UserDeletionTarget{deletionTarget(target)},
				actor.Id,
				actor.SessionGeneration,
			)

			require.ErrorIs(t, err, ErrUserNoPermission)
			assert.Nil(t, deleted)
			var targetCount int64
			require.NoError(t, DB.Model(&User{}).Where("id = ?", target.Id).Count(&targetCount).Error)
			assert.EqualValues(t, 1, targetCount)
		})
	}
}

func TestHardDeleteUsersByIdsRejectsReusedActorID(t *testing.T) {
	truncateTables(t)
	actor := seedAdminTestUser(t, "original-delete-actor", common.RoleRootUser, 0)
	target := seedAdminTestUser(t, "actor-reuse-target", common.RoleCommonUser, -1)
	originalActorGeneration := actor.SessionGeneration
	require.NoError(t, DB.Unscoped().Delete(&User{}, actor.Id).Error)
	replacement := &User{
		Id:       actor.Id,
		Username: "replacement-delete-actor",
		Password: "password",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-delete-actor",
	}
	require.NoError(t, DB.Create(replacement).Error)

	deleted, err := HardDeleteUsersByIds(
		[]UserDeletionTarget{deletionTarget(target)},
		actor.Id,
		originalActorGeneration,
	)

	require.ErrorIs(t, err, ErrUserNoPermission)
	assert.Nil(t, deleted)
	var targetCount int64
	require.NoError(t, DB.Model(&User{}).Where("id = ?", target.Id).Count(&targetCount).Error)
	assert.EqualValues(t, 1, targetCount)
}

func TestHardDeleteUsersByIdsRejectsReusedTargetID(t *testing.T) {
	truncateTables(t)
	actor := seedAdminTestUser(t, "target-reuse-actor", common.RoleRootUser, 0)
	target := seedAdminTestUser(t, "original-delete-target", common.RoleCommonUser, -1)
	originalTarget := deletionTarget(target)
	require.NoError(t, DB.Unscoped().Delete(&User{}, target.Id).Error)
	replacement := &User{
		Id:       target.Id,
		Username: "replacement-delete-target",
		Password: "password",
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
		AffCode:  "replacement-delete-target",
	}
	require.NoError(t, DB.Create(replacement).Error)

	deleted, err := HardDeleteUsersByIds(
		[]UserDeletionTarget{originalTarget},
		actor.Id,
		actor.SessionGeneration,
	)

	require.ErrorIs(t, err, ErrUserNotFound)
	assert.Nil(t, deleted)
	var replacementCount int64
	require.NoError(t, DB.Model(&User{}).Where("id = ?", replacement.Id).Count(&replacementCount).Error)
	assert.EqualValues(t, 1, replacementCount)
}

func TestValidateUserTokenRejectsDeletedUser(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "deleted-token-owner", common.RoleCommonUser, 0)
	token := &Token{
		UserId:         user.Id,
		Key:            "deletedtokenowner",
		Name:           "deleted owner token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, DB.Create(token).Error)
	require.NoError(t, DB.Unscoped().Delete(&User{}, user.Id).Error)

	validatedToken, err := ValidateUserToken(token.Key)

	require.ErrorIs(t, err, ErrTokenInvalid)
	assert.Nil(t, validatedToken)
}

func TestValidateUserTokenRejectsDisabledUser(t *testing.T) {
	truncateTables(t)
	user := seedAdminTestUser(t, "disabled-token-owner", common.RoleCommonUser, 0)
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
	token := &Token{
		UserId:         user.Id,
		Key:            "disabledtokenowner",
		Name:           "disabled owner token",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, DB.Create(token).Error)

	validatedToken, err := ValidateUserToken(token.Key)

	require.ErrorIs(t, err, ErrUserDisabled)
	require.NotNil(t, validatedToken)
	assert.Equal(t, token.Id, validatedToken.Id)
}
