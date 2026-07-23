package model

import (
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestDecreaseUserQuotaDoesNotOverdraw(t *testing.T) {
	truncateTables(t)

	previousBatchUpdate := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() {
		common.BatchUpdateEnabled = previousBatchUpdate
	})

	user := &User{
		Id:       1001,
		Username: "quota-guard-user",
		Quota:    100,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, DB.Create(user).Error)

	require.ErrorIs(t, DecreaseUserQuota(user.Id, 150, false), ErrUserQuotaInsufficient)

	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, 100, quota)

	require.NoError(t, DecreaseUserQuota(user.Id, 60, false))
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, 40, quota)
}

func TestConcurrentSettlementsAccumulateDebtWithoutUnboundedNegativeQuota(t *testing.T) {
	truncateTables(t)
	previousMaxDebt := common.UserQuotaMaxDebtUsd
	common.UserQuotaMaxDebtUsd = 25 / common.QuotaPerUnit
	t.Cleanup(func() { common.UserQuotaMaxDebtUsd = previousMaxDebt })

	user := &User{
		Id:       1004,
		Username: "quota-concurrent-settlement-user",
		Quota:    100,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, DB.Create(user).Error)

	const (
		settlements = 100
		charge      = 10
	)
	var wg sync.WaitGroup
	errorsCh := make(chan error, settlements)
	for i := 0; i < settlements; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsCh <- DecreaseUserQuotaForSettlement(user.Id, charge)
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		require.NoError(t, err)
	}

	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, -25, quota)

	debt, err := GetUserQuotaDebt(user.Id)
	require.NoError(t, err)
	require.EqualValues(t, settlements*charge-125, debt)
}

func TestDecreaseUserQuotaForSettlementRecordsDebt(t *testing.T) {
	truncateTables(t)
	previousMaxDebt := common.UserQuotaMaxDebtUsd
	common.UserQuotaMaxDebtUsd = 0
	t.Cleanup(func() { common.UserQuotaMaxDebtUsd = previousMaxDebt })

	user := &User{
		Id:       1002,
		Username: "quota-settlement-debt-user",
		Quota:    100,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, DB.Create(user).Error)

	require.NoError(t, DecreaseUserQuotaForSettlement(user.Id, 150))

	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, 0, quota)

	debt, err := GetUserQuotaDebt(user.Id)
	require.NoError(t, err)
	require.EqualValues(t, 50, debt)

	require.ErrorIs(t, DecreaseUserQuota(user.Id, 1, false), ErrUserQuotaInsufficient)
}

func TestIncreaseUserQuotaRepaysDebtBeforeRestoringBalance(t *testing.T) {
	truncateTables(t)
	previousMaxDebt := common.UserQuotaMaxDebtUsd
	common.UserQuotaMaxDebtUsd = 0
	t.Cleanup(func() { common.UserQuotaMaxDebtUsd = previousMaxDebt })

	user := &User{
		Id:       1003,
		Username: "quota-debt-repayment-user",
		Quota:    100,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, DB.Create(user).Error)
	require.NoError(t, DecreaseUserQuotaForSettlement(user.Id, 150))

	require.NoError(t, IncreaseUserQuota(user.Id, 80, false))

	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, 30, quota)

	debt, err := GetUserQuotaDebt(user.Id)
	require.NoError(t, err)
	require.Zero(t, debt)
}

func TestSetUserQuotaClearsDebtForExplicitAdminOverride(t *testing.T) {
	truncateTables(t)
	previousMaxDebt := common.UserQuotaMaxDebtUsd
	common.UserQuotaMaxDebtUsd = 0
	t.Cleanup(func() { common.UserQuotaMaxDebtUsd = previousMaxDebt })

	user := &User{
		Id:       1005,
		Username: "quota-debt-override-user",
		Quota:    10,
		Status:   common.UserStatusEnabled,
	}
	require.NoError(t, DB.Create(user).Error)
	require.NoError(t, DecreaseUserQuotaForSettlement(user.Id, 25))

	require.NoError(t, SetUserQuota(user.Id, 40))

	var quota int
	require.NoError(t, DB.Model(&User{}).Where("id = ?", user.Id).Select("quota").Scan(&quota).Error)
	require.Equal(t, 40, quota)
	debt, err := GetUserQuotaDebt(user.Id)
	require.NoError(t, err)
	require.Zero(t, debt)
}
