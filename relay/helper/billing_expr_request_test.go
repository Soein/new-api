package helper

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveIncomingBillingExprRequestInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"service_tier":"fast"}`)
	for _, tc := range []struct {
		name      string
		frozen    *billingexpr.RequestInput
		readsBody bool
		wantBody  []byte
	}{
		{name: "param expression reads the body", readsBody: true, wantBody: body},
		{name: "expression without param leaves the body unread"},
		{name: "later param expression reads a body the snapshot skipped", frozen: &billingexpr.RequestInput{}, readsBody: true, wantBody: body},
		{name: "frozen body is kept", frozen: &billingexpr.RequestInput{Body: []byte(`{"service_tier":"flex"}`)}, readsBody: true, wantBody: []byte(`{"service_tier":"flex"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(ctx) })
			info := &relaycommon.RelayInfo{
				RequestHeaders:      map[string]string{"Content-Type": "application/json"},
				BillingRequestInput: tc.frozen,
			}

			input, err := ResolveIncomingBillingExprRequestInput(ctx, info, tc.readsBody)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBody, input.Body)
			assert.Equal(t, "application/json", input.Headers["Content-Type"])
		})
	}
}

func TestBuildBillingExprRequestInputFromRequest(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:  "gemini-3.1-pro-preview",
		Stream: lo.ToPtr(true),
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "hi",
			},
		},
		MaxTokens: lo.ToPtr(uint(3000)),
	}

	input, err := BuildBillingExprRequestInputFromRequest(request, map[string]string{
		"Content-Type": "application/json",
		"X-Test":       "1",
	})
	require.NoError(t, err)
	require.Equal(t, "application/json", input.Headers["Content-Type"])
	require.Equal(t, "1", input.Headers["X-Test"])
	require.Empty(t, input.Body)
	require.True(t, ojson.GetBytes(input.StructuredBody, "stream").Bool())
	require.Equal(t, "user", ojson.GetBytes(input.StructuredBody, "messages.0.role").String())
	require.Equal(t, float64(3000), ojson.GetBytes(input.StructuredBody, "max_tokens").Float())
}

func TestResolveImageBillingExprRequestInputUsesNormalizedFieldsAndRawFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	rawBody := []byte(`{"model":"gpt-image-1","size":"raw","vendor_mode":"fast"}`)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(rawBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set(common.KeyRequestBody, rawBody)

	request := &dto.ImageRequest{
		Model:   "gpt-image-1",
		N:       common.GetPointer(uint(3)),
		Size:    "1024x1536",
		Quality: "high",
	}
	info := &relaycommon.RelayInfo{
		Request:        request,
		RequestHeaders: map[string]string{"Content-Type": "application/json"},
	}

	input, err := ResolveIncomingBillingExprRequestInput(ctx, info, true)
	require.NoError(t, err)
	require.Equal(t, rawBody, input.Body)
	require.Equal(t, "1024x1536", ojson.GetBytes(input.StructuredBody, "size").String())
	require.Equal(t, float64(3), ojson.GetBytes(input.StructuredBody, "n").Float())

	cost, _, err := billingexpr.RunExprWithRequest(
		`v2:rule("normalized", param("size") == "1024x1536", 2) * rule("raw-extra", param("vendor_mode") == "fast", 3)`,
		billingexpr.TokenParams{},
		input,
	)
	require.NoError(t, err)
	require.Equal(t, 6.0, cost)
}

func TestResolveMultipartImageBillingExprRequestInputUsesStructuredDTO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary=test")

	request := &dto.ImageRequest{
		Model:      "gpt-image-1",
		N:          common.GetPointer(uint(2)),
		Size:       "1024x1536",
		Quality:    "high",
		Background: []byte(`"transparent"`),
	}
	input, err := ResolveIncomingBillingExprRequestInput(ctx, &relaycommon.RelayInfo{
		Request:        request,
		RequestHeaders: map[string]string{"Content-Type": ctx.Request.Header.Get("Content-Type")},
	}, true)

	require.NoError(t, err)
	require.Empty(t, input.Body)
	require.Equal(t, "transparent", ojson.GetBytes(input.StructuredBody, "background").String())
	require.Equal(t, float64(2), ojson.GetBytes(input.StructuredBody, "n").Float())
}
