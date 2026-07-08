package model

import (
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

func TestDecreaseUserQuotaForSettlementRecordsDebt(t *testing.T) {
	truncateTables(t)

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
	require.Equal(t, -50, quota)

	require.ErrorIs(t, DecreaseUserQuota(user.Id, 1, false), ErrUserQuotaInsufficient)
}
