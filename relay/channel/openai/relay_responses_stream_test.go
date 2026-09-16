package openai

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOaiResponsesStreamHandlerCompletedEventWinsClientDisconnect(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         `"total_tokens":30`,
		cancel:         cancel,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: &blockingBody{
			chunk:  []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\n"),
			closed: make(chan struct{}),
		},
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.1"},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 30, usage.TotalTokens)
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.Contains(t, recorder.Body.String(), `"type":"response.completed"`)
}

var initTokenEncodersOnce sync.Once

func runTestResponsesStream(t *testing.T, chunks ...string) (*dto.Usage, *relaycommon.RelayInfo, string) {
	t.Helper()
	initTokenEncodersOnce.Do(service.InitTokenEncoders)

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	var body strings.Builder
	for _, chunk := range chunks {
		body.WriteString("data: ")
		body.WriteString(chunk)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body.String())),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.1"},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	return usage, info, recorder.Body.String()
}

func TestOaiResponsesStreamHandlerSixTerminalsPreserveUsage(t *testing.T) {
	terminals := []struct {
		eventType   string
		status      string
		isException bool
	}{
		{"response.completed", "completed", false},
		{"response.done", "completed", false},
		{"response.failed", "failed", true},
		{"response.incomplete", "incomplete", true},
		{"response.cancelled", "cancelled", true},
		{"response.canceled", "canceled", true},
	}

	for _, tt := range terminals {
		t.Run(tt.eventType+"/valid_12_8", func(t *testing.T) {
			chunk := fmt.Sprintf(`{"type":%q,"response":{"status":%q,"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`, tt.eventType, tt.status)
			usage, info, body := runTestResponsesStream(t, chunk)

			assert.Equal(t, 12, usage.PromptTokens)
			assert.Equal(t, 8, usage.CompletionTokens)
			assert.Equal(t, 20, usage.TotalTokens)
			require.NotNil(t, info.ResponsesUsageInfo)
			assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
			require.NotNil(t, info.StreamStatus)
			if tt.isException {
				assert.True(t, info.StreamStatus.HasErrors())
			} else {
				assert.True(t, info.StreamStatus.IsNormalEnd())
				assert.False(t, info.StreamStatus.HasErrors())
			}
			assert.Contains(t, body, fmt.Sprintf(`"type":%q`, tt.eventType))
		})

		t.Run(tt.eventType+"/explicit_zero", func(t *testing.T) {
			chunk := fmt.Sprintf(`{"type":%q,"response":{"status":%q,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`, tt.eventType, tt.status)
			usage, info, _ := runTestResponsesStream(t, chunk)

			assert.Equal(t, 0, usage.PromptTokens)
			assert.Equal(t, 0, usage.CompletionTokens)
			assert.Equal(t, 0, usage.TotalTokens)
			require.NotNil(t, info.ResponsesUsageInfo)
			assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
			require.NotNil(t, info.StreamStatus)
			if tt.isException {
				assert.True(t, info.StreamStatus.HasErrors())
			} else {
				assert.True(t, info.StreamStatus.IsNormalEnd())
				assert.False(t, info.StreamStatus.HasErrors())
			}
		})

		t.Run(tt.eventType+"/missing_usage", func(t *testing.T) {
			chunk := fmt.Sprintf(`{"type":%q,"response":{"status":%q}}`, tt.eventType, tt.status)
			usage, info, _ := runTestResponsesStream(t, chunk)

			assert.Equal(t, 0, usage.PromptTokens)
			assert.Equal(t, 0, usage.CompletionTokens)
			assert.Equal(t, 0, usage.TotalTokens)
			require.NotNil(t, info.ResponsesUsageInfo)
			assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
			require.NotNil(t, info.StreamStatus)
			if tt.isException {
				assert.True(t, info.StreamStatus.HasErrors())
			} else {
				assert.True(t, info.StreamStatus.IsNormalEnd())
				assert.False(t, info.StreamStatus.HasErrors())
			}
		})
	}
}

func TestOaiResponsesStreamHandlerZeroAndUnknownDistinction(t *testing.T) {
	t.Run("verified zero replaces previous and resists text estimation", func(t *testing.T) {
		chunk1 := `{"type":"response.output_text.delta","delta":"generated output text"}`
		chunk2 := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`
		usage, info, _ := runTestResponsesStream(t, chunk1, chunk2)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("absent usage without estimator evidence is unknown", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed"}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("absent usage with text delta is estimated", func(t *testing.T) {
		chunk1 := `{"type":"response.output_text.delta","delta":"hello world"}`
		chunk2 := `{"type":"response.completed","response":{"status":"completed"}}`
		usage, info, _ := runTestResponsesStream(t, chunk1, chunk2)

		assert.Greater(t, usage.CompletionTokens, 0)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceEstimated, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("missing core without text is unknown never authoritative zero", func(t *testing.T) {
		outputOnly := `{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":8,"total_tokens":8}}}`
		usage, info, _ := runTestResponsesStream(t, outputOnly)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)

		totalOnly := `{"type":"response.completed","response":{"status":"completed","usage":{"total_tokens":20}}}`
		usage, info, _ = runTestResponsesStream(t, totalOnly)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("missing core with text delta falls back to text estimation", func(t *testing.T) {
		chunk1 := `{"type":"response.output_text.delta","delta":"hello world"}`
		chunk2 := `{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":8}}}`
		usage, info, _ := runTestResponsesStream(t, chunk1, chunk2)

		assert.Greater(t, usage.CompletionTokens, 0)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceEstimated, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("outer prompt and completion aliases alone without text remain unknown", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("incoming legacy fields and cache semantics preserved on DTO without setting public source", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"claude_cache_creation_5_m_tokens":50,"claude_cache_creation_1_h_tokens":20}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Equal(t, 50, usage.ClaudeCacheCreation5mTokens)
		assert.Equal(t, 20, usage.ClaudeCacheCreation1hTokens)
		assert.Empty(t, usage.UsageSource)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})
}

func TestOaiResponsesStreamHandlerSnapshotsAndSidecarValidation(t *testing.T) {
	t.Run("handler stops on first terminal and second terminal not forwarded", func(t *testing.T) {
		chunk1 := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
		chunk2 := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":25,"output_tokens":15,"total_tokens":40}}}`
		usage, _, body := runTestResponsesStream(t, chunk1, chunk2)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Contains(t, body, `"total_tokens":15`)
		assert.NotContains(t, body, `"total_tokens":40`)
	})

	t.Run("sidecar missing output is discarded and full native outer retained", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":10}}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Nil(t, usage.BillingUsage)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("incomplete outer with trusted selected sidecar yields canonical usage", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 10, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 0, canonical.CompletionTokens)
		assert.Equal(t, 10, canonical.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("outer with selected sidecar retains sidecar output zero in canonical usage", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 0, canonical.CompletionTokens)
		assert.Equal(t, 10, canonical.TotalTokens)
	})

	t.Run("valid selected sidecar preserves despite malformed unused payload", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"claude_usage":["malformed"]}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 5, canonical.CompletionTokens)
		assert.Equal(t, 15, canonical.TotalTokens)
	})

	t.Run("mixed metadata with absent openai usage selects valid claude sidecar", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"claude_messages","semantic":"openai","claude_usage":{"input_tokens":10,"output_tokens":5}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 5, canonical.CompletionTokens)
		assert.Equal(t, 15, canonical.TotalTokens)
	})

	t.Run("mixed metadata with all-zero openai usage selects valid claude sidecar", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"claude_messages","semantic":"openai","openai_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"claude_usage":{"input_tokens":10,"output_tokens":5}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 5, canonical.CompletionTokens)
		assert.Equal(t, 15, canonical.TotalTokens)
	})

	t.Run("zero-partial openai candidate with missing output selects valid claude sidecar", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"claude_messages","semantic":"openai","openai_usage":{"input_tokens":0},"claude_usage":{"input_tokens":10,"output_tokens":5}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 5, canonical.CompletionTokens)
		assert.Equal(t, 15, canonical.TotalTokens)
	})

	t.Run("zero-partial claude candidate with missing output selects valid gemini sidecar", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"gemini_chat","semantic":"anthropic","claude_usage":{"input_tokens":0},"gemini_usage_metadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 5, canonical.CompletionTokens)
		assert.Equal(t, 15, canonical.TotalTokens)
	})

	t.Run("mixed metadata with nonzero invalid openai candidate rejects sidecar", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"claude_messages","semantic":"openai","openai_usage":{"input_tokens":-10,"output_tokens":5,"total_tokens":15},"claude_usage":{"input_tokens":10,"output_tokens":5}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Nil(t, usage.BillingUsage)
	})

	t.Run("invalid selected sidecar falls back to complete outer", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":-100,"output_tokens":5,"total_tokens":15}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Nil(t, usage.BillingUsage)
	})

	t.Run("all zero sidecar rejected and complete outer retained", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Nil(t, usage.BillingUsage)
	})

	t.Run("known numeric details preserved and unknown nested extension ignored", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":4,"text_tokens":6,"cached_tokens_details":{"ignored_key":123}},"completion_tokens_details":{"reasoning_tokens":3,"text_tokens":2},"unknown_extension":{"ignored_key":123}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Equal(t, 4, usage.PromptTokensDetails.CachedTokens)
		assert.Equal(t, 6, usage.PromptTokensDetails.TextTokens)
		assert.Equal(t, 3, usage.CompletionTokenDetails.ReasoningTokens)
		assert.Equal(t, 2, usage.CompletionTokenDetails.TextTokens)
	})

	rawUsageRejectionCases := []struct {
		name string
		raw  string
	}{
		{
			name: "negative detail in completion tokens details",
			raw:  `"completion_tokens_details":{"image_tokens":-1}`,
		},
		{
			name: "negative detail in output tokens details",
			raw:  `"output_tokens_details":{"image_tokens":-1}`,
		},
		{
			name: "over-int32 detail in output tokens details",
			raw:  `"output_tokens_details":{"reasoning_tokens":2147483648}`,
		},
		{
			name: "negative prompt cache hit tokens",
			raw:  `"prompt_cache_hit_tokens":-1`,
		},
		{
			name: "over-int32 prompt cache hit tokens",
			raw:  `"prompt_cache_hit_tokens":2147483648`,
		},
	}
	for _, tc := range rawUsageRejectionCases {
		t.Run(tc.name+" rejected and becomes unknown without text", func(t *testing.T) {
			chunk := fmt.Sprintf(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,%s}}}`, tc.raw)
			usage, info, _ := runTestResponsesStream(t, chunk)

			assert.Equal(t, 0, usage.PromptTokens)
			assert.Equal(t, 0, usage.CompletionTokens)
			assert.Equal(t, 0, usage.TotalTokens)
			require.NotNil(t, info.ResponsesUsageInfo)
			assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
		})
	}

	t.Run("prompt cache hit tokens explicit zero and null survive validation", func(t *testing.T) {
		chunkZero := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"prompt_cache_hit_tokens":0}}}`
		usage, info, _ := runTestResponsesStream(t, chunkZero)
		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Equal(t, 0, usage.PromptCacheHitTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)

		chunkNull := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"prompt_cache_hit_tokens":null,"output_tokens_details":null}}}`
		usage, info, _ = runTestResponsesStream(t, chunkNull)
		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Equal(t, 0, usage.PromptCacheHitTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("valid openai sidecar with ignored output tokens details retains canonical output zero", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10,"output_tokens_details":{"image_tokens":-1}}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, usage.BillingUsage)
		canonical, ok := usage.BillingUsage.CanonicalUsage()
		require.True(t, ok)
		assert.Equal(t, 10, canonical.PromptTokens)
		assert.Equal(t, 0, canonical.CompletionTokens)
		assert.Equal(t, 10, canonical.TotalTokens)
	})

	t.Run("unsafe canonical sums rejected and falls back to outer", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":2147483640,"output_tokens":10}}}}}`
		usage, _, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Nil(t, usage.BillingUsage)
	})

	t.Run("valid estimated sidecar marks source estimated", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"billing_usage":{"source":"oai_responses","semantic":"openai","estimated":true,"openai_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceEstimated, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("output tokens details and prompt cache metadata survive normalization", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"prompt_cache_hit_tokens":4,"input_tokens_details":{"cached_tokens":4,"text_tokens":6},"output_tokens_details":{"reasoning_tokens":3,"text_tokens":2}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		assert.Equal(t, 4, usage.PromptCacheHitTokens)
		require.NotNil(t, usage.InputTokensDetails)
		assert.Equal(t, 4, usage.InputTokensDetails.CachedTokens)
		assert.Equal(t, 6, usage.InputTokensDetails.TextTokens)
		require.NotNil(t, usage.OutputTokensDetails)
		assert.Equal(t, 3, usage.OutputTokensDetails.ReasoningTokens)
		assert.Equal(t, 2, usage.OutputTokensDetails.TextTokens)
		assert.Equal(t, 3, usage.CompletionTokenDetails.ReasoningTokens)
		assert.Equal(t, 2, usage.CompletionTokenDetails.TextTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
	})
}

type failingStreamWriter struct {
	gin.ResponseWriter
	failAfterBytes int
	written        int
}

func (w *failingStreamWriter) Write(p []byte) (int, error) {
	if w.written+len(p) > w.failAfterBytes {
		return 0, io.ErrClosedPipe
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += n
	return n, err
}

func TestOaiResponsesStreamHandlerDownstreamWriteFailureRecordsError(t *testing.T) {
	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer = &failingStreamWriter{
		ResponseWriter: c.Writer,
		failAfterBytes: 10,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\ndata: [DONE]\n\n")),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-5.1"},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.HasErrors())
}

type delayedTerminalBody struct {
	mu           sync.Mutex
	chunk1       []byte
	chunk2       []byte
	chunk1Sent   bool
	chunk2Sent   bool
	cancelSignal <-chan struct{}
	closed       chan struct{}
}

func newDelayedTerminalBody(chunk1, chunk2 []byte, cancelSignal <-chan struct{}) *delayedTerminalBody {
	return &delayedTerminalBody{
		chunk1:       chunk1,
		chunk2:       chunk2,
		cancelSignal: cancelSignal,
		closed:       make(chan struct{}),
	}
}

func (b *delayedTerminalBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if !b.chunk1Sent {
		b.chunk1Sent = true
		n := copy(p, b.chunk1)
		b.mu.Unlock()
		return n, nil
	}
	b.mu.Unlock()

	select {
	case <-b.cancelSignal:
	case <-b.closed:
		return 0, io.EOF
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.closed:
		return 0, io.EOF
	default:
	}
	if !b.chunk2Sent {
		b.chunk2Sent = true
		n := copy(p, b.chunk2)
		return n, nil
	}
	return 0, io.EOF
}

func (b *delayedTerminalBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

var testSettingMutex sync.Mutex

func setupDrainTest(t *testing.T, channelIDs ...int) {
	t.Helper()
	testSettingMutex.Lock()
	t.Cleanup(testSettingMutex.Unlock)

	initTokenEncodersOnce.Do(service.InitTokenEncoders)

	oldMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(oldMode) })

	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	setting := operation_setting.GetGeneralSetting()
	oldIDs := setting.ResponsesDrainChannelIDs
	if len(channelIDs) == 0 {
		channelIDs = []int{42}
	}
	setting.ResponsesDrainChannelIDs = channelIDs
	t.Cleanup(func() {
		setting.ResponsesDrainChannelIDs = oldIDs
	})
}

func TestOaiResponsesStreamHandlerDelayedTerminalAfterCancellation(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "first delta",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"first delta\"}\n\n"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":15,\"output_tokens\":25,\"total_tokens\":40}}}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelId:         42,
			UpstreamModelName: "gpt-5.1",
		},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 15, usage.PromptTokens)
	assert.Equal(t, 25, usage.CompletionTokens)
	assert.Equal(t, 40, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
	require.NotNil(t, info.StreamStatus)
	// StreamStatus is first-writer-wins by design. When requestContext is canceled,
	// delayedTerminalBody releases the completed terminal and [DONE]. Either the scanner
	// marks done or the cancellation branch marks client_gone first; both are legal.
	assert.Contains(t,
		[]relaycommon.StreamEndReason{relaycommon.StreamEndReasonClientGone, relaycommon.StreamEndReasonDone},
		info.StreamStatus.EndReason,
	)
	assert.Equal(t, "recovered", info.DrainResult)
}

func TestOaiResponsesStreamHandlerNoDownstreamWritesAfterDisconnect(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "delta-1",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"delta-1\"}\n\n"),
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"delta-2\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 30, usage.TotalTokens)

	// Verify that delta-1 was written, but delta-2 and terminal were NOT forwarded downstream after disconnect
	recorded := recorder.Body.String()
	assert.Contains(t, recorded, "delta-1")
	assert.NotContains(t, recorded, "delta-2")
	assert.NotContains(t, recorded, "response.completed")
}

func TestOaiResponsesStreamHandlerWriteFailureRecoversTerminalUsage(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer = &failingStreamWriter{
		ResponseWriter: c.Writer,
		failAfterBytes: 15,
	}

	body := strings.Join([]string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"early chunk\"}",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"second chunk fails write\"}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":30,\"total_tokens\":50}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 20, usage.PromptTokens)
	assert.Equal(t, 30, usage.CompletionTokens)
	assert.Equal(t, 50, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
	assert.Equal(t, "recovered", info.DrainResult)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.HasErrors())
}

func TestOaiResponsesStreamHandlerDuplicateTerminalDoesNotDuplicate(t *testing.T) {
	setupDrainTest(t, 42)

	body := strings.Join([]string{
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"output_tokens\":60,\"total_tokens\":110}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 30, usage.TotalTokens)
}

func TestOaiResponsesStreamHandlerExplicitZeroDuringDrain(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "hello",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 0, usage.PromptTokens)
	assert.Equal(t, 0, usage.CompletionTokens)
	assert.Equal(t, 0, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
	assert.Equal(t, "recovered", info.DrainResult)
}

func TestOaiResponsesStreamHandlerMissingUsageNoPreDisconnectTextUnknown(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "response.created",
		cancel:         cancel,
	}

	// First event has no text delta, then disconnect, then terminal has no usage
	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.created\"}\n\n"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 0, usage.PromptTokens)
	assert.Equal(t, 0, usage.CompletionTokens)
	assert.Equal(t, 0, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.GetResponsesUsageSource())
}

func TestOaiResponsesStreamHandlerDrainOnlyDeltasDoNotGenerateEstimatedCharges(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "response.created",
		cancel:         cancel,
	}

	// Disconnect immediately after response.created. Then deltas arrive during drain, but no terminal usage.
	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.created\"}\n\n"),
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"unseen text delta during drain\"}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	// Must not estimate from unseen deltas arriving during drain
	assert.Equal(t, 0, usage.PromptTokens)
	assert.Equal(t, 0, usage.CompletionTokens)
	assert.Equal(t, 0, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.GetResponsesUsageSource())
}

func TestOaiResponsesStreamHandlerOffChannelPreservesImmediateAbort(t *testing.T) {
	// Channel 999 is NOT in ResponsesDrainChannelIDs
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "delta-1",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"delta-1\"}\n\n"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"output_tokens\":60,\"total_tokens\":110}}}\n\ndata: [DONE]\n\n"),
		make(chan struct{}),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 999, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	// Because drain is disabled for channel 999, body was closed immediately on disconnect.
	// Delayed terminal (50/60/110) was NOT read.
	assert.NotEqual(t, 110, usage.TotalTokens)
	assert.Empty(t, info.DrainResult)
	select {
	case <-body.closed:
	default:
		assert.Fail(t, "expected body.closed to be closed")
	}
}

func TestOaiResponsesStreamHandlerConvertedChatPreservesImmediateAbort(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "delta-1",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"delta-1\"}\n\n"),
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"output_tokens\":60,\"total_tokens\":110}}}\n\ndata: [DONE]\n\n"),
		make(chan struct{}),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:              relayconstant.RelayModeResponses,
		RelayFormat:            types.RelayFormatOpenAIResponses,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses},
		IsStream:               true,
		DisablePing:            true,
		ChannelMeta:            &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	// Converted chat must not enter drain policy
	assert.NotEqual(t, 110, usage.TotalTokens)
	assert.Empty(t, info.DrainResult)
	select {
	case <-body.closed:
	default:
		assert.Fail(t, "expected body.closed to be closed")
	}
}

func TestOaiResponsesStreamHandlerSupplierClientRequestID(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Client-Request-ID", "malicious-downstream-header")

	body := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}\n\ndata: [DONE]\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Set("X-Client-Request-ID", "uuid-from-supplier-4567")
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		RequestId:   "newapi-request-id-111",
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// Preserved supplier response ID faithfully
	assert.Equal(t, "uuid-from-supplier-4567", info.SupplierClientRequestID)
	// Preserved existing internal request ID
	assert.Equal(t, "newapi-request-id-111", info.RequestId)

	other := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 0, 0, 1)
	require.NotNil(t, other)
	snap := other.Snapshot()
	adminInfo, ok := snap["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "uuid-from-supplier-4567", adminInfo["x_client_request_id"])
	_, publicHasID := snap["x_client_request_id"]
	assert.False(t, publicHasID)

	// Check validation rules
	assert.True(t, relaycommon.ValidateSupplierClientRequestID("valid-id_123:ABC.def"))
	assert.False(t, relaycommon.ValidateSupplierClientRequestID(""))
	assert.False(t, relaycommon.ValidateSupplierClientRequestID("id with space"))
	assert.False(t, relaycommon.ValidateSupplierClientRequestID("id;with;semi"))
	assert.False(t, relaycommon.ValidateSupplierClientRequestID(strings.Repeat("a", 129)))
}

type failOnNeedleWriter struct {
	gin.ResponseWriter
	needle string
}

func (w *failOnNeedleWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.needle) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseWriter.Write(p)
}

func (w *failOnNeedleWriter) WriteString(s string) (int, error) {
	if strings.Contains(s, w.needle) {
		return 0, io.ErrClosedPipe
	}
	return io.WriteString(w.ResponseWriter, s)
}

func TestOaiResponsesStreamHandlerCancelBeforeNewDeltaNoTerminalUsagePreservesPreDisconnectEstimate(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
	c.Writer = &cancelAfterWriter{
		ResponseWriter: c.Writer,
		needle:         "pre-disconnect text",
		cancel:         cancel,
	}

	body := newDelayedTerminalBody(
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pre-disconnect text\"}\n\n"),
		[]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"post-disconnect text\"}\n\ndata: [DONE]\n\n"),
		requestContext.Done(),
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	// Estimate must be based only on pre-disconnect text
	preTokens := service.CountTextToken("pre-disconnect text", "gpt-5.1")
	assert.Equal(t, preTokens, usage.CompletionTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceEstimated, info.GetResponsesUsageSource())

	// Downstream writer must NOT receive post-disconnect delta
	recorded := recorder.Body.String()
	assert.Contains(t, recorded, "pre-disconnect text")
	assert.NotContains(t, recorded, "post-disconnect text")
}

func TestOaiResponsesStreamHandlerCancelBeforeAnyDeltaNoTerminalUsageUnknown(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	cancel() // Canceled before any delta arrives
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"arrived after cancel\"}\n\ndata: [DONE]\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)

	assert.Equal(t, 0, usage.PromptTokens)
	assert.Equal(t, 0, usage.CompletionTokens)
	assert.Equal(t, 0, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.GetResponsesUsageSource())
	assert.Empty(t, recorder.Body.String())
}

func TestOaiResponsesStreamHandlerTerminalSavedBeforeFailingTerminalWrite(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Writer = &failOnNeedleWriter{
		ResponseWriter: c.Writer,
		needle:         "response.completed",
	}

	body := strings.Join([]string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"chunk-1\"}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":15,\"output_tokens\":25,\"total_tokens\":40}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 15, usage.PromptTokens)
	assert.Equal(t, 25, usage.CompletionTokens)
	assert.Equal(t, 40, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
	assert.Equal(t, "recovered", info.DrainResult)
}

func TestOaiResponsesStreamHandlerNativeHTTPClientCancellation(t *testing.T) {
	setupDrainTest(t, 42)

	var upstreamRequestCount atomic.Int32
	serverDownstreamCanceled := make(chan struct{})
	testDone := make(chan struct{})
	var testDoneOnce sync.Once
	closeTestDone := func() {
		testDoneOnce.Do(func() {
			close(testDone)
		})
	}

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"initial\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Wait for server-side downstream request context cancellation before allowing upstream final response
		select {
		case <-serverDownstreamCanceled:
		case <-r.Context().Done():
			return
		case <-testDone:
			return
		}

		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstreamServer.Close()

	type handlerResult struct {
		usage  *dto.Usage
		apiErr *types.NewAPIError
		info   *relaycommon.RelayInfo
	}
	resultChan := make(chan handlerResult, 1)

	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		requestCtx := c.Request.Context()
		var cancelSignaled atomic.Bool
		go func() {
			select {
			case <-requestCtx.Done():
				if cancelSignaled.CompareAndSwap(false, true) {
					close(serverDownstreamCanceled)
				}
			case <-testDone:
			}
		}()

		req, err := http.NewRequest(http.MethodPost, upstreamServer.URL, nil)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		upstreamResp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		info := &relaycommon.RelayInfo{
			RelayMode:   relayconstant.RelayModeResponses,
			RelayFormat: types.RelayFormatOpenAIResponses,
			IsStream:    true,
			DisablePing: true,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
		}
		info.InitRequestConversionChain()

		usage, apiErr := OaiResponsesStreamHandler(c, info, upstreamResp)
		resultChan <- handlerResult{
			usage:  usage,
			apiErr: apiErr,
			info:   info,
		}
	})

	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	clientCtx, clientCancel := context.WithCancel(context.Background())
	var clientResp *http.Response
	defer func() {
		clientCancel()
		if clientResp != nil && clientResp.Body != nil {
			_ = clientResp.Body.Close()
		}
		closeTestDone()
	}()

	clientReq, err := http.NewRequestWithContext(clientCtx, http.MethodPost, downstreamServer.URL+"/v1/responses", nil)
	require.NoError(t, err)

	clientResp, err = http.DefaultClient.Do(clientReq)
	require.NoError(t, err)

	reader := bufio.NewReader(clientResp.Body)
	var foundInitial bool
	for i := 0; i < 10; i++ {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "initial") {
			foundInitial = true
			break
		}
	}
	require.True(t, foundInitial, "downstream client must observe initial delta before cancellation")

	clientCancel()
	_ = clientResp.Body.Close()

	select {
	case res := <-resultChan:
		require.Nil(t, res.apiErr)
		require.NotNil(t, res.usage)
		assert.Equal(t, 10, res.usage.PromptTokens)
		assert.Equal(t, 20, res.usage.CompletionTokens)
		assert.Equal(t, 30, res.usage.TotalTokens)
		assert.Equal(t, 5, res.usage.PromptTokensDetails.CachedTokens)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, res.info.GetResponsesUsageSource())
		assert.Equal(t, "recovered", res.info.DrainResult)
	case <-time.After(5 * time.Second):
		t.Fatal("native HTTP cancellation did not complete in time")
	}

	assert.Equal(t, int32(1), upstreamRequestCount.Load(), "upstream request count must remain exactly one")
}

func TestOaiResponsesStreamHandlerOrdinaryOptInSuccessNoDrainResult(t *testing.T) {
	setupDrainTest(t, 42)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	body := strings.Join([]string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"normal flow\"}",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20,\"total_tokens\":30}}}",
		"data: [DONE]",
	}, "\n\n") + "\n\n"

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	info := &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		RelayFormat: types.RelayFormatOpenAIResponses,
		IsStream:    true,
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
	}
	info.InitRequestConversionChain()

	usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 30, usage.TotalTokens)
	assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
	// Normal opt-in request with no downstream closure must have empty DrainResult
	assert.Empty(t, info.DrainResult)
	require.NotNil(t, info.StreamStatus)
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

func TestOaiResponsesStreamHandlerMalformedCreatedAtPreservesAccountingAndStatus(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	t.Run("malformed created_at before status and usage preserves upstream usage and unmodified payload", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"created_at":{"bad":true},"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
		usage, info, body := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
		assert.Contains(t, body, `"created_at":{"bad":true}`)
	})

	t.Run("sglang channel with malformed created_at leaves timestamp unmodified and preserves usage", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		body := "data: {\"type\":\"response.completed\",\"response\":{\"created_at\":\"bad_timestamp\",\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\ndata: [DONE]\n\n"
		resp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}
		info := &relaycommon.RelayInfo{
			IsStream:    true,
			DisablePing: true,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType:       constant.ChannelTypeSGLang,
				UpstreamModelName: "sglang-model",
			},
		}

		usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
		require.Nil(t, apiErr)
		require.NotNil(t, usage)
		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.ResponsesUsageInfo.UsageSource)
		assert.Contains(t, recorder.Body.String(), `"created_at":"bad_timestamp"`)
	})

	t.Run("malformed created_at before failed status still clears prior image call", func(t *testing.T) {
		item := `{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"img_1","status":"completed","result":"base64-a"}}`
		terminal := `{"type":"response.completed","response":{"created_at":true,"status":"failed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`

		usage, info, body := runTestResponsesStream(t, item, terminal)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
		assert.Contains(t, body, `"created_at":true`)
	})

	t.Run("drain write failure with malformed created_at before terminal usage recovers usage", func(t *testing.T) {
		setupDrainTest(t, 42)

		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Writer = &failingStreamWriter{
			ResponseWriter: c.Writer,
			failAfterBytes: 15,
		}

		body := strings.Join([]string{
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"early chunk\"}",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"second chunk fails write\"}",
			"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":{},\"status\":\"completed\",\"usage\":{\"input_tokens\":20,\"output_tokens\":30,\"total_tokens\":50}}}",
			"data: [DONE]",
		}, "\n\n") + "\n\n"

		resp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}
		info := &relaycommon.RelayInfo{
			RelayMode:   relayconstant.RelayModeResponses,
			RelayFormat: types.RelayFormatOpenAIResponses,
			IsStream:    true,
			DisablePing: true,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 42, UpstreamModelName: "gpt-5.1"},
		}
		info.InitRequestConversionChain()

		usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
		require.Nil(t, apiErr)
		require.NotNil(t, usage)
		assert.Equal(t, 20, usage.PromptTokens)
		assert.Equal(t, 30, usage.CompletionTokens)
		assert.Equal(t, 50, usage.TotalTokens)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUpstream, info.GetResponsesUsageSource())
		assert.Equal(t, "recovered", info.DrainResult)
		require.NotNil(t, info.StreamStatus)
		assert.True(t, info.StreamStatus.HasErrors())
	})
}
