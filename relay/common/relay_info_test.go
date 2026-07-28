package common

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}

func TestGenRelayInfoResponsesCapturesImageGenerationToolPricingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := GenRelayInfoResponses(ctx, &dto.OpenAIResponsesRequest{
		Model: "gpt-5.4-mini",
		Tools: json.RawMessage(`[
			{"type":"image_generation","quality":"high","size":"1024x1536"}
		]`),
	})

	require.NotNil(t, info.ResponsesUsageInfo)
	imageTool := info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration]
	require.NotNil(t, imageTool)
	require.Equal(t, dto.BuildInToolImageGeneration, imageTool.ToolName)
	require.Equal(t, "high", imageTool.ImageGenerationQuality)
	require.Equal(t, "1024x1536", imageTool.ImageGenerationSize)
}

func TestGenRelayInfoFreezesDeclaredCustomToolPrices(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("OpenAI tools", func(t *testing.T) {
		const toolName = "lookup_openai_customer"
		operation_setting.SetToolPriceForTest(toolName, 11)
		t.Cleanup(func() { operation_setting.DeleteToolPriceForTest(toolName) })

		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx.Set(string(constant.ContextKeyOriginalModel), "frozen-openai-tool-model")
		info := GenRelayInfoOpenAI(ctx, &dto.GeneralOpenAIRequest{
			Tools: []dto.ToolCallRequest{{
				Type:     "function",
				Function: dto.FunctionRequest{Name: toolName},
			}},
		})

		operation_setting.SetToolPriceForTest(toolName, 25)
		require.Equal(t, 11.0, info.GetToolPrice(toolName))
	})

	t.Run("OpenAI legacy functions", func(t *testing.T) {
		const toolName = "lookup_legacy_customer"
		operation_setting.SetToolPriceForTest(toolName, 12)
		t.Cleanup(func() { operation_setting.DeleteToolPriceForTest(toolName) })

		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ctx.Set(string(constant.ContextKeyOriginalModel), "frozen-legacy-tool-model")
		info := GenRelayInfoOpenAI(ctx, &dto.GeneralOpenAIRequest{
			Functions: json.RawMessage(`[{"name":"lookup_legacy_customer"}]`),
		})

		operation_setting.SetToolPriceForTest(toolName, 26)
		require.Equal(t, 12.0, info.GetToolPrice(toolName))
	})

	t.Run("Claude tools", func(t *testing.T) {
		const toolName = "lookup_claude_customer"
		operation_setting.SetToolPriceForTest(toolName, 13)
		t.Cleanup(func() { operation_setting.DeleteToolPriceForTest(toolName) })

		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		ctx.Set(string(constant.ContextKeyOriginalModel), "frozen-claude-tool-model")
		info := GenRelayInfoClaude(ctx, &dto.ClaudeRequest{
			Tools: []any{map[string]any{"name": toolName}},
		})

		operation_setting.SetToolPriceForTest(toolName, 27)
		require.Equal(t, 13.0, info.GetToolPrice(toolName))
	})
}

func TestRelayInfoMetaTypedNilReceiver(t *testing.T) {
	var info *RelayInfo
	var meta convmeta.Meta = info

	assert.Empty(t, meta.GetOriginModelName())
	assert.Empty(t, meta.GetUpstreamModelName())
	assert.False(t, meta.HasChannelMeta())
	assert.Zero(t, meta.GetChannelID())
	assert.Zero(t, meta.GetChannelType())
	assert.False(t, meta.GetIsStream())
	assert.Empty(t, meta.GetReasoningEffort())
	assert.Zero(t, meta.GetEstimatePromptTokens())
	assert.Zero(t, meta.GetSendResponseCount())

	assert.NotPanics(t, func() {
		meta.SetReasoningEffort("high")
		meta.IncrSendResponseCount()
		meta.AppendRequestConversion(types.RelayFormatClaude)
	})

	firstState := meta.EnsureClaudeConvertInfo()
	secondState := meta.EnsureClaudeConvertInfo()
	require.NotNil(t, firstState)
	require.NotNil(t, secondState)
	assert.Equal(t, convmeta.LastMessageTypeNone, firstState.LastMessagesType)
	assert.NotSame(t, firstState, secondState)

	firstOptions := meta.ConvOptions()
	secondOptions := meta.ConvOptions()
	require.NotNil(t, firstOptions)
	require.NotNil(t, secondOptions)
	assert.NotSame(t, firstOptions, secondOptions)
	assert.NotNil(t, firstOptions.Claude.DefaultMaxTokens)
	assert.NotNil(t, firstOptions.Gemini.SupportsImagine)
	assert.NotNil(t, firstOptions.Gemini.SafetySetting)
	assert.NotNil(t, firstOptions.PreserveThinkingSuffix)
}
