package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMidjourneySettlementHonorsDurableBalanceInBatchMode(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, 10000)
	token := Token{
		UserId:      user.Id,
		Key:         "midjourney-batch-" + common.GetRandomString(8),
		Name:        "midjourney-batch",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 10000,
	}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6101, Name: "midjourney-batch", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, populateUserCache(user))
	_, err := cacheInitToken(token)
	require.NoError(t, err)

	reserved, err := TryReserveUserQuota(user.Id, 8000)
	require.NoError(t, err)
	require.True(t, reserved)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 8000, false)
	require.NoError(t, err)
	require.True(t, reserved)
	task := Midjourney{
		UserId:           user.Id,
		MjId:             "mj-batch-insufficient",
		ChannelId:        channel.Id,
		Quota:            3000,
		TokenId:          token.Id,
		BillingChannelId: channel.Id,
		BillingStatus:    MidjourneyBillingStatusPrepared,
	}
	require.NoError(t, task.Insert())

	settled, err := task.SettleBilling(token.Key)

	require.Error(t, err)
	assert.False(t, settled.Applied)
	assert.Equal(t, 2000, getUserQuotaFromDB(t, user.Id), "spendable balance must be durable even in batch mode")
	assert.Equal(t, 2000, getTokenFromDB(t, token.Id).RemainQuota, "token balance must be durable even in batch mode")
	persisted := Midjourney{}
	require.NoError(t, DB.First(&persisted, task.Id).Error)
	assert.Equal(t, MidjourneyBillingStatusPrepared, persisted.BillingStatus)
	assert.Equal(t, 2000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, 2000, mustGetCachedTokenQuota(t, token.Key))
}

func TestMidjourneySettlementUsesDurableBalanceAfterCacheLoss(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true
	user := createReserveTestUser(t, 10000)
	token := Token{UserId: user.Id, Key: "midjourney-batch-cache-loss-" + common.GetRandomString(8), Name: "midjourney-batch-cache-loss", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 10000}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6107, Name: "midjourney-batch-cache-loss", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, populateUserCache(user))
	_, err := cacheInitToken(token)
	require.NoError(t, err)
	reserved, err := TryReserveUserQuota(user.Id, 8000)
	require.NoError(t, err)
	require.True(t, reserved)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 8000, false)
	require.NoError(t, err)
	require.True(t, reserved)
	task := Midjourney{UserId: user.Id, MjId: "mj-batch-cache-loss", ChannelId: channel.Id, Quota: 3000, TokenId: token.Id, BillingChannelId: channel.Id, BillingStatus: MidjourneyBillingStatusPrepared}
	require.NoError(t, task.Insert())
	require.NoError(t, common.RDB.FlushAll(t.Context()).Err())

	settled, err := task.SettleBilling(token.Key)

	assert.False(t, settled.Applied)
	assert.ErrorIs(t, err, ErrUserQuotaInsufficient)
	assert.Equal(t, 2000, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 2000, getTokenFromDB(t, token.Id).RemainQuota)
	persisted := Midjourney{}
	require.NoError(t, DB.First(&persisted, task.Id).Error)
	assert.Equal(t, MidjourneyBillingStatusPrepared, persisted.BillingStatus)
	assert.Equal(t, 2000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, 2000, mustGetCachedTokenQuota(t, token.Key))
}

func TestMidjourneyRefundRetriesCacheReconciliationWithoutDoubleCredit(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10000)
	token := Token{
		UserId:      user.Id,
		Key:         "midjourney-refund-retry-" + common.GetRandomString(8),
		Name:        "midjourney-refund-retry",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 5000,
	}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6102, Name: "midjourney-refund-retry", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, populateUserCache(user))
	_, err := cacheInitToken(token)
	require.NoError(t, err)
	task := Midjourney{
		UserId:           user.Id,
		MjId:             "mj-refund-retry",
		ChannelId:        channel.Id,
		Quota:            3000,
		TokenId:          token.Id,
		BillingChannelId: channel.Id,
		BillingStatus:    MidjourneyBillingStatusPrepared,
	}
	require.NoError(t, task.Insert())
	settled, err := task.SettleBilling(token.Key)
	require.NoError(t, err)
	require.True(t, settled.Applied)

	require.NoError(t, common.RDB.Close())
	refunded, err := task.RefundBilling(token.Key)
	require.Error(t, err)
	assert.False(t, refunded.Applied)
	assert.Equal(t, 7000, getUserQuotaFromDB(t, user.Id), "refund must remain retryable until cache coordination is available")
	assert.Equal(t, 2000, getTokenFromDB(t, token.Id).RemainQuota)
	persisted := Midjourney{}
	require.NoError(t, DB.First(&persisted, task.Id).Error)
	assert.Equal(t, 3000, persisted.Quota, "pending refund must retain its retry amount")

	common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr()})
	refunded, err = task.RefundBilling(token.Key)
	require.NoError(t, err)
	require.True(t, refunded.Applied)
	assert.Equal(t, 10000, getUserQuotaFromDB(t, user.Id), "retry must not credit the database twice")
	assert.Equal(t, 5000, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, 10000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, 5000, mustGetCachedTokenQuota(t, token.Key))
	persisted = Midjourney{}
	require.NoError(t, DB.First(&persisted, task.Id).Error)
	assert.Zero(t, persisted.Quota)
	assert.Equal(t, MidjourneyBillingStatusRefunded, persisted.BillingStatus)
}

func TestMidjourneyRefundCacheOnlyCreditsSpendableQuotaAfterDebt(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 0)
	require.NoError(t, DB.Create(&UserQuotaDebt{UserId: user.Id, Amount: 200}).Error)
	token := Token{
		UserId:      user.Id,
		Key:         "midjourney-refund-debt-" + common.GetRandomString(8),
		Name:        "midjourney-refund-debt",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: 0,
		UsedQuota:   300,
	}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6103, Name: "midjourney-refund-debt", Key: "sk-test", Status: common.ChannelStatusEnabled, UsedQuota: 300}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, populateUserCache(user))
	_, err := cacheInitToken(token)
	require.NoError(t, err)
	task := Midjourney{
		UserId:           user.Id,
		MjId:             "mj-refund-debt",
		ChannelId:        channel.Id,
		Quota:            300,
		TokenId:          token.Id,
		BillingChannelId: channel.Id,
		BillingStatus:    MidjourneyBillingStatusCharged,
	}
	require.NoError(t, task.Insert())

	refunded, err := task.RefundBilling(token.Key)

	require.NoError(t, err)
	require.True(t, refunded.Applied)
	assert.Equal(t, 100, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 100, mustGetCachedUserQuota(t, user.Id))
	debt, err := GetUserQuotaDebt(user.Id)
	require.NoError(t, err)
	assert.Zero(t, debt)
	assert.Equal(t, 300, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, 300, mustGetCachedTokenQuota(t, token.Key))
}

func TestMidjourneyChargingReappliesLostCacheDeltaBeforeCommit(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10000)
	token := Token{UserId: user.Id, Key: "midjourney-charge-cache-loss-" + common.GetRandomString(8), Name: "midjourney-charge-cache-loss", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6104, Name: "midjourney-charge-cache-loss", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	task := Midjourney{UserId: user.Id, MjId: "mj-charge-cache-loss", ChannelId: channel.Id, Quota: 3000, TokenId: token.Id, BillingChannelId: channel.Id, BillingStatus: MidjourneyBillingStatusCharging}
	require.NoError(t, task.Insert())
	owner := midjourneyBillingCacheOwner(task.Id, "charge")
	require.NoError(t, common.RDB.Set(t.Context(), getUserQuotaMutationFenceKey(user.Id), owner, 0).Err())
	require.NoError(t, common.RDB.Set(t.Context(), getTokenQuotaMutationFenceKey(token.Key), owner, 0).Err())
	require.NoError(t, common.RDB.Set(t.Context(), midjourneyBillingCacheOperationKey(task.Id, "charge", "user"), 1, 0).Err())
	require.NoError(t, common.RDB.Set(t.Context(), midjourneyBillingCacheOperationKey(task.Id, "charge", "token"), 1, 0).Err())

	settled, err := task.SettleBilling(token.Key)

	require.NoError(t, err)
	require.True(t, settled.Applied)
	assert.Equal(t, 7000, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 2000, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, 7000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, 2000, mustGetCachedTokenQuota(t, token.Key))
	assert.Equal(t, MidjourneyBillingStatusCharged, task.BillingStatus)
}

func TestMidjourneyRefundPendingHydratesCommittedBalanceAfterCacheLoss(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10000)
	token := Token{UserId: user.Id, Key: "midjourney-refund-cache-loss-" + common.GetRandomString(8), Name: "midjourney-refund-cache-loss", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6105, Name: "midjourney-refund-cache-loss", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	task := Midjourney{
		UserId:            user.Id,
		MjId:              "mj-refund-cache-loss",
		ChannelId:         channel.Id,
		Quota:             3000,
		TokenId:           token.Id,
		BillingChannelId:  channel.Id,
		BillingStatus:     MidjourneyBillingStatusRefundPending,
		BillingQuotaDelta: 3000,
	}
	require.NoError(t, task.Insert())

	refunded, err := task.RefundBilling(token.Key)

	require.NoError(t, err)
	require.True(t, refunded.Applied)
	assert.Equal(t, 10000, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 5000, getTokenFromDB(t, token.Id).RemainQuota)
	assert.Equal(t, 10000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, 5000, mustGetCachedTokenQuota(t, token.Key))
	assert.Equal(t, MidjourneyBillingStatusRefunded, task.BillingStatus)
}

func TestMidjourneySettlementRecoversAfterTokenDeletion(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10000)
	token := Token{UserId: user.Id, Key: "midjourney-deleted-token-" + common.GetRandomString(8), Name: "midjourney-deleted-token", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000}
	require.NoError(t, token.Insert(user.SessionGeneration))
	channel := Channel{Id: 6106, Name: "midjourney-deleted-token", Key: "sk-test", Status: common.ChannelStatusEnabled}
	require.NoError(t, DB.Create(&channel).Error)
	require.NoError(t, populateUserCache(user))
	_, err := cacheInitToken(token)
	require.NoError(t, err)
	task := Midjourney{UserId: user.Id, MjId: "mj-deleted-token", ChannelId: channel.Id, Quota: 3000, TokenId: token.Id, BillingChannelId: channel.Id, BillingStatus: MidjourneyBillingStatusPrepared}
	require.NoError(t, task.Insert())
	require.NoError(t, token.Delete())

	settled, err := task.SettleBilling("")

	require.NoError(t, err)
	require.True(t, settled.Applied)
	assert.Equal(t, 7000, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 7000, mustGetCachedUserQuota(t, user.Id))
	assert.Equal(t, MidjourneyBillingStatusCharged, task.BillingStatus)
	exists, err := common.RDB.Exists(t.Context(), getUserQuotaMutationFenceKey(user.Id)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

func TestReserveDoesNotFallbackToDatabaseWhileMidjourneyBillingIsPending(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10000)
	token := Token{UserId: user.Id, Key: "midjourney-fallback-fence-" + common.GetRandomString(8), Name: "midjourney-fallback-fence", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 5000}
	require.NoError(t, token.Insert(user.SessionGeneration))
	task := Midjourney{UserId: user.Id, MjId: "mj-fallback-fence", Quota: 3000, TokenId: token.Id, BillingStatus: MidjourneyBillingStatusCharging}
	require.NoError(t, task.Insert())
	require.NoError(t, common.RDB.Close())

	reserved, err := TryReserveUserQuota(user.Id, 1000)

	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrQuotaCacheMutationPending)
	assert.Equal(t, 10000, getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 1000, false)

	assert.False(t, reserved)
	assert.ErrorIs(t, err, ErrQuotaCacheMutationPending)
	assert.Equal(t, 5000, getTokenFromDB(t, token.Id).RemainQuota)
}

func TestTerminalMidjourneyTaskWithPendingBillingRemainsPollable(t *testing.T) {
	truncateTables(t)

	task := Midjourney{
		UserId:        1,
		MjId:          "mj-terminal-pending",
		ChannelId:     1,
		Progress:      "100%",
		Quota:         3000,
		BillingStatus: MidjourneyBillingStatusRefundPending,
	}
	require.NoError(t, task.Insert())

	tasks := GetAllUnFinishTasks()

	require.Len(t, tasks, 1)
	assert.Equal(t, task.Id, tasks[0].Id)
	assert.True(t, HasUnfinishedMidjourneyTasks())
}

func TestFailedChargedMidjourneyTaskRemainsPollableForRefund(t *testing.T) {
	truncateTables(t)

	task := Midjourney{
		UserId:        1,
		MjId:          "mj-failed-charged",
		ChannelId:     1,
		Status:        "FAILURE",
		Progress:      "100%",
		Quota:         3000,
		BillingStatus: MidjourneyBillingStatusCharged,
	}
	require.NoError(t, task.Insert())

	tasks := GetAllUnFinishTasks()

	require.Len(t, tasks, 1)
	assert.Equal(t, task.Id, tasks[0].Id)
	assert.True(t, HasUnfinishedMidjourneyTasks())
}

func mustGetCachedUserQuota(t *testing.T, userId int) int {
	t.Helper()
	user, err := cacheGetUserBase(userId)
	require.NoError(t, err)
	return user.Quota
}

func mustGetCachedTokenQuota(t *testing.T, tokenKey string) int {
	t.Helper()
	token, err := cacheGetTokenByKey(tokenKey)
	require.NoError(t, err)
	return token.RemainQuota
}
