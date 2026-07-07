package service

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
)

// 生产流量首字（FRT）熔断器：
// 真实请求的首字时间超过阈值记一次违规，滑动窗口内连击达到次数则自动禁用渠道。
//
// 恢复有两条路径，可共存：
//   - 定时测试自动启用（合成测试请求，见 performChannelTests）
//   - 半开探测（FRT_BREAKER_HALF_OPEN_ENABLED=true）：禁用冷却期满后自动重新启用，
//     放行真实用户流量试探；半开观察窗内首字超阈达到 FRT_BREAKER_HALF_OPEN_STRIKES
//     （默认 1，即一超阈立即）就重新禁用，窗口内无违规则转回常规连击模式
//
// 设计要点：
//   - 默认关闭，FRT_BREAKER_ENABLED=true 开启，对存量部署零影响
//   - 阈值优先级：渠道额外设置 response_time_threshold_sec > 全局 FRT_BREAKER_THRESHOLD_SEC
//   - 必须连击（默认 5 分钟内 3 次）才触发，单条冷缓存大 prompt 的合法慢请求不会误伤
//   - 尊重渠道级 auto_ban 开关：关闭时只告警不禁用
//   - frt < 0 为非流式哨兵值，跳过
//   - 图片生成/编辑请求不打点（整图生成时长 ≠ 首字，见 GenerateTextOtherInfo 调用处）
//   - 连击计数与半开观察窗为节点内存级；半开扫描候选来自数据库（status_reason
//     前缀 + status_time 冷却判定），节点重启或触发节点下线后任一开启半开的
//     节点都能接手恢复，启用成功的节点持有观察窗，其他节点按常规连击计数
//   - 半开只接管「禁因是 FRT 熔断」的渠道，错误关键词禁用、手动禁用一概不碰
var (
	frtBreakerEnabled      = common.GetEnvOrDefaultBool("FRT_BREAKER_ENABLED", false)
	frtBreakerThresholdSec = common.GetEnvOrDefault("FRT_BREAKER_THRESHOLD_SEC", 15)
	frtBreakerStrikes      = common.GetEnvOrDefault("FRT_BREAKER_STRIKES", 3)
	frtBreakerWindowSec    = common.GetEnvOrDefault("FRT_BREAKER_WINDOW_SEC", 300)
	frtBreakerCooldownSec  = common.GetEnvOrDefault("FRT_BREAKER_COOLDOWN_SEC", 600)

	frtBreakerHalfOpenEnabled   = common.GetEnvOrDefaultBool("FRT_BREAKER_HALF_OPEN_ENABLED", false)
	frtBreakerHalfOpenWindowSec = common.GetEnvOrDefault("FRT_BREAKER_HALF_OPEN_WINDOW_SEC", 300)
	frtBreakerHalfOpenStrikes   = common.GetEnvOrDefault("FRT_BREAKER_HALF_OPEN_STRIKES", 1)
	frtBreakerHalfOpenSweepSec  = common.GetEnvOrDefault("FRT_BREAKER_HALF_OPEN_SWEEP_SEC", 30)

	frtBreakerMu            sync.Mutex
	frtBreakerStrikeTs      = make(map[int][]int64)
	frtBreakerLastTrip      = make(map[int]int64)
	frtBreakerHalfOpenUntil = make(map[int]int64)
	frtBreakerHalfOpenHits  = make(map[int]int)
)

// 禁用原因前缀，半开扫描据此识别「归本熔断器管」的渠道
const (
	frtBreakerReasonPrefix         = "生产流量首字熔断"
	frtBreakerHalfOpenReasonPrefix = "半开探测失败"
)

func init() {
	// 连击数下限 1：0 或负数会退化成单次超阈即熔断
	if frtBreakerStrikes < 1 {
		frtBreakerStrikes = 1
	}
	if frtBreakerHalfOpenStrikes < 1 {
		frtBreakerHalfOpenStrikes = 1
	}
	if frtBreakerHalfOpenSweepSec < 5 {
		frtBreakerHalfOpenSweepSec = 5
	}
	if frtBreakerEnabled && frtBreakerHalfOpenEnabled {
		go frtBreakerHalfOpenLoop()
	}
}

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

	// 半开观察窗：渠道刚被真实流量探测性启用，违规按半开连击数（默认 1，一超阈立即）重新禁用
	if until, halfOpen := frtBreakerHalfOpenUntil[channelId]; halfOpen {
		if now >= until {
			// 观察窗已过且无违规，转正回常规连击模式
			delete(frtBreakerHalfOpenUntil, channelId)
			delete(frtBreakerHalfOpenHits, channelId)
		} else {
			frtBreakerHalfOpenHits[channelId]++
			if frtBreakerHalfOpenHits[channelId] < frtBreakerHalfOpenStrikes {
				return
			}
			reason := fmt.Sprintf("%s：半开观察期内首字 %.1fs 超过 %.0fs", frtBreakerHalfOpenReasonPrefix, float64(frtMs)/1000.0, thresholdSec)
			tripFrtBreakerLocked(channelId, channelType, now, reason, autoBan)
			return
		}
	}

	// 冷却期内不重复触发，等定时测试自动启用或半开探测来做恢复
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

	reason := fmt.Sprintf("%s：%ds 内 %d 次首字超过 %.0fs（最近一次 %.1fs）",
		frtBreakerReasonPrefix, frtBreakerWindowSec, frtBreakerStrikes, thresholdSec, float64(frtMs)/1000.0)
	tripFrtBreakerLocked(channelId, channelType, now, reason, autoBan)
}

// tripFrtBreakerLocked 执行一次熔断触发：清计数、记冷却起点、按 auto_ban 禁用或仅告警。
// 调用方必须持有 frtBreakerMu。
func tripFrtBreakerLocked(channelId int, channelType int, now int64, reason string, autoBan bool) {
	frtBreakerStrikeTs[channelId] = nil
	frtBreakerLastTrip[channelId] = now
	delete(frtBreakerHalfOpenUntil, channelId)
	delete(frtBreakerHalfOpenHits, channelId)

	if !autoBan {
		common.SysLog(fmt.Sprintf("FRT熔断告警（渠道 #%d auto_ban 已关，仅告警不禁用）：%s", channelId, reason))
		return
	}

	channelError := types.NewChannelError(channelId, channelType, "", false, "", true)
	gopoolGo(func() {
		DisableChannel(*channelError, reason)
	})
}

// gopoolGo 与 processChannelError 的异步禁用保持同款语义（gopool.Go），抽出便于测试替换。
var gopoolGo = func(f func()) { gopool.Go(f) }

// frtBreakerHalfOpenLoop 周期扫描被本节点 FRT 禁用且冷却期满的渠道，
// 重新启用进入半开观察窗，用真实用户流量探测恢复。
func frtBreakerHalfOpenLoop() {
	ticker := time.NewTicker(time.Duration(frtBreakerHalfOpenSweepSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		frtBreakerHalfOpenSweep(time.Now().Unix())
	}
}

// frtBreakerHalfOpenSweep 执行单轮半开扫描，抽出便于测试。
// 候选来自数据库（自动禁用 + 禁因是 FRT 熔断 + 按 status_time 判定冷却期满），
// 不依赖节点内存：节点重启后、或触发禁用的节点下线时，任一开启半开的节点都能恢复。
// UpdateChannelStatus 幂等（已启用返回 false），多节点并发扫描只有一个节点
// 启用成功并把渠道纳入自己的观察窗。
func frtBreakerHalfOpenSweep(now int64) {
	if model.DB == nil {
		return
	}
	frtBreakerMu.Lock()
	// 清理已过期的半开观察窗（有流量时 FrtBreakerStrike 会顺路清，这里兜底无流量渠道）
	for id, until := range frtBreakerHalfOpenUntil {
		if now >= until {
			delete(frtBreakerHalfOpenUntil, id)
			delete(frtBreakerHalfOpenHits, id)
		}
	}
	frtBreakerMu.Unlock()

	channels, err := model.GetAutoDisabledChannels()
	if err != nil {
		common.SysLog(fmt.Sprintf("FRT半开扫描查询渠道失败：%v", err))
		return
	}
	for _, channel := range channels {
		info := channel.GetOtherInfo()
		reason, _ := info["status_reason"].(string)
		// 只接管禁因出自本熔断器的渠道；错误关键词禁用、手动禁用等一概不碰
		if !strings.HasPrefix(reason, frtBreakerReasonPrefix) && !strings.HasPrefix(reason, frtBreakerHalfOpenReasonPrefix) {
			continue
		}
		// other_info 经 JSON round-trip 后数字为 float64；缺失时保守跳过
		statusTime := int64(0)
		switch v := info["status_time"].(type) {
		case float64:
			statusTime = int64(v)
		case int64:
			statusTime = v
		}
		if statusTime <= 0 || now-statusTime < int64(frtBreakerCooldownSec) {
			continue
		}
		if !model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusEnabled, "") {
			continue
		}
		frtBreakerMu.Lock()
		frtBreakerHalfOpenUntil[channel.Id] = now + int64(frtBreakerHalfOpenWindowSec)
		delete(frtBreakerHalfOpenHits, channel.Id)
		frtBreakerMu.Unlock()

		msg := fmt.Sprintf("通道「%s」（#%d）熔断冷却期满，已半开启用，%ds 内用真实流量探测首字", channel.Name, channel.Id, frtBreakerHalfOpenWindowSec)
		common.SysLog(msg)
		channelId, channelName := channel.Id, channel.Name
		gopoolGo(func() {
			NotifyRootUser(fmt.Sprintf("%s_%d_half_open", dto.NotifyTypeChannelUpdate, channelId),
				fmt.Sprintf("通道「%s」（#%d）已半开启用", channelName, channelId), msg)
		})
	}
}
