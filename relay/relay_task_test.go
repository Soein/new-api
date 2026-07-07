package relay

import (
	"math"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
)

func TestRecalcQuotaFromRatios_RejectsNonPositiveAndInfRatios(t *testing.T) {
	tests := []struct {
		name       string
		baseQuota  int
		newRatios  map[string]float64
		wantQuota  int
	}{
		{
			name:      "normal positive ratio applies",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 2},
			wantQuota: 200,
		},
		{
			name:      "negative ratio from a buggy or malicious adaptor is dropped, not multiplied in",
			baseQuota: 100,
			newRatios: map[string]float64{"n": -5},
			wantQuota: 100,
		},
		{
			name:      "zero ratio is dropped",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 0},
			wantQuota: 100,
		},
		{
			name:      "positive infinity ratio is dropped instead of blowing up the quota",
			baseQuota: 100,
			newRatios: map[string]float64{"n": math.Inf(1)},
			wantQuota: 100,
		},
		{
			name:      "NaN ratio is dropped",
			baseQuota: 100,
			newRatios: map[string]float64{"n": math.NaN()},
			wantQuota: 100,
		},
		{
			name:      "ratio of exactly 1.0 is a no-op",
			baseQuota: 100,
			newRatios: map[string]float64{"n": 1.0},
			wantQuota: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				PriceData: types.PriceData{Quota: tt.baseQuota},
			}
			got := recalcQuotaFromRatios(info, tt.newRatios)
			assert.Equal(t, tt.wantQuota, got)
		})
	}
}

func TestRecalcQuotaFromRatios_DividesOutExistingRatiosBeforeApplyingNew(t *testing.T) {
	info := &relaycommon.RelayInfo{
		PriceData: types.PriceData{
			Quota:       200,
			OtherRatios: map[string]float64{"n": 2},
		},
	}
	got := recalcQuotaFromRatios(info, map[string]float64{"n": 4})
	assert.Equal(t, 400, got)
}
