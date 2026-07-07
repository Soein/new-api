package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupFrtBreaker 覆盖包级配置与状态，返回被调度的异步禁用次数指针。
// gopoolGo 被替换为只计数不执行，避免测试触碰数据库。
func setupFrtBreaker(t *testing.T, enabled bool, thresholdSec, strikes, windowSec, cooldownSec int) *int {
	t.Helper()
	origEnabled := frtBreakerEnabled
	origThreshold := frtBreakerThresholdSec
	origStrikes := frtBreakerStrikes
	origWindow := frtBreakerWindowSec
	origCooldown := frtBreakerCooldownSec
	origGo := gopoolGo

	frtBreakerEnabled = enabled
	frtBreakerThresholdSec = thresholdSec
	frtBreakerStrikes = strikes
	frtBreakerWindowSec = windowSec
	frtBreakerCooldownSec = cooldownSec
	frtBreakerStrikeTs = make(map[int][]int64)
	frtBreakerLastTrip = make(map[int]int64)

	disables := 0
	gopoolGo = func(f func()) { disables++ }

	t.Cleanup(func() {
		frtBreakerEnabled = origEnabled
		frtBreakerThresholdSec = origThreshold
		frtBreakerStrikes = origStrikes
		frtBreakerWindowSec = origWindow
		frtBreakerCooldownSec = origCooldown
		frtBreakerStrikeTs = make(map[int][]int64)
		frtBreakerLastTrip = make(map[int]int64)
		gopoolGo = origGo
	})
	return &disables
}

func TestFrtBreakerStrikeSkipConditions(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		channelId  int
		frtMs      int64
		chThreshold float64
	}{
		{name: "disabled by default", enabled: false, channelId: 1, frtMs: 60000},
		{name: "non-stream sentinel frt<0", enabled: true, channelId: 1, frtMs: -1000},
		{name: "invalid channel id", enabled: true, channelId: 0, frtMs: 60000},
		{name: "below global threshold", enabled: true, channelId: 1, frtMs: 10000},
		{name: "channel override raises threshold", enabled: true, channelId: 1, frtMs: 20000, chThreshold: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disables := setupFrtBreaker(t, tt.enabled, 15, 1, 300, 600)
			FrtBreakerStrike(tt.channelId, 1, tt.frtMs, tt.chThreshold, true)
			assert.Equal(t, 0, *disables)
			assert.Empty(t, frtBreakerStrikeTs[tt.channelId])
			assert.Zero(t, frtBreakerLastTrip[tt.channelId])
		})
	}
}

func TestFrtBreakerTripsAfterConsecutiveStrikes(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 3, 300, 600)

	FrtBreakerStrike(1, 1, 20000, 0, true)
	FrtBreakerStrike(1, 1, 20000, 0, true)
	require.Equal(t, 0, *disables, "两次连击不应触发")
	require.Len(t, frtBreakerStrikeTs[1], 2)

	FrtBreakerStrike(1, 1, 20000, 0, true)
	require.Equal(t, 1, *disables, "第三次连击应触发禁用")
	assert.Empty(t, frtBreakerStrikeTs[1], "触发后连击计数应清零")
	assert.NotZero(t, frtBreakerLastTrip[1], "触发后应记录冷却起点")
}

func TestFrtBreakerChannelThresholdOverridesGlobal(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 1, 300, 600)

	// 首字 10s：低于全局 15s 不打点，但渠道级 5s 应触发
	FrtBreakerStrike(1, 1, 10000, 5, true)
	assert.Equal(t, 1, *disables)
}

func TestFrtBreakerAutoBanOffOnlyWarns(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 1, 300, 600)

	FrtBreakerStrike(1, 1, 20000, 0, false)
	assert.Equal(t, 0, *disables, "auto_ban 关闭时不应调度禁用")
	assert.NotZero(t, frtBreakerLastTrip[1], "告警同样消耗冷却，避免刷屏")
}

func TestFrtBreakerCooldownSuppressesRetrigger(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 1, 300, 600)

	FrtBreakerStrike(1, 1, 20000, 0, true)
	require.Equal(t, 1, *disables)

	FrtBreakerStrike(1, 1, 20000, 0, true)
	assert.Equal(t, 1, *disables, "冷却期内不应重复触发")
	assert.Empty(t, frtBreakerStrikeTs[1], "冷却期内不应累积连击")
}

func TestFrtBreakerWindowPrunesOldStrikes(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 3, 300, 600)

	// 预置两条已滑出窗口的旧违规，新一次违规后总数应只剩 1，不触发
	old := time.Now().Unix() - 301
	frtBreakerStrikeTs[1] = []int64{old, old}

	FrtBreakerStrike(1, 1, 20000, 0, true)
	assert.Equal(t, 0, *disables)
	assert.Len(t, frtBreakerStrikeTs[1], 1, "窗口外旧违规应被剔除")
}

func TestFrtBreakerCountsPerChannel(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 2, 300, 600)

	FrtBreakerStrike(1, 1, 20000, 0, true)
	FrtBreakerStrike(2, 1, 20000, 0, true)
	assert.Equal(t, 0, *disables, "不同渠道的违规不应互相累积")

	FrtBreakerStrike(1, 1, 20000, 0, true)
	assert.Equal(t, 1, *disables)
}
