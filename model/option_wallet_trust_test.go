package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestUpdateOptionMapUpdatesWalletTrustBypassOptions(t *testing.T) {
	oldOptionMap := common.OptionMap
	oldEnabled := common.WalletTrustBypassEnabled
	oldMinUsd := common.WalletTrustBypassMinUsd
	oldMaxInflightUsd := common.WalletTrustBypassMaxInflightUsd

	common.OptionMap = map[string]string{}
	t.Cleanup(func() {
		common.OptionMap = oldOptionMap
		common.WalletTrustBypassEnabled = oldEnabled
		common.WalletTrustBypassMinUsd = oldMinUsd
		common.WalletTrustBypassMaxInflightUsd = oldMaxInflightUsd
	})

	require.NoError(t, updateOptionMap("WalletTrustBypassEnabled", "true"))
	require.True(t, common.WalletTrustBypassEnabled)

	require.NoError(t, updateOptionMap("WalletTrustBypassMinUsd", "125.5"))
	require.Equal(t, 125.5, common.WalletTrustBypassMinUsd)

	require.NoError(t, updateOptionMap("WalletTrustBypassMaxInflightUsd", "30.25"))
	require.Equal(t, 30.25, common.WalletTrustBypassMaxInflightUsd)
}
