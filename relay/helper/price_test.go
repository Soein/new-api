package helper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelPriceHelperTieredUsesPreloadedRequestInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"tiered-test-model":"tiered_expr"}`,
		"billing_setting.billing_expr": `{"tiered-test-model":"param(\"stream\") == true ? tier(\"stream\", p * 3) : tier(\"base\", p * 2)"}`,
	}))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/channel/test/1", nil)
	req.Body = nil
	req.ContentLength = 0
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	ctx.Set("group", "default")

	info := &relaycommon.RelayInfo{
		OriginModelName: "tiered-test-model",
		UserGroup:       "default",
		UsingGroup:      "default",
		RequestHeaders:  map[string]string{"Content-Type": "application/json"},
		BillingRequestInput: &billingexpr.RequestInput{
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    []byte(`{"stream":true}`),
		},
	}

	priceData, err := ModelPriceHelper(ctx, info, 1000, &types.TokenCountMeta{})
	require.NoError(t, err)
	require.Equal(t, 1500, priceData.QuotaToPreConsume)
	require.NotNil(t, info.TieredBillingSnapshot)
	require.Equal(t, "stream", info.TieredBillingSnapshot.EstimatedTier)
	require.Equal(t, billing_setting.BillingModeTieredExpr, info.TieredBillingSnapshot.BillingMode)
	require.Equal(t, common.QuotaPerUnit, info.TieredBillingSnapshot.QuotaPerUnit)
}

func TestModelPriceHelperPreConsumeMaxTokensFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	savedModelRatio := ratio_setting.ModelRatio2JSONString()
	savedGroupRatio := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatio))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatio))
	})

	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"non-tiered-fallback-model":2}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"free":0}`))

	const promptTokens = 1000

	cases := []struct {
		name      string
		group     string
		maxTokens int
		expected  int
	}{
		{
			// max_tokens omitted in a paid group -> fall back to 8192 completion tokens.
			// (max(1000, 500) + 8192) * model_ratio(2) * group_ratio(1) = 18384
			name:      "paid group falls back to 8192 completion tokens",
			group:     "default",
			maxTokens: 0,
			expected:  18384,
		},
		{
			// explicit max_tokens is used verbatim, no fallback.
			// (1000 + 100) * 2 = 2200
			name:      "explicit max_tokens is used verbatim",
			group:     "default",
			maxTokens: 100,
			expected:  2200,
		},
		{
			// free group (ratio 0) stays zero; fallback is gated on non-zero group ratio.
			name:      "free group stays zero without fallback",
			group:     "free",
			maxTokens: 0,
			expected:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Content-Type", "application/json")
			ctx.Request = req
			ctx.Set("group", tc.group)

			info := &relaycommon.RelayInfo{
				OriginModelName: "non-tiered-fallback-model",
				UserGroup:       tc.group,
				UsingGroup:      tc.group,
				RequestHeaders:  map[string]string{"Content-Type": "application/json"},
			}

			priceData, err := ModelPriceHelper(ctx, info, promptTokens, &types.TokenCountMeta{MaxTokens: tc.maxTokens})
			require.NoError(t, err)
			require.Equal(t, tc.expected, priceData.QuotaToPreConsume)
		})
	}
}

func TestModelPriceHelperPreConsumesResponsesImageGenerationTool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	savedModelRatio := ratio_setting.ModelRatio2JSONString()
	savedGroupRatio := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(savedModelRatio))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(savedGroupRatio))
	})

	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"responses-image-tool-model":0.375}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"gpt-T":0.1}`))

	cases := []struct {
		name    string
		quality string
		size    string
		price   float64
	}{
		{
			name:    "explicit high rectangular image tool",
			quality: "high",
			size:    "1024x1536",
			price:   operation_setting.GPTImage1High1024x1536,
		},
		{
			name:    "explicit low square image tool",
			quality: "low",
			size:    "1024x1024",
			price:   operation_setting.GPTImage1Low1024x1024,
		},
		{
			name:  "unknown image shape reserves max tool price",
			price: operation_setting.GPTImage1High1024x1536,
		},
		{
			name:    "unknown non-empty image parameters reserve max tool price",
			quality: "auto",
			size:    "1536x1536",
			price:   operation_setting.GPTImage1High1024x1536,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Header.Set("Content-Type", "application/json")
			ctx.Request = req
			ctx.Set("group", "gpt-T")

			info := &relaycommon.RelayInfo{
				OriginModelName: "responses-image-tool-model",
				UserGroup:       "gpt-T",
				UsingGroup:      "gpt-T",
				RequestHeaders:  map[string]string{"Content-Type": "application/json"},
				ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
					BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
						dto.BuildInToolImageGeneration: &relaycommon.BuildInToolInfo{
							ToolName:               dto.BuildInToolImageGeneration,
							ImageGenerationQuality: tc.quality,
							ImageGenerationSize:    tc.size,
						},
					},
				},
			}

			const promptTokens = 1000
			const maxTokens = 100
			priceData, err := ModelPriceHelper(ctx, info, promptTokens, &types.TokenCountMeta{MaxTokens: maxTokens})
			require.NoError(t, err)

			expectedTextQuota := common.QuotaFromFloat(float64(promptTokens+maxTokens) * 0.375 * 0.1)
			expectedToolQuota := common.QuotaFromFloat(tc.price * common.QuotaPerUnit * 0.1)
			require.Equal(t, expectedTextQuota+expectedToolQuota, priceData.QuotaToPreConsume)
		})
	}
}

func TestModelPriceHelperTieredPreConsumeMaxTokensFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode":    `{"tiered-fallback-model":"tiered_expr"}`,
		"billing_setting.billing_expr":    `{"tiered-fallback-model":"tier(\"base\", p * 3 + c * 15)"}`,
		"group_ratio_setting.group_ratio": `{"default":1,"free":0}`,
	}))

	const promptTokens = 1000

	cases := []struct {
		name      string
		group     string
		maxTokens int
		expected  int
	}{
		{
			// max_tokens omitted in a paid group -> fall back to 8192 completion tokens.
			// p*3 + c*15 = 1000*3 + 8192*15 = 125880 -> /1e6 * 500000 = 62940
			name:      "non-free group falls back to 8192 completion tokens",
			group:     "default",
			maxTokens: 0,
			expected:  62940,
		},
		{
			// explicit max_tokens is used verbatim, no fallback.
			// 1000*3 + 100*15 = 4500 -> /1e6 * 500000 = 2250
			name:      "explicit max_tokens is used verbatim",
			group:     "default",
			maxTokens: 100,
			expected:  2250,
		},
		{
			// free group (ratio 0) stays zero; fallback is gated on non-zero group ratio.
			name:      "free group stays zero without fallback",
			group:     "free",
			maxTokens: 0,
			expected:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Content-Type", "application/json")
			ctx.Request = req
			ctx.Set("group", tc.group)

			info := &relaycommon.RelayInfo{
				OriginModelName: "tiered-fallback-model",
				UserGroup:       tc.group,
				UsingGroup:      tc.group,
				RequestHeaders:  map[string]string{"Content-Type": "application/json"},
				BillingRequestInput: &billingexpr.RequestInput{
					Headers: map[string]string{"Content-Type": "application/json"},
					Body:    []byte(`{}`),
				},
			}

			priceData, err := ModelPriceHelper(ctx, info, promptTokens, &types.TokenCountMeta{MaxTokens: tc.maxTokens})
			require.NoError(t, err)
			require.Equal(t, tc.expected, priceData.QuotaToPreConsume)
		})
	}
}
