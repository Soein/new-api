package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
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

	t.Run("negative detail in completion tokens details rejected and becomes unknown without text", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"completion_tokens_details":{"image_tokens":-1}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 0, usage.PromptTokens)
		assert.Equal(t, 0, usage.CompletionTokens)
		assert.Equal(t, 0, usage.TotalTokens)
		require.NotNil(t, info.ResponsesUsageInfo)
		assert.Equal(t, relaycommon.ResponsesUsageSourceUnknown, info.ResponsesUsageInfo.UsageSource)
	})

	t.Run("unsupported output tokens details with negative contents does not invalidate native usage", func(t *testing.T) {
		chunk := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"output_tokens_details":{"image_tokens":-1}}}}`
		usage, info, _ := runTestResponsesStream(t, chunk)

		assert.Equal(t, 10, usage.PromptTokens)
		assert.Equal(t, 5, usage.CompletionTokens)
		assert.Equal(t, 15, usage.TotalTokens)
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
