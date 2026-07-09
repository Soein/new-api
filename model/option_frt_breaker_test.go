package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestUpdateOptionMapUpdatesFrtBreakerOptions(t *testing.T) {
	oldOptionMap := common.OptionMap
	oldEnabled := common.FrtBreakerEnabled
	oldThresholdSec := common.FrtBreakerThresholdSec
	oldStrikes := common.FrtBreakerStrikes
	oldWindowSec := common.FrtBreakerWindowSec
	oldCooldownSec := common.FrtBreakerCooldownSec
	oldHalfOpenEnabled := common.FrtBreakerHalfOpenEnabled
	oldHalfOpenWindowSec := common.FrtBreakerHalfOpenWindowSec
	oldHalfOpenStrikes := common.FrtBreakerHalfOpenStrikes
	oldHalfOpenSweepSec := common.FrtBreakerHalfOpenSweepSec

	common.OptionMap = map[string]string{}
	t.Cleanup(func() {
		common.OptionMap = oldOptionMap
		common.FrtBreakerEnabled = oldEnabled
		common.FrtBreakerThresholdSec = oldThresholdSec
		common.FrtBreakerStrikes = oldStrikes
		common.FrtBreakerWindowSec = oldWindowSec
		common.FrtBreakerCooldownSec = oldCooldownSec
		common.FrtBreakerHalfOpenEnabled = oldHalfOpenEnabled
		common.FrtBreakerHalfOpenWindowSec = oldHalfOpenWindowSec
		common.FrtBreakerHalfOpenStrikes = oldHalfOpenStrikes
		common.FrtBreakerHalfOpenSweepSec = oldHalfOpenSweepSec
	})

	require.NoError(t, updateOptionMap("FrtBreakerEnabled", "true"))
	require.True(t, common.FrtBreakerEnabled)

	require.NoError(t, updateOptionMap("FrtBreakerThresholdSec", "12"))
	require.Equal(t, 12, common.FrtBreakerThresholdSec)

	require.NoError(t, updateOptionMap("FrtBreakerStrikes", "4"))
	require.Equal(t, 4, common.FrtBreakerStrikes)

	require.NoError(t, updateOptionMap("FrtBreakerWindowSec", "180"))
	require.Equal(t, 180, common.FrtBreakerWindowSec)

	require.NoError(t, updateOptionMap("FrtBreakerCooldownSec", "900"))
	require.Equal(t, 900, common.FrtBreakerCooldownSec)

	require.NoError(t, updateOptionMap("FrtBreakerHalfOpenEnabled", "true"))
	require.True(t, common.FrtBreakerHalfOpenEnabled)

	require.NoError(t, updateOptionMap("FrtBreakerHalfOpenWindowSec", "240"))
	require.Equal(t, 240, common.FrtBreakerHalfOpenWindowSec)

	require.NoError(t, updateOptionMap("FrtBreakerHalfOpenStrikes", "2"))
	require.Equal(t, 2, common.FrtBreakerHalfOpenStrikes)

	require.NoError(t, updateOptionMap("FrtBreakerHalfOpenSweepSec", "45"))
	require.Equal(t, 45, common.FrtBreakerHalfOpenSweepSec)
}
