package model

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestWalletReservationUsesConfiguredTables(t *testing.T) {
	previousDB, previousRedis, previousDebt := DB, common.RedisEnabled, common.UserQuotaMaxDebtUsd
	t.Cleanup(func() { DB, common.RedisEnabled, common.UserQuotaMaxDebtUsd = previousDB, previousRedis, previousDebt })
	common.RedisEnabled, common.UserQuotaMaxDebtUsd = false, 0
	for _, prefix := range []string{"", "wallet-review_"} {
		t.Run(fmt.Sprintf("prefix=%s", prefix), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix, SingularTable: prefix != ""}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&User{}, &UserQuotaDebt{}, &Midjourney{}, &WalletQuotaReservation{}))
			DB = db
			user := createReserveTestUser(t, 120)
			requestID := common.GetUUID()
			reserved, err := TryReserveWalletQuota(user.Id, 30, requestID, time.Now().Add(time.Minute).UnixMilli())
			require.NoError(t, err)
			require.True(t, reserved)
			confirmed, err := GetWalletSettledQuota(user.Id)
			require.NoError(t, err)
			assert.Equal(t, 120, confirmed)
			change, err := SettleWalletQuotaReservation(user.Id, 30, 25, requestID, true)
			require.NoError(t, err)
			assert.Equal(t, common.WalletQuotaChange{Before: 120, After: 95}, change)
			require.NoError(t, DecreaseUserQuotaForSettlement(user.Id, 100))
			require.NoError(t, DecreaseUserQuotaForSettlement(user.Id, 20))
			debt, err := GetUserQuotaDebt(user.Id)
			require.NoError(t, err)
			assert.EqualValues(t, 25, debt)
			require.NoError(t, SetUserQuota(user.Id, 120))
			task := Midjourney{UserId: user.Id, Quota: 30, BillingStatus: MidjourneyBillingStatusPrepared}
			require.NoError(t, db.Create(&task).Error)
			applied, err := task.SettleBilling("")
			require.NoError(t, err)
			require.True(t, applied.Applied)
			changePtr, err := task.ConfirmBilling(true)
			require.NoError(t, err)
			require.NotNil(t, changePtr)
			assert.Equal(t, common.WalletQuotaChange{Before: 120, After: 90}, *changePtr)
			refunded, err := task.RefundBilling("")
			require.NoError(t, err)
			require.True(t, refunded.Completed)
			confirmed, err = GetWalletSettledQuota(user.Id)
			require.NoError(t, err)
			assert.Equal(t, 120, confirmed)
		})
	}
}

func TestWalletReservationRejectsSoftDeletedUsers(t *testing.T) {
	for _, mode := range []string{"database", "cached", "cache unavailable"} {
		t.Run(mode, func(t *testing.T) {
			truncateTables(t)
			previousRedis := common.RedisEnabled
			t.Cleanup(func() { common.RedisEnabled = previousRedis })
			common.RedisEnabled = false
			user := createReserveTestUser(t, 120)
			if mode != "database" {
				server := useUserCacheMiniRedis(t)
				require.NoError(t, populateUserCache(user))
				if mode == "cache unavailable" {
					server.Close()
				}
			}
			require.NoError(t, DB.Delete(&user).Error)
			reserved, err := TryReserveWalletQuota(user.Id, 30, common.GetUUID(), time.Now().Add(time.Minute).UnixMilli())
			require.NoError(t, err)
			assert.False(t, reserved)
			if mode == "cached" {
				cached, err := common.RDB.Exists(t.Context(), getUserCacheKey(user.Id)).Result()
				require.NoError(t, err)
				assert.Zero(t, cached, "a rejected database debit must invalidate stale quota admission data")
			}
			_, err = DecreaseUserQuotaWithBalance(user.Id, 30)
			require.ErrorIs(t, err, ErrUserQuotaInsufficient)
			task := Midjourney{UserId: user.Id, Quota: 30, BillingStatus: MidjourneyBillingStatusPrepared}
			require.NoError(t, DB.Create(&task).Error)
			applied, err := task.SettleBilling("")
			require.Error(t, err)
			assert.False(t, applied.Applied)
			var deleted User
			require.NoError(t, DB.Unscoped().First(&deleted, user.Id).Error)
			assert.Equal(t, 120, deleted.Quota)
			assert.Zero(t, deleted.QuotaReserved)
			_, err = GetWalletSettledQuota(user.Id)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)
		})
	}
}

func createReserveTestUser(t *testing.T, quota int) User {
	t.Helper()
	user := User{
		Username:    "reserve-user-" + common.GetRandomString(6),
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		Quota:       quota,
		AffCode:     "reserve-aff-" + common.GetRandomString(8),
	}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

func createReserveTestToken(t *testing.T, remainQuota int) Token {
	t.Helper()
	user := createReserveTestUser(t, 0)
	token := Token{
		UserId:      user.Id,
		Key:         "reserve-token-" + common.GetRandomString(8),
		Name:        "reserve-test",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: remainQuota,
	}
	require.NoError(t, token.Insert(user.SessionGeneration))
	return token
}

func getUserQuotaFromDB(t *testing.T, id int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").First(&user, id).Error)
	return user.Quota
}

func getTokenFromDB(t *testing.T, id int) Token {
	t.Helper()
	var token Token
	require.NoError(t, DB.First(&token, id).Error)
	return token
}

func resetBatchUpdateTestState(t *testing.T) {
	t.Helper()
	oldBatchEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	for i := range BatchUpdateTypeCount {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
	t.Cleanup(func() {
		common.BatchUpdateEnabled = oldBatchEnabled
		for i := range BatchUpdateTypeCount {
			batchUpdateLocks[i].Lock()
			batchUpdateStores[i] = make(map[int]int)
			batchUpdateLocks[i].Unlock()
		}
	})
}

func TestTryReserveQuotaWithoutRedis(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)

	user := createReserveTestUser(t, 100)
	reserved, err := TryReserveUserQuota(user.Id, 60)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 40, getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 41)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 40, getUserQuotaFromDB(t, user.Id))

	token := createReserveTestToken(t, 80)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 25, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	reloaded := getTokenFromDB(t, token.Id)
	assert.Equal(t, 55, reloaded.RemainQuota)
	assert.Equal(t, 25, reloaded.UsedQuota)

	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 56, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 55, getTokenFromDB(t, token.Id).RemainQuota)
}

func TestRedisReserveKeepsDatabaseBalanceDurableInBatchMode(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, 10)
	reserved, err := TryReserveUserQuota(user.Id, 8)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 2, getUserQuotaFromDB(t, user.Id), "spendable balance must be durable before returning")

	reserved, err = TryReserveUserQuota(user.Id, 3)
	require.NoError(t, err)
	assert.False(t, reserved, "stale DB balance must not authorize a second spend")
	cachedUser, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 2, cachedUser.Quota)

	token := createReserveTestToken(t, 9)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 3, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 2, getTokenFromDB(t, token.Id).RemainQuota)

	batchUpdate()
	assert.Equal(t, 2, getUserQuotaFromDB(t, user.Id))
	reloadedToken := getTokenFromDB(t, token.Id)
	assert.Equal(t, 2, reloadedToken.RemainQuota)
	assert.Equal(t, 7, reloadedToken.UsedQuota)
}

func TestTokenQuotaAdjustmentsStayDurableInBatchMode(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	token := createReserveTestToken(t, 100)
	_, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, IncreaseTokenQuota(token.Id, token.Key, 20))
	require.NoError(t, DecreaseTokenQuota(token.Id, token.Key, 35))

	reloaded := getTokenFromDB(t, token.Id)
	assert.Equal(t, 85, reloaded.RemainQuota)
	assert.Equal(t, 15, reloaded.UsedQuota)
	_, err = cacheGetTokenByKey(token.Key)
	assert.Error(t, err, "DB-first adjustment must invalidate the old cache snapshot")

	reserved, err := TryReserveTokenQuota(token.Id, token.Key, 80, false)

	require.NoError(t, err)
	assert.True(t, reserved, "a quota adjustment must not fence the token from immediate reuse")
	reloaded = getTokenFromDB(t, token.Id)
	assert.Equal(t, 5, reloaded.RemainQuota)
	assert.Equal(t, 95, reloaded.UsedQuota)
}

func TestTokenReserveRejectsStaleHighCacheAtDatabaseBoundary(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	token := createReserveTestToken(t, 100)
	_, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, common.RDB.HSet(t.Context(), getTokenCacheKey(token.Key), "RemainQuota", 200).Err())

	reserved, err := TryReserveTokenQuota(token.Id, token.Key, 150, false)

	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 100, getTokenFromDB(t, token.Id).RemainQuota)
	_, err = cacheGetTokenByKey(token.Key)
	assert.Error(t, err, "a cache that disagrees with the durable balance must be invalidated")
}

func TestReserveFailsClosedDuringBillingCacheMutation(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 100)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, common.RDB.Set(t.Context(), getUserQuotaMutationFenceKey(user.Id), "billing-owner", 0).Err())

	reserved, err := TryReserveUserQuota(user.Id, 10)

	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrQuotaCacheMutationPending)
	assert.Equal(t, 100, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 100, mustGetCachedUserQuota(t, user.Id))

	token := createReserveTestToken(t, 100)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, common.RDB.Set(t.Context(), getTokenQuotaMutationFenceKey(token.Key), "billing-owner", 0).Err())

	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 10, false)

	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrQuotaCacheMutationPending)
	assert.Equal(t, 100, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, 100, mustGetCachedTokenQuota(t, token.Key))
}

func TestBatchUpdateAccumulatesTwoMaximumUsedQuotaDeltas(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, 100)
	UpdateUserUsedQuota(user.Id, common.MaxQuota)
	UpdateUserUsedQuota(user.Id, common.MaxQuota)

	var usedQuota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("used_quota").Scan(&usedQuota).Error)
	assert.Zero(t, usedQuota, "batch deltas must remain pending before flush")

	batchUpdate()
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("used_quota").Scan(&usedQuota).Error)
	assert.Equal(t, common.MaxQuota*2, usedQuota)
}

func TestBatchUpdateAccumulatorSaturatesOverflow(t *testing.T) {
	resetBatchUpdateTestState(t)

	addNewRecord(BatchUpdateTypeUserQuota, 1, math.MaxInt)
	addNewRecord(BatchUpdateTypeUserQuota, 1, 1)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	assert.Equal(t, math.MaxInt, batchUpdateStores[BatchUpdateTypeUserQuota][1])
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()

	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	batchUpdateStores[BatchUpdateTypeUserQuota] = make(map[int]int)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	addNewRecord(BatchUpdateTypeUserQuota, 1, math.MinInt)
	addNewRecord(BatchUpdateTypeUserQuota, 1, -1)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	assert.Equal(t, math.MinInt, batchUpdateStores[BatchUpdateTypeUserQuota][1])
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
}

func TestReserveFallsBackToDatabaseWhenRedisIsUnavailable(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 20)
	require.NoError(t, populateUserCache(user))
	server.Close()

	// Redis 故障时降级为数据库条件更新：服务保持可用且不会超扣。
	reserved, err := TryReserveUserQuota(user.Id, 5)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 15, getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 16)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 15, getUserQuotaFromDB(t, user.Id))
}

func TestSynchronousReserveCompensatesCacheWhenPersistenceFails(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, DB.Delete(&user).Error)

	reserved, err := TryReserveUserQuota(user.Id, 6)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	cached, cacheErr := cacheGetUserBase(user.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, 10, cached.Quota)

	token := createReserveTestToken(t, 12)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, DB.Delete(&token).Error)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	cachedToken, cacheErr := cacheGetTokenByKey(token.Key)
	require.NoError(t, cacheErr)
	assert.Equal(t, 12, cachedToken.RemainQuota)
	assert.Zero(t, cachedToken.UsedQuota)
}

func TestTokenCacheInitPreservesLiveQuotaAndFenceBlocksStaleSnapshot(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	token := createReserveTestToken(t, 100)
	loaded, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	stale := *loaded

	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	// 已存在的哈希只刷新 TTL：数据库快照不得覆盖已被原子预扣的余额。
	code, err := cacheInitToken(stale)
	require.NoError(t, err)
	assert.Equal(t, 2, code)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 30, cached.RemainQuota)

	// 变更期间：fence 删除缓存并拦截并发读者手中的过期快照。
	require.NoError(t, invalidateTokenCacheForMutation(token.Key))
	code, err = cacheInitToken(stale)
	require.NoError(t, err)
	assert.Zero(t, code, "the pre-mutation snapshot must not be published while fenced")
	_, err = cacheGetTokenByKey(token.Key)
	assert.Error(t, err)

	// fence 过期后可重新从数据库水合。
	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	fresh, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 100, fresh.RemainQuota)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 100, cached.RemainQuota)
}
