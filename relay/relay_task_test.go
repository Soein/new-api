package relay

import (
	"math"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecalcQuotaFromRatios_RejectsNonPositiveAndInfRatios(t *testing.T) {
	tests := []struct {
		name      string
		baseQuota int
		newRatios map[string]float64
		wantQuota int
		wantOK    bool
	}{
		{
			name:      "normal positive ratio applies",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 2},
			wantQuota: 200,
			wantOK:    true,
		},
		{
			name:      "negative ratio from a buggy or malicious adaptor is dropped, not multiplied in",
			baseQuota: 100,
			newRatios: map[string]float64{"n": -5},
			wantOK:    false,
		},
		{
			name:      "zero ratio is dropped",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 0},
			wantOK:    false,
		},
		{
			name:      "positive infinity ratio is dropped instead of blowing up the quota",
			baseQuota: 100,
			newRatios: map[string]float64{"n": math.Inf(1)},
			wantOK:    false,
		},
		{
			name:      "NaN ratio is dropped",
			baseQuota: 100,
			newRatios: map[string]float64{"n": math.NaN()},
			wantOK:    false,
		},
		{
			name:      "ratio of exactly 1.0 is a no-op",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 1.0},
			wantQuota: 100,
			wantOK:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				PriceData: types.PriceData{Quota: tt.baseQuota},
			}
			got, ok := recalcQuotaFromRatios(info, tt.newRatios)
			assert.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			assert.Equal(t, tt.wantQuota, got)
		})
	}
}

func TestRecalcQuotaFromRatios_DividesOutExistingRatiosBeforeApplyingNew(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota: 200,
		},
	}
	require.True(t, info.PriceData.ReplaceOtherRatios(map[string]float64{"n": 2}))
	got, ok := recalcQuotaFromRatios(info, map[string]float64{"n": 4})
	require.True(t, ok)
	assert.Equal(t, 400, got)
}
