package operation_setting

import (
	"math"
	"slices"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// 额度展示类型
const (
	QuotaDisplayTypeUSD    = "USD"
	QuotaDisplayTypeCNY    = "CNY"
	QuotaDisplayTypeTokens = "TOKENS"
	QuotaDisplayTypeCustom = "CUSTOM"
)

type GeneralSetting struct {
	DocsLink                  string `json:"docs_link"`
	PingIntervalEnabled       bool   `json:"ping_interval_enabled"`
	PingIntervalSeconds       int    `json:"ping_interval_seconds"`
	StreamPingChannelIDs      []int  `json:"stream_ping_channel_ids"`
	StreamPingIntervalSeconds int    `json:"stream_ping_interval_seconds"`

	// Native OpenAI Responses post-disconnect usage drain settings
	ResponsesDrainChannelIDs      []int `json:"responses_drain_channel_ids"`
	ResponsesDrainTimeoutSeconds  int   `json:"responses_drain_timeout_seconds"`
	ResponsesDrainMaxBytes        int64 `json:"responses_drain_max_bytes"`
	ResponsesDrainMaxGlobalSlots  int   `json:"responses_drain_max_global_slots"`
	ResponsesDrainMaxChannelSlots int   `json:"responses_drain_max_channel_slots"`

	// 当前站点额度展示类型：USD / CNY / TOKENS
	QuotaDisplayType string `json:"quota_display_type"`
	// 自定义货币符号，用于 CUSTOM 展示类型
	CustomCurrencySymbol string `json:"custom_currency_symbol"`
	// 自定义货币与美元汇率（1 USD = X Custom）
	CustomCurrencyExchangeRate float64 `json:"custom_currency_exchange_rate"`
}

// 默认配置
var generalSetting = GeneralSetting{
	DocsLink:                      "https://docs.newapi.pro",
	PingIntervalEnabled:           false,
	PingIntervalSeconds:           60,
	StreamPingChannelIDs:          []int{},
	StreamPingIntervalSeconds:     15,
	ResponsesDrainChannelIDs:      []int{},
	ResponsesDrainTimeoutSeconds:  DefaultResponsesDrainTimeoutSeconds,
	ResponsesDrainMaxBytes:        DefaultResponsesDrainMaxBytes,
	ResponsesDrainMaxGlobalSlots:  DefaultResponsesDrainMaxGlobalSlots,
	ResponsesDrainMaxChannelSlots: DefaultResponsesDrainMaxChannelSlots,
	QuotaDisplayType:              QuotaDisplayTypeUSD,
	CustomCurrencySymbol:          "¤",
	CustomCurrencyExchangeRate:    1.0,
}

func init() {
	// 注册到全局配置管理器
	config.GlobalConfig.Register("general_setting", &generalSetting)
}

func GetGeneralSetting() *GeneralSetting {
	return &generalSetting
}

const (
	DefaultStreamPingIntervalSeconds = 15
	MaxSafePingIntervalSeconds       = 86400
	maxSafeLegacyPingIntervalSeconds = math.MaxInt64 / int64(time.Second)

	DefaultResponsesDrainTimeoutSeconds = 300
	MaxResponsesDrainTimeoutSeconds     = 600

	DefaultResponsesDrainMaxBytes = 8 << 20  // 8 MiB
	MaxResponsesDrainMaxBytes     = 64 << 20 // 64 MiB

	DefaultResponsesDrainMaxGlobalSlots = 16
	MaxResponsesDrainMaxGlobalSlots     = 128

	DefaultResponsesDrainMaxChannelSlots = 4
	MaxResponsesDrainMaxChannelSlots     = 32
)

type ResponsesDrainPolicy struct {
	Enabled         bool
	Timeout         time.Duration
	MaxBytes        int64
	MaxGlobalSlots  int
	MaxChannelSlots int
}

// GetResponsesDrainPolicy returns a snapshot of the drain policy for the given channel.
// Reads configuration safely under common.OptionMapRWMutex.RLock.
func GetResponsesDrainPolicy(channelId int) ResponsesDrainPolicy {
	if channelId <= 0 {
		return ResponsesDrainPolicy{Enabled: false}
	}
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()

	if !slices.Contains(generalSetting.ResponsesDrainChannelIDs, channelId) {
		return ResponsesDrainPolicy{Enabled: false}
	}

	timeoutSec := generalSetting.ResponsesDrainTimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = DefaultResponsesDrainTimeoutSeconds
	} else if timeoutSec > MaxResponsesDrainTimeoutSeconds {
		timeoutSec = MaxResponsesDrainTimeoutSeconds
	}

	maxBytes := generalSetting.ResponsesDrainMaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultResponsesDrainMaxBytes
	} else if maxBytes > MaxResponsesDrainMaxBytes {
		maxBytes = MaxResponsesDrainMaxBytes
	}

	globalSlots := generalSetting.ResponsesDrainMaxGlobalSlots
	if globalSlots <= 0 {
		globalSlots = DefaultResponsesDrainMaxGlobalSlots
	} else if globalSlots > MaxResponsesDrainMaxGlobalSlots {
		globalSlots = MaxResponsesDrainMaxGlobalSlots
	}

	channelSlots := generalSetting.ResponsesDrainMaxChannelSlots
	if channelSlots <= 0 {
		channelSlots = DefaultResponsesDrainMaxChannelSlots
	} else if channelSlots > MaxResponsesDrainMaxChannelSlots {
		channelSlots = MaxResponsesDrainMaxChannelSlots
	}

	return ResponsesDrainPolicy{
		Enabled:         true,
		Timeout:         time.Duration(timeoutSec) * time.Second,
		MaxBytes:        maxBytes,
		MaxGlobalSlots:  globalSlots,
		MaxChannelSlots: channelSlots,
	}
}

// GetStreamPingPolicy returns whether post-header streaming ping is enabled and the interval.
// Reads configuration safely under common.OptionMapRWMutex.RLock.
func GetStreamPingPolicy(channelId int, disablePing bool) (bool, time.Duration) {
	if disablePing {
		return false, 0
	}
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()

	if channelId > 0 && slices.Contains(generalSetting.StreamPingChannelIDs, channelId) {
		sec := generalSetting.StreamPingIntervalSeconds
		if sec <= 0 || sec > MaxSafePingIntervalSeconds {
			sec = DefaultStreamPingIntervalSeconds
		}
		return true, time.Duration(sec) * time.Second
	}

	if !generalSetting.PingIntervalEnabled {
		return false, 0
	}
	sec := generalSetting.PingIntervalSeconds
	if sec <= 0 || int64(sec) > maxSafeLegacyPingIntervalSeconds {
		return true, 10 * time.Second
	}
	return true, time.Duration(sec) * time.Second
}

// GetPreHeaderPingPolicy returns whether pre-header ping is enabled and the interval.
// Pre-header does NOT use channel opt-in (new stream ping), only the global ping setting.
// Reads configuration safely under common.OptionMapRWMutex.RLock.
func GetPreHeaderPingPolicy(disablePing bool) (bool, time.Duration) {
	if disablePing {
		return false, 0
	}
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()

	if !generalSetting.PingIntervalEnabled {
		return false, 0
	}
	sec := generalSetting.PingIntervalSeconds
	if sec <= 0 || int64(sec) > maxSafeLegacyPingIntervalSeconds {
		return true, 10 * time.Second
	}
	return true, time.Duration(sec) * time.Second
}

// IsCurrencyDisplay 是否以货币形式展示（美元或人民币）
func IsCurrencyDisplay() bool {
	return generalSetting.QuotaDisplayType != QuotaDisplayTypeTokens
}

// IsCNYDisplay 是否以人民币展示
func IsCNYDisplay() bool {
	return generalSetting.QuotaDisplayType == QuotaDisplayTypeCNY
}

// GetQuotaDisplayType 返回额度展示类型
func GetQuotaDisplayType() string {
	return generalSetting.QuotaDisplayType
}

// GetCurrencySymbol 返回当前展示类型对应符号
func GetCurrencySymbol() string {
	switch generalSetting.QuotaDisplayType {
	case QuotaDisplayTypeUSD:
		return "$"
	case QuotaDisplayTypeCNY:
		return "¥"
	case QuotaDisplayTypeCustom:
		if generalSetting.CustomCurrencySymbol != "" {
			return generalSetting.CustomCurrencySymbol
		}
		return "¤"
	default:
		return ""
	}
}

// GetUsdToCurrencyRate 返回 1 USD = X <currency> 的 X（TOKENS 不适用）
func GetUsdToCurrencyRate(usdToCny float64) float64 {
	switch generalSetting.QuotaDisplayType {
	case QuotaDisplayTypeUSD:
		return 1
	case QuotaDisplayTypeCNY:
		return usdToCny
	case QuotaDisplayTypeCustom:
		if generalSetting.CustomCurrencyExchangeRate > 0 {
			return generalSetting.CustomCurrencyExchangeRate
		}
		return 1
	default:
		return 1
	}
}
