package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

// 生产流量首字（FRT）熔断器：
// 真实请求的首字时间超过阈值记一次违规，滑动窗口内连击达到次数则自动禁用渠道，
// 恢复依赖既有的定时测试自动启用机制（半开探测）。
//
// 设计要点：
//   - 默认关闭，FRT_BREAKER_ENABLED=true 开启，对存量部署零影响
//   - 阈值优先级：渠道额外设置 response_time_threshold_sec > 全局 FRT_BREAKER_THRESHOLD_SEC
//   - 必须连击（默认 5 分钟内 3 次）才触发，单条冷缓存大 prompt 的合法慢请求不会误伤
//   - 尊重渠道级 auto_ban 开关：关闭时只告警不禁用
//   - frt < 0 为非流式哨兵值，跳过
//   - 计数为节点内存级（各节点独立计数），多节点下最先达到连击数的节点执行禁用
var (
	frtBreakerEnabled      = common.GetEnvOrDefaultBool("FRT_BREAKER_ENABLED", false)
	frtBreakerThresholdSec = common.GetEnvOrDefault("FRT_BREAKER_THRESHOLD_SEC", 15)
	frtBreakerStrikes      = common.GetEnvOrDefault("FRT_BREAKER_STRIKES", 3)
	frtBreakerWindowSec    = common.GetEnvOrDefault("FRT_BREAKER_WINDOW_SEC", 300)
	frtBreakerCooldownSec  = common.GetEnvOrDefault("FRT_BREAKER_COOLDOWN_SEC", 600)

	frtBreakerMu       sync.Mutex
	frtBreakerStrikeTs = make(map[int][]int64)
	frtBreakerLastTrip = make(map[int]int64)
)

// FrtBreakerStrike 在每条真实请求完成、frt 已知时调用（渠道测试流量不应调用）。
func FrtBreakerStrike(channelId int, channelType int, frtMs int64, channelThresholdSec float64, autoBan bool) {
	if !frtBreakerEnabled || channelId <= 0 || frtMs < 0 {
		return
	}
	thresholdSec := float64(frtBreakerThresholdSec)
	if channelThresholdSec > 0 {
		thresholdSec = channelThresholdSec
	}
	if float64(frtMs) <= thresholdSec*1000 {
		return
	}

	now := time.Now().Unix()
	frtBreakerMu.Lock()
	defer frtBreakerMu.Unlock()

	// 冷却期内不重复触发，等定时测试的自动启用来做半开恢复
	if now-frtBreakerLastTrip[channelId] < int64(frtBreakerCooldownSec) {
		return
	}

	cutoff := now - int64(frtBreakerWindowSec)
	kept := frtBreakerStrikeTs[channelId][:0]
	for _, ts := range frtBreakerStrikeTs[channelId] {
		if ts > cutoff {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	frtBreakerStrikeTs[channelId] = kept

	if len(kept) < frtBreakerStrikes {
		return
	}

	frtBreakerStrikeTs[channelId] = nil
	frtBreakerLastTrip[channelId] = now
	reason := fmt.Sprintf("生产流量首字熔断：%ds 内 %d 次首字超过 %.0fs（最近一次 %.1fs）",
		frtBreakerWindowSec, frtBreakerStrikes, thresholdSec, float64(frtMs)/1000.0)

	if !autoBan {
		common.SysLog(fmt.Sprintf("FRT熔断告警（渠道 #%d auto_ban 已关，仅告警不禁用）：%s", channelId, reason))
		return
	}

	channelError := types.NewChannelError(channelId, channelType, "", false, "", true)
	gopoolGo(func() {
		DisableChannel(*channelError, reason)
	})
}

// gopoolGo 与 processChannelError 的异步禁用保持同款语义，抽出便于测试替换。
var gopoolGo = func(f func()) { go f() }
