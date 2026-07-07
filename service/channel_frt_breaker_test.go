package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupFrtBreaker 覆盖包级配置与状态，返回被调度的异步任务次数指针
//（触发禁用与半开通知都经 gopoolGo）。gopoolGo 被替换为只计数不执行，避免测试触碰数据库。
func setupFrtBreaker(t *testing.T, enabled bool, thresholdSec, strikes, windowSec, cooldownSec int) *int {
	t.Helper()
	origEnabled := frtBreakerEnabled
	origThreshold := frtBreakerThresholdSec
	origStrikes := frtBreakerStrikes
	origWindow := frtBreakerWindowSec
	origCooldown := frtBreakerCooldownSec
	origHalfOpenWindow := frtBreakerHalfOpenWindowSec
	origHalfOpenStrikes := frtBreakerHalfOpenStrikes
	origGo := gopoolGo

	frtBreakerEnabled = enabled
	frtBreakerThresholdSec = thresholdSec
	frtBreakerStrikes = strikes
	frtBreakerWindowSec = windowSec
	frtBreakerCooldownSec = cooldownSec
	frtBreakerStrikeTs = make(map[int][]int64)
	frtBreakerLastTrip = make(map[int]int64)
	frtBreakerHalfOpenUntil = make(map[int]int64)
	frtBreakerHalfOpenHits = make(map[int]int)

	disables := 0
	gopoolGo = func(f func()) { disables++ }

	t.Cleanup(func() {
		frtBreakerEnabled = origEnabled
		frtBreakerThresholdSec = origThreshold
		frtBreakerStrikes = origStrikes
		frtBreakerWindowSec = origWindow
		frtBreakerCooldownSec = origCooldown
		frtBreakerHalfOpenWindowSec = origHalfOpenWindow
		frtBreakerHalfOpenStrikes = origHalfOpenStrikes
		frtBreakerStrikeTs = make(map[int][]int64)
		frtBreakerLastTrip = make(map[int]int64)
		frtBreakerHalfOpenUntil = make(map[int]int64)
		frtBreakerHalfOpenHits = make(map[int]int)
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

func TestFrtBreakerHalfOpenStrikeTripsImmediately(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 3, 300, 600)

	// 半开观察窗内，常规连击数 3 不适用，单次超阈立即重新禁用
	frtBreakerHalfOpenUntil[1] = time.Now().Unix() + 300
	FrtBreakerStrike(1, 1, 20000, 0, true)

	assert.Equal(t, 1, *disables, "半开期单次超阈应立即禁用")
	assert.NotContains(t, frtBreakerHalfOpenUntil, 1, "触发后应退出半开状态")
	assert.NotZero(t, frtBreakerLastTrip[1], "重新禁用后应重置冷却起点")
}

func TestFrtBreakerHalfOpenBelowThresholdNoTrip(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 3, 300, 600)

	frtBreakerHalfOpenUntil[1] = time.Now().Unix() + 300
	FrtBreakerStrike(1, 1, 10000, 0, true)

	assert.Equal(t, 0, *disables)
	assert.Contains(t, frtBreakerHalfOpenUntil, 1, "未超阈应保持半开观察")
	assert.Zero(t, frtBreakerHalfOpenHits[1])
}

func TestFrtBreakerHalfOpenWindowExpiredFallsBackToNormal(t *testing.T) {
	disables := setupFrtBreaker(t, true, 15, 3, 300, 600)

	// 观察窗已过：本次超阈应按常规计数（3 连击），不立即禁用
	frtBreakerHalfOpenUntil[1] = time.Now().Unix() - 1
	FrtBreakerStrike(1, 1, 20000, 0, true)

	assert.Equal(t, 0, *disables, "转正后单次超阈不应触发")
	assert.NotContains(t, frtBreakerHalfOpenUntil, 1, "过期半开状态应被清理")
	assert.Len(t, frtBreakerStrikeTs[1], 1, "应转入常规连击计数")
}

func TestFrtBreakerHalfOpenSweepEnablesFrtDisabledChannel(t *testing.T) {
	setupFrtBreaker(t, true, 15, 3, 300, 600)

	channel := &model.Channel{
		Id:        9101,
		Name:      "frt-half-open-target",
		Key:       "sk-test",
		Status:    common.ChannelStatusAutoDisabled,
		OtherInfo: `{"status_reason":"` + frtBreakerReasonPrefix + `：300s 内 3 次首字超过 15s"}`,
	}
	require.NoError(t, model.DB.Create(channel).Error)
	frtBreakerLastTrip[9101] = time.Now().Unix() - 601

	now := time.Now().Unix()
	frtBreakerHalfOpenSweep(now)

	got, err := model.GetChannelById(9101, false)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, got.Status, "冷却期满应半开启用")
	assert.Equal(t, now+int64(frtBreakerHalfOpenWindowSec), frtBreakerHalfOpenUntil[9101])
}

func TestFrtBreakerHalfOpenSweepSkipsForeignDisableReason(t *testing.T) {
	setupFrtBreaker(t, true, 15, 3, 300, 600)

	channel := &model.Channel{
		Id:        9102,
		Name:      "error-disabled-target",
		Key:       "sk-test",
		Status:    common.ChannelStatusAutoDisabled,
		OtherInfo: `{"status_reason":"error: invalid api key"}`,
	}
	require.NoError(t, model.DB.Create(channel).Error)
	frtBreakerLastTrip[9102] = time.Now().Unix() - 601

	frtBreakerHalfOpenSweep(time.Now().Unix())

	got, err := model.GetChannelById(9102, false)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, got.Status, "非 FRT 禁用的渠道不应被半开接管")
	assert.NotContains(t, frtBreakerHalfOpenUntil, 9102)
	assert.NotContains(t, frtBreakerLastTrip, 9102, "交还常规路径后应清理冷却记录")
}

func TestFrtBreakerHalfOpenSweepRespectsCooldown(t *testing.T) {
	setupFrtBreaker(t, true, 15, 3, 300, 600)

	channel := &model.Channel{
		Id:        9103,
		Name:      "cooling-target",
		Key:       "sk-test",
		Status:    common.ChannelStatusAutoDisabled,
		OtherInfo: `{"status_reason":"` + frtBreakerReasonPrefix + `：test"}`,
	}
	require.NoError(t, model.DB.Create(channel).Error)
	frtBreakerLastTrip[9103] = time.Now().Unix() - 10

	frtBreakerHalfOpenSweep(time.Now().Unix())

	got, err := model.GetChannelById(9103, false)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, got.Status, "冷却期内不应启用")
	assert.Contains(t, frtBreakerLastTrip, 9103)
}
