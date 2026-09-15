package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := relayconvert.NormalizeResponsesUsage(responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	if !relaycommon.IsNonBillableResponsesStatus(responsesResponse.Status) {
		for i := range responsesResponse.Output {
			idx := i
			imageCounter.Observe(&responsesResponse.Output[i], &idx)
		}
	}
	imageCounter.Commit(info)

	return usage, nil
}

func isResponsesTerminalType(t string) bool {
	switch t {
	case "response.completed", "response.done",
		"response.failed", "response.incomplete",
		"response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

type rawResponsesStreamEnvelope struct {
	Type        string               `json:"type"`
	Response    json.RawMessage      `json:"response,omitempty"`
	Delta       string               `json:"delta,omitempty"`
	Item        *dto.ResponsesOutput `json:"item,omitempty"`
	OutputIndex *int                 `json:"output_index,omitempty"`
}

type rawResponsesResponsePayload struct {
	Status json.RawMessage       `json:"status,omitempty"`
	Usage  json.RawMessage       `json:"usage,omitempty"`
	Output []dto.ResponsesOutput `json:"output,omitempty"`
}

func isAbsentOrNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == "null"
}

func parseStrictNonNegativeInt(raw json.RawMessage) (int, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, false
	}
	if trimmed[0] < '0' || trimmed[0] > '9' {
		return 0, false
	}
	var num json.Number
	if err := common.Unmarshal(trimmed, &num); err != nil {
		return 0, false
	}
	str := num.String()
	if strings.ContainsAny(str, ".eE-") {
		return 0, false
	}
	val, err := strconv.ParseInt(str, 10, 64)
	if err != nil || val < 0 || val > math.MaxInt32 {
		return 0, false
	}
	return int(val), true
}

func validateKnownDetailObject(raw json.RawMessage, knownFields []string) bool {
	if isAbsentOrNull(raw) {
		return true
	}
	if common.GetJsonType(raw) != "object" {
		return false
	}
	var detailsMap map[string]json.RawMessage
	if err := common.Unmarshal(raw, &detailsMap); err != nil {
		return false
	}
	for _, field := range knownFields {
		valRaw, ok := detailsMap[field]
		if !ok || isAbsentOrNull(valRaw) {
			continue
		}
		if _, ok := parseStrictNonNegativeInt(valRaw); !ok {
			return false
		}
	}
	return true
}

type rawBillingUsageEnvelope struct {
	Source              string          `json:"source,omitempty"`
	Semantic            string          `json:"semantic,omitempty"`
	Estimated           bool            `json:"estimated,omitempty"`
	OpenAIUsage         json.RawMessage `json:"openai_usage,omitempty"`
	ClaudeUsage         json.RawMessage `json:"claude_usage,omitempty"`
	GeminiUsageMetadata json.RawMessage `json:"gemini_usage_metadata,omitempty"`
}

type sidecarValidationResult int

const (
	sidecarValid sidecarValidationResult = iota
	sidecarAbsentOrZero
	sidecarInvalid
)

func parseAndValidateSelectedSidecar(rawBU json.RawMessage) (*dto.BillingUsage, bool) {
	if isAbsentOrNull(rawBU) || common.GetJsonType(rawBU) != "object" {
		return nil, false
	}

	var rawEnv rawBillingUsageEnvelope
	if err := common.Unmarshal(rawBU, &rawEnv); err != nil {
		return nil, false
	}

	source := strings.TrimSpace(rawEnv.Source)
	semantic := strings.TrimSpace(rawEnv.Semantic)

	// Check eligible candidates in CanonicalUsage priority order:
	// 1. OpenAI
	if strings.EqualFold(source, dto.BillingUsageSourceOAIChat) ||
		strings.EqualFold(source, dto.BillingUsageSourceOAIResponses) ||
		strings.EqualFold(semantic, dto.BillingUsageSemanticOpenAI) {
		bu, res := validateSelectedOpenAISidecar(&rawEnv)
		if res == sidecarValid {
			return bu, true
		}
		if res == sidecarInvalid {
			return nil, false
		}
		// res == sidecarAbsentOrZero: continue existing priority
	}

	// 2. Claude
	if strings.EqualFold(source, dto.BillingUsageSourceClaudeMessages) ||
		strings.EqualFold(semantic, dto.BillingUsageSemanticAnthropic) {
		bu, res := validateSelectedClaudeSidecar(&rawEnv)
		if res == sidecarValid {
			return bu, true
		}
		if res == sidecarInvalid {
			return nil, false
		}
		// res == sidecarAbsentOrZero: continue existing priority
	}

	// 3. Gemini
	if strings.EqualFold(source, dto.BillingUsageSourceGeminiChat) ||
		strings.EqualFold(semantic, dto.BillingUsageSemanticGemini) {
		bu, res := validateSelectedGeminiSidecar(&rawEnv)
		if res == sidecarValid {
			return bu, true
		}
		if res == sidecarInvalid {
			return nil, false
		}
		// res == sidecarAbsentOrZero: continue existing priority
	}

	return nil, false
}

func validateSelectedOpenAISidecar(rawEnv *rawBillingUsageEnvelope) (*dto.BillingUsage, sidecarValidationResult) {
	if isAbsentOrNull(rawEnv.OpenAIUsage) {
		return nil, sidecarAbsentOrZero
	}
	if common.GetJsonType(rawEnv.OpenAIUsage) != "object" {
		return nil, sidecarInvalid
	}

	var rawMap map[string]json.RawMessage
	if err := common.Unmarshal(rawEnv.OpenAIUsage, &rawMap); err != nil {
		return nil, sidecarInvalid
	}
	if len(rawMap) == 0 {
		return nil, sidecarAbsentOrZero
	}

	var u dto.Usage
	if err := common.Unmarshal(rawEnv.OpenAIUsage, &u); err != nil {
		return nil, sidecarInvalid
	}
	if !dto.HasOpenAIUsageTokens(&u) {
		return nil, sidecarAbsentOrZero
	}

	var promptVal int
	hasPromptCounter := false

	if raw, ok := rawMap["prompt_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, sidecarInvalid
		}
		promptVal = val
		hasPromptCounter = true
	}
	if raw, ok := rawMap["input_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, sidecarInvalid
		}
		if !hasPromptCounter || (promptVal == 0 && val > 0) {
			promptVal = val
			hasPromptCounter = true
		}
	}
	if !hasPromptCounter {
		return nil, sidecarInvalid
	}

	var completionVal int
	hasCompletionCounter := false

	if raw, ok := rawMap["completion_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, sidecarInvalid
		}
		completionVal = val
		hasCompletionCounter = true
	}
	if raw, ok := rawMap["output_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, sidecarInvalid
		}
		if !hasCompletionCounter || (completionVal == 0 && val > 0) {
			completionVal = val
			hasCompletionCounter = true
		}
	}
	if !hasCompletionCounter {
		return nil, sidecarInvalid
	}

	var totalVal int
	hasTotal := false
	if raw, ok := rawMap["total_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, sidecarInvalid
		}
		totalVal = val
		hasTotal = true
	}

	sum := int64(promptVal) + int64(completionVal)
	if sum > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if hasTotal && totalVal > 0 && int64(totalVal) < sum {
		return nil, sidecarInvalid
	}

	inputDetails := []string{"cached_tokens", "cached_creation_tokens", "cache_write_tokens", "text_tokens", "audio_tokens", "image_tokens"}
	if raw, ok := rawMap["input_tokens_details"]; ok {
		if !validateKnownDetailObject(raw, inputDetails) {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["prompt_tokens_details"]; ok {
		if !validateKnownDetailObject(raw, inputDetails) {
			return nil, sidecarInvalid
		}
	}

	outputDetails := []string{"reasoning_tokens", "text_tokens", "audio_tokens", "image_tokens"}
	if raw, ok := rawMap["completion_tokens_details"]; ok {
		if !validateKnownDetailObject(raw, outputDetails) {
			return nil, sidecarInvalid
		}
	}

	if raw, ok := rawMap["prompt_cache_hit_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["claude_cache_creation_5_m_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["claude_cache_creation_1_h_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}

	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 ||
		u.InputTokens < 0 || u.OutputTokens < 0 || u.PromptCacheHitTokens < 0 ||
		u.ClaudeCacheCreation5mTokens < 0 || u.ClaudeCacheCreation1hTokens < 0 {
		return nil, sidecarInvalid
	}
	if u.PromptTokens > math.MaxInt32 || u.CompletionTokens > math.MaxInt32 || u.TotalTokens > math.MaxInt32 ||
		u.InputTokens > math.MaxInt32 || u.OutputTokens > math.MaxInt32 || u.PromptCacheHitTokens > math.MaxInt32 ||
		u.ClaudeCacheCreation5mTokens > math.MaxInt32 || u.ClaudeCacheCreation1hTokens > math.MaxInt32 {
		return nil, sidecarInvalid
	}

	if u.TotalTokens > 0 && int64(u.TotalTokens) < sum {
		return nil, sidecarInvalid
	}

	d := u.PromptTokensDetails
	if d.CachedTokens < 0 || d.CachedCreationTokens < 0 || d.CacheWriteTokens < 0 ||
		d.TextTokens < 0 || d.AudioTokens < 0 || d.ImageTokens < 0 ||
		d.CachedTokens > math.MaxInt32 || d.CachedCreationTokens > math.MaxInt32 || d.CacheWriteTokens > math.MaxInt32 ||
		d.TextTokens > math.MaxInt32 || d.AudioTokens > math.MaxInt32 || d.ImageTokens > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	cd := u.CompletionTokenDetails
	if cd.ReasoningTokens < 0 || cd.TextTokens < 0 || cd.AudioTokens < 0 || cd.ImageTokens < 0 ||
		cd.ReasoningTokens > math.MaxInt32 || cd.TextTokens > math.MaxInt32 || cd.AudioTokens > math.MaxInt32 || cd.ImageTokens > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if u.InputTokensDetails != nil {
		id := u.InputTokensDetails
		if id.CachedTokens < 0 || id.CachedCreationTokens < 0 || id.CacheWriteTokens < 0 ||
			id.TextTokens < 0 || id.AudioTokens < 0 || id.ImageTokens < 0 ||
			id.CachedTokens > math.MaxInt32 || id.CachedCreationTokens > math.MaxInt32 || id.CacheWriteTokens > math.MaxInt32 ||
			id.TextTokens > math.MaxInt32 || id.AudioTokens > math.MaxInt32 || id.ImageTokens > math.MaxInt32 {
			return nil, sidecarInvalid
		}
	}

	return &dto.BillingUsage{
		Source:      rawEnv.Source,
		Semantic:    rawEnv.Semantic,
		Estimated:   rawEnv.Estimated,
		OpenAIUsage: &u,
	}, sidecarValid
}

func validateSelectedClaudeSidecar(rawEnv *rawBillingUsageEnvelope) (*dto.BillingUsage, sidecarValidationResult) {
	if isAbsentOrNull(rawEnv.ClaudeUsage) {
		return nil, sidecarAbsentOrZero
	}
	if common.GetJsonType(rawEnv.ClaudeUsage) != "object" {
		return nil, sidecarInvalid
	}

	var rawMap map[string]json.RawMessage
	if err := common.Unmarshal(rawEnv.ClaudeUsage, &rawMap); err != nil {
		return nil, sidecarInvalid
	}
	if len(rawMap) == 0 {
		return nil, sidecarAbsentOrZero
	}

	var u dto.ClaudeUsage
	if err := common.Unmarshal(rawEnv.ClaudeUsage, &u); err != nil {
		return nil, sidecarInvalid
	}
	if !dto.HasClaudeUsageTokens(&u) {
		return nil, sidecarAbsentOrZero
	}

	rawInput, hasInput := rawMap["input_tokens"]
	if !hasInput || isAbsentOrNull(rawInput) {
		return nil, sidecarInvalid
	}
	if _, ok := parseStrictNonNegativeInt(rawInput); !ok {
		return nil, sidecarInvalid
	}

	rawOutput, hasOutput := rawMap["output_tokens"]
	if !hasOutput || isAbsentOrNull(rawOutput) {
		return nil, sidecarInvalid
	}
	if _, ok := parseStrictNonNegativeInt(rawOutput); !ok {
		return nil, sidecarInvalid
	}

	if raw, ok := rawMap["cache_creation_input_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["cache_read_input_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["claude_cache_creation_5_m_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["claude_cache_creation_1_h_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["cache_creation"]; ok && !isAbsentOrNull(raw) {
		if common.GetJsonType(raw) != "object" {
			return nil, sidecarInvalid
		}
		var ccMap map[string]json.RawMessage
		if err := common.Unmarshal(raw, &ccMap); err != nil {
			return nil, sidecarInvalid
		}
		if r, ok := ccMap["ephemeral_5m_input_tokens"]; ok && !isAbsentOrNull(r) {
			if _, ok := parseStrictNonNegativeInt(r); !ok {
				return nil, sidecarInvalid
			}
		}
		if r, ok := ccMap["ephemeral_1h_input_tokens"]; ok && !isAbsentOrNull(r) {
			if _, ok := parseStrictNonNegativeInt(r); !ok {
				return nil, sidecarInvalid
			}
		}
	}

	if u.InputTokens < 0 || u.OutputTokens < 0 ||
		u.CacheCreationInputTokens < 0 || u.CacheReadInputTokens < 0 ||
		u.ClaudeCacheCreation5mTokens < 0 || u.ClaudeCacheCreation1hTokens < 0 {
		return nil, sidecarInvalid
	}
	if u.InputTokens > math.MaxInt32 || u.OutputTokens > math.MaxInt32 ||
		u.CacheCreationInputTokens > math.MaxInt32 || u.CacheReadInputTokens > math.MaxInt32 ||
		u.ClaudeCacheCreation5mTokens > math.MaxInt32 || u.ClaudeCacheCreation1hTokens > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if int64(u.InputTokens)+int64(u.OutputTokens) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if int64(u.InputTokens)+int64(u.CacheReadInputTokens)+int64(u.CacheCreationInputTokens) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if int64(u.ClaudeCacheCreation5mTokens)+int64(u.ClaudeCacheCreation1hTokens) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if u.CacheCreation != nil {
		if u.CacheCreation.Ephemeral5mInputTokens < 0 || u.CacheCreation.Ephemeral1hInputTokens < 0 ||
			u.CacheCreation.Ephemeral5mInputTokens > math.MaxInt32 || u.CacheCreation.Ephemeral1hInputTokens > math.MaxInt32 {
			return nil, sidecarInvalid
		}
		if int64(u.CacheCreation.Ephemeral5mInputTokens)+int64(u.CacheCreation.Ephemeral1hInputTokens) > math.MaxInt32 {
			return nil, sidecarInvalid
		}
	}

	return &dto.BillingUsage{
		Source:      rawEnv.Source,
		Semantic:    rawEnv.Semantic,
		Estimated:   rawEnv.Estimated,
		ClaudeUsage: &u,
	}, sidecarValid
}

func validateSelectedGeminiSidecar(rawEnv *rawBillingUsageEnvelope) (*dto.BillingUsage, sidecarValidationResult) {
	if isAbsentOrNull(rawEnv.GeminiUsageMetadata) {
		return nil, sidecarAbsentOrZero
	}
	if common.GetJsonType(rawEnv.GeminiUsageMetadata) != "object" {
		return nil, sidecarInvalid
	}

	var rawMap map[string]json.RawMessage
	if err := common.Unmarshal(rawEnv.GeminiUsageMetadata, &rawMap); err != nil {
		return nil, sidecarInvalid
	}
	if len(rawMap) == 0 {
		return nil, sidecarAbsentOrZero
	}

	var m dto.GeminiUsageMetadata
	if err := common.Unmarshal(rawEnv.GeminiUsageMetadata, &m); err != nil {
		return nil, sidecarInvalid
	}
	if !dto.HasGeminiUsageMetadataTokens(&m) {
		return nil, sidecarAbsentOrZero
	}

	rawPrompt, hasPrompt := rawMap["promptTokenCount"]
	if !hasPrompt || isAbsentOrNull(rawPrompt) {
		return nil, sidecarInvalid
	}
	if _, ok := parseStrictNonNegativeInt(rawPrompt); !ok {
		return nil, sidecarInvalid
	}

	rawCandidates, hasCandidates := rawMap["candidatesTokenCount"]
	if !hasCandidates || isAbsentOrNull(rawCandidates) {
		return nil, sidecarInvalid
	}
	if _, ok := parseStrictNonNegativeInt(rawCandidates); !ok {
		return nil, sidecarInvalid
	}

	if raw, ok := rawMap["totalTokenCount"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["toolUsePromptTokenCount"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["thoughtsTokenCount"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}
	if raw, ok := rawMap["cachedContentTokenCount"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, sidecarInvalid
		}
	}

	if m.PromptTokenCount < 0 || m.CandidatesTokenCount < 0 || m.TotalTokenCount < 0 ||
		m.CachedContentTokenCount < 0 || m.ThoughtsTokenCount < 0 || m.ToolUsePromptTokenCount < 0 {
		return nil, sidecarInvalid
	}
	if m.PromptTokenCount > math.MaxInt32 || m.CandidatesTokenCount > math.MaxInt32 || m.TotalTokenCount > math.MaxInt32 ||
		m.CachedContentTokenCount > math.MaxInt32 || m.ThoughtsTokenCount > math.MaxInt32 || m.ToolUsePromptTokenCount > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if int64(m.PromptTokenCount)+int64(m.ToolUsePromptTokenCount) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if int64(m.CandidatesTokenCount)+int64(m.ThoughtsTokenCount) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	promptTokens := m.PromptTokenCount + m.ToolUsePromptTokenCount
	completionTokens := m.CandidatesTokenCount + m.ThoughtsTokenCount
	if int64(promptTokens)+int64(completionTokens) > math.MaxInt32 {
		return nil, sidecarInvalid
	}
	if m.TotalTokenCount > 0 && int64(m.TotalTokenCount) < int64(promptTokens)+int64(completionTokens) {
		return nil, sidecarInvalid
	}

	var promptDetailSum int64
	for _, d := range m.PromptTokensDetails {
		if d.TokenCount < 0 || d.TokenCount > math.MaxInt32 {
			return nil, sidecarInvalid
		}
		promptDetailSum += int64(d.TokenCount)
		if promptDetailSum > math.MaxInt32 {
			return nil, sidecarInvalid
		}
	}
	var toolDetailSum int64
	for _, d := range m.ToolUsePromptTokensDetails {
		if d.TokenCount < 0 || d.TokenCount > math.MaxInt32 {
			return nil, sidecarInvalid
		}
		toolDetailSum += int64(d.TokenCount)
		if toolDetailSum > math.MaxInt32 {
			return nil, sidecarInvalid
		}
	}
	var candidateDetailSum int64
	for _, d := range m.CandidatesTokensDetails {
		if d.TokenCount < 0 || d.TokenCount > math.MaxInt32 {
			return nil, sidecarInvalid
		}
		candidateDetailSum += int64(d.TokenCount)
		if candidateDetailSum > math.MaxInt32 {
			return nil, sidecarInvalid
		}
	}

	return &dto.BillingUsage{
		Source:              rawEnv.Source,
		Semantic:            rawEnv.Semantic,
		Estimated:           rawEnv.Estimated,
		GeminiUsageMetadata: &m,
	}, sidecarValid
}

func validateAndParseResponsesUsage(rawUsage json.RawMessage) (*dto.Usage, bool) {
	if isAbsentOrNull(rawUsage) || common.GetJsonType(rawUsage) != "object" {
		return nil, false
	}

	var rawMap map[string]json.RawMessage
	if err := common.Unmarshal(rawUsage, &rawMap); err != nil || len(rawMap) == 0 {
		return nil, false
	}

	var inputVal, outputVal, totalVal int
	var hasInput, hasOutput, hasTotal bool

	if raw, ok := rawMap["input_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, false
		}
		inputVal = val
		hasInput = true
	}

	if raw, ok := rawMap["output_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, false
		}
		outputVal = val
		hasOutput = true
	}

	if raw, ok := rawMap["total_tokens"]; ok && !isAbsentOrNull(raw) {
		val, ok := parseStrictNonNegativeInt(raw)
		if !ok {
			return nil, false
		}
		totalVal = val
		hasTotal = true
	}

	hasBothCoreCounters := hasInput && hasOutput

	inputDetails := []string{"cached_tokens", "cached_creation_tokens", "cache_write_tokens", "text_tokens", "audio_tokens", "image_tokens"}
	if raw, ok := rawMap["input_tokens_details"]; ok {
		if !validateKnownDetailObject(raw, inputDetails) {
			return nil, false
		}
	}

	outputDetails := []string{"text_tokens", "audio_tokens", "image_tokens", "reasoning_tokens"}
	if raw, ok := rawMap["completion_tokens_details"]; ok {
		if !validateKnownDetailObject(raw, outputDetails) {
			return nil, false
		}
	}

	if raw, ok := rawMap["claude_cache_creation_5_m_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, false
		}
	}
	if raw, ok := rawMap["claude_cache_creation_1_h_tokens"]; ok && !isAbsentOrNull(raw) {
		if _, ok := parseStrictNonNegativeInt(raw); !ok {
			return nil, false
		}
	}

	var validBU *dto.BillingUsage
	if rawBU, ok := rawMap["billing_usage"]; ok && !isAbsentOrNull(rawBU) {
		bu, ok := parseAndValidateSelectedSidecar(rawBU)
		if ok {
			validBU = bu
		}
	}

	if hasBothCoreCounters {
		sum := int64(inputVal) + int64(outputVal)
		if sum > math.MaxInt32 {
			return nil, false
		}
		if hasTotal {
			if int64(totalVal) < sum {
				return nil, false
			}
		} else {
			totalVal = int(sum)
		}

		delete(rawMap, "billing_usage")
		cleanOuter, err := common.Marshal(rawMap)
		if err != nil {
			return nil, false
		}
		var decodedUsage dto.Usage
		if err := common.Unmarshal(cleanOuter, &decodedUsage); err != nil {
			return nil, false
		}
		if validBU != nil {
			decodedUsage.BillingUsage = validBU
		}

		normalized := relayconvert.NormalizeResponsesUsage(&decodedUsage)
		normalized.InputTokens = inputVal
		normalized.PromptTokens = inputVal
		normalized.OutputTokens = outputVal
		normalized.CompletionTokens = outputVal
		normalized.TotalTokens = totalVal
		return normalized, true
	}

	if validBU != nil {
		canonical, ok := validBU.CanonicalUsage()
		if ok && canonical != nil {
			return canonical, true
		}
	}

	return nil, false
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	var terminalUsage *dto.Usage
	hasTerminalUsage := false
	terminalSeen := false

	var responseTextBuilder strings.Builder
	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	imageCommitted := false

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if terminalSeen {
			return
		}

		var envelope rawResponsesStreamEnvelope
		if err := common.UnmarshalJsonStr(data, &envelope); err != nil {
			logger.LogError(c, "failed to unmarshal stream response: "+err.Error())
			sr.Error(err)
			return
		}

		var respEnv rawResponsesResponsePayload
		if len(envelope.Response) > 0 && string(envelope.Response) != "null" {
			_ = common.Unmarshal(envelope.Response, &respEnv)
		}

		isTerminal := isResponsesTerminalType(envelope.Type)

		if isTerminal {
			terminalSeen = true

			if len(respEnv.Usage) > 0 && string(respEnv.Usage) != "null" {
				incomingUsage, valid := validateAndParseResponsesUsage(respEnv.Usage)
				if valid {
					terminalUsage = incomingUsage
					hasTerminalUsage = true
				} else {
					logger.LogWarn(c, "invalid responses stream usage rejected")
				}
			}

			isExceptional := envelope.Type == "response.failed" ||
				envelope.Type == "response.incomplete" ||
				envelope.Type == "response.cancelled" ||
				envelope.Type == "response.canceled" ||
				relaycommon.IsNonBillableResponsesStatus(respEnv.Status)

			if isExceptional {
				if !imageCommitted {
					imageCounter.Reset()
					imageCounter.Commit(info)
					imageCommitted = true
				}
				sr.Stop(fmt.Errorf("stream terminal status: %s", envelope.Type))
			} else {
				if !imageCommitted {
					for i := range respEnv.Output {
						idx := i
						imageCounter.Observe(&respEnv.Output[i], &idx)
					}
					imageCounter.Commit(info)
					imageCommitted = true
				}
				sr.Done()
			}

			streamResponse := dto.ResponsesStreamResponse{Type: envelope.Type}
			if err := sendResponsesStreamData(c, streamResponse, data); err != nil {
				logger.LogWarn(c, "send responses stream data failed: "+err.Error())
				sr.Stop(err)
			}
			return
		}

		switch envelope.Type {
		case "response.output_text.delta":
			responseTextBuilder.WriteString(envelope.Delta)
		case dto.ResponsesOutputTypeItemDone:
			if envelope.Item != nil {
				switch envelope.Item.Type {
				case dto.BuildInCallWebSearchCall:
					info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
				case dto.BuildInCallFileSearchCall:
					info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
				case dto.BuildInCallFunctionCall:
					info.CountBillableToolCall(dto.BuildInCallFunctionCall, envelope.Item.Name)
				case dto.ResponsesOutputTypeImageGenerationCall:
					if !imageCommitted {
						imageCounter.Observe(envelope.Item, envelope.OutputIndex)
					}
				}
			}
		}

		streamResponse := dto.ResponsesStreamResponse{Type: envelope.Type}
		if err := sendResponsesStreamData(c, streamResponse, data); err != nil {
			logger.LogWarn(c, "send responses stream data failed: "+err.Error())
			sr.Stop(err)
		}
	})

	var usage *dto.Usage
	var source string

	if hasTerminalUsage {
		usage = terminalUsage
		if usage.BillingUsage != nil && usage.BillingUsage.Estimated {
			source = relaycommon.ResponsesUsageSourceEstimated
		} else {
			source = relaycommon.ResponsesUsageSourceUpstream
		}
	} else {
		tempStr := responseTextBuilder.String()
		if len(tempStr) > 0 {
			completionTokens := service.CountTextToken(tempStr, info.UpstreamModelName)
			promptTokens := info.GetEstimatePromptTokens()
			usage = &dto.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			}
			source = relaycommon.ResponsesUsageSourceEstimated
			common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		} else {
			usage = &dto.Usage{
				PromptTokens:     0,
				CompletionTokens: 0,
				TotalTokens:      0,
			}
			source = relaycommon.ResponsesUsageSourceUnknown
		}
	}

	info.SetResponsesUsageSource(source)

	return usage, nil
}
