package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestBuildAlphaSearchRequestBodyPreservesUnknownFields(t *testing.T) {
	raw := []byte(`{
		"id":"req_1",
		"model":"gpt-5.1",
		"input":[{"role":"user","content":"hi"}],
		"commands":{"search_query":[{"q":"weather","recency":1}]},
		"settings":{"locale":"en"},
		"future_field":{"nested":true}
	}`)

	out, err := buildAlphaSearchRequestBody(raw, "gpt-5.1", "gpt-5.1-mapped")
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, common.Unmarshal(out, &body))
	assert.Equal(t, "gpt-5.1-mapped", body["model"])
	assert.Equal(t, "req_1", body["id"])
	require.Contains(t, body, "commands")
	require.Contains(t, body, "settings")
	require.Contains(t, body, "future_field")
	require.Contains(t, body, "input")

	commands, ok := body["commands"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, commands, "search_query")

	future, ok := body["future_field"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, future["nested"])
}

func TestBuildAlphaSearchRequestBodyNoMappingKeepsRawBytes(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.1","commands":{"search_query":[{"q":"x"}]},"future_field":1}`)
	out, err := buildAlphaSearchRequestBody(raw, "gpt-5.1", "gpt-5.1")
	require.NoError(t, err)
	assert.Equal(t, raw, out)
}

// A single model is rewritten where it stands. Routing reads the first
// "model" and most upstreams read the last, so a repeated model comes out as
// one member carrying the mapped value.
func TestBuildAlphaSearchRequestBodyRewritesModel(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"single model keeps its place", `{"model":"gpt-5.1","input":[]}`, `{"model":"gpt-5.1-mapped","input":[]}`},
		{"repeated model collapses", `{"model":"gpt-5.1","input":[],"model":"gpt-5.5-pro"}`, `{"input":[],"model":"gpt-5.1-mapped"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := buildAlphaSearchRequestBody([]byte(tt.raw), "gpt-5.1", "gpt-5.1-mapped")
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(out))
		})
	}
}

type recordedSettlement struct {
	settled  []int
	refunded bool
}

func (s *recordedSettlement) Settle(quota int) error {
	s.settled = append(s.settled, quota)
	return nil
}
func (s *recordedSettlement) Refund(*gin.Context)    { s.refunded = true }
func (*recordedSettlement) NeedsRefund() bool        { return false }
func (*recordedSettlement) GetPreConsumedQuota() int { return 0 }
func (*recordedSettlement) Reserve(int) error        { return nil }

func (*recordedSettlement) ReserveBeforeSubmit(int) error { return nil }

// /v1/tools/search and /v1/tools/fetch follow /v1/alpha/search: the raw body
// reaches /v1/tools/* unchanged and the response returns unchanged. With only
// the built-in prices, a response DeepSeek bills (results, page content)
// settles one call at the routing name's request price and no tokens, an
// empty one releases the reservation, an upstream error keeps its status, and
// a channel that cannot relay the endpoint fails retryably without sending
// anything.
func TestDeepSeekToolsHelper(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	batch, logConsume := common.BatchUpdateEnabled, common.LogConsumeEnabled
	common.BatchUpdateEnabled, common.LogConsumeEnabled = true, false
	t.Cleanup(func() { common.BatchUpdateEnabled, common.LogConsumeEnabled = batch, logConsume })

	const search, fetch = "/v1/tools/search", "/v1/tools/fetch"
	const results = `{"results":[{"title":"DeepSeek","url":"https://deepseek.com/","rerank_score":2.1}]}`
	tests := []struct {
		name, path, body, response string
		channelType, status        int
		settled                    []int
		refunded                   bool
	}{
		{name: "search with results", path: search, body: `{"query":"deepseek","count":2,"future_field":{"nested":true}}`,
			response: results, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, settled: []int{375}},
		{name: "search without results", path: search, body: `{"query":"site:none.invalid deepseek"}`,
			response: `{"results":[]}`, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, refunded: true},
		{name: "fetch with page content", path: fetch, body: `{"url":"https://example.com/"}`,
			response: `{"title":"Example Domain","content":"# Example Domain"}`, channelType: constant.ChannelTypeDeepSeek, status: http.StatusOK, settled: []int{375}},
		{name: "fetch failure keeps its status", path: fetch, body: `{"url":"https://example.com/missing"}`,
			response:    `{"error":{"message":"failed to fetch URL","type":"not_found_error","param":null,"code":"invalid_request_error"}}`,
			channelType: constant.ChannelTypeDeepSeek, status: http.StatusNotFound},
		{name: "search through a New API channel", path: search, body: `{"query":"deepseek"}`,
			response: results, channelType: constant.ChannelTypeNewAPI, status: http.StatusOK, settled: []int{375}},
		{name: "channel without the endpoint", path: search, body: `{"query":"deepseek"}`, channelType: constant.ChannelTypeOpenAI},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(upstream.Close)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			routingName := map[string]string{search: constant.DeepSeekSearchModel, fetch: constant.DeepSeekFetchModel}[tc.path]
			common.SetContextKey(c, constant.ContextKeyOriginalModel, routingName)
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyChannelType, tc.channelType)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")

			request, err := helper.GetAndValidateRequest(c, types.RelayFormatDeepSeekTools)
			require.NoError(t, err)
			info, err := relaycommon.GenRelayInfo(c, types.RelayFormatDeepSeekTools, request, nil)
			require.NoError(t, err)
			info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(tc.body)}
			_, err = helper.ModelPriceHelper(c, info, 0, request.GetTokenCountMeta())
			require.NoError(t, err, "the routing name is priced out of the box")
			settlement := &recordedSettlement{}
			info.Billing = settlement

			apiErr := DeepSeekToolsHelper(c, info)
			if tc.status == 0 {
				require.NotNil(t, apiErr)
				assert.False(t, types.IsSkipRetryError(apiErr))
				assert.Empty(t, gotPath)
				return
			}
			assert.Equal(t, tc.path, gotPath)
			assert.Equal(t, tc.body, string(gotBody))
			assert.Equal(t, tc.settled, settlement.settled)
			assert.Equal(t, tc.refunded, settlement.refunded)
			if tc.status != http.StatusOK {
				require.NotNil(t, apiErr)
				assert.Equal(t, tc.status, apiErr.StatusCode)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tc.status, recorder.Code)
			assert.Equal(t, tc.response, recorder.Body.String())
		})
	}
}

func TestDeepSeekToolsRequestPriceAccounting(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.UserQuotaDebt{}, &model.WalletQuotaReservation{}, &model.Log{}))
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	oldBatch, oldLogConsume, oldRedis := common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RedisEnabled
	oldPrices := ratio_setting.ModelPrice2JSONString()
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RedisEnabled = false, true, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMainType, oldLogType)
		common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RedisEnabled = oldBatch, oldLogConsume, oldRedis
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrices))
	})
	service.InitHttpClient()
	const startingQuota = 2_000_000
	for index, tc := range []struct {
		name, path, response string
		price                *float64
		status               int
		refund, text         bool
		want                 int
	}{
		{name: "default search fixed expression", path: "/v1/tools/search", response: `{"results":[{"title":"result"}]}`, want: 375},
		{name: "default fetch fixed expression", path: "/v1/tools/fetch", response: `{"content":"page"}`, want: 375},
		{name: "administrator search request price", path: "/v1/tools/search", response: `{"results":[{"title":"result"}]}`, price: common.GetPointer(0.1), want: 50_000},
		{name: "administrator fetch request price", path: "/v1/tools/fetch", response: `{"content":"page"}`, price: common.GetPointer(0.1), want: 50_000},
		{name: "explicit zero search price", path: "/v1/tools/search", response: `{"results":[{"title":"result"}]}`, price: common.GetPointer(0.0)},
		{name: "explicit zero fetch price", path: "/v1/tools/fetch", response: `{"content":"page"}`, price: common.GetPointer(0.0)},
		{name: "empty search refunds", path: "/v1/tools/search", response: `{"results":[]}`, price: common.GetPointer(0.1), refund: true},
		{name: "empty fetch refunds", path: "/v1/tools/fetch", response: `{"content":""}`, price: common.GetPointer(0.1), refund: true},
		{name: "accepted search is not a completed result", path: "/v1/tools/search", response: `{"results":[{"title":"result"}]}`, status: http.StatusAccepted, price: common.GetPointer(0.1), refund: true},
		{name: "accepted fetch refunds default fixed price", path: "/v1/tools/fetch", response: `{"content":"page"}`, status: http.StatusAccepted, refund: true},
		{name: "non-array search results refund", path: "/v1/tools/search", response: `{"results":{"title":"result"}}`, price: common.GetPointer(0.1), refund: true},
		{name: "non-string fetch content refunds", path: "/v1/tools/fetch", response: `{"content":false}`, price: common.GetPointer(0.1), refund: true},
		{name: "whitespace fetch content refunds", path: "/v1/tools/fetch", response: `{"content":" \t\n\u3000"}`, price: common.GetPointer(0.1), refund: true},
		{name: "malformed search JSON refunds default fixed price", path: "/v1/tools/search", response: `{"results":[{"title":"result"}]`, refund: true},
		{name: "malformed fetch JSON refunds", path: "/v1/tools/fetch", response: `{"content":"page"`, price: common.GetPointer(0.1), refund: true},
		{name: "failed fetch refunds", path: "/v1/tools/fetch", response: `{"error":{"message":"not found","type":"not_found_error"}}`, status: http.StatusNotFound, price: common.GetPointer(0.1), refund: true},
		{name: "ordinary text zero usage remains free", path: "/v1/tools/search", price: common.GetPointer(0.1), text: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routingName := constant.DeepSeekSearchModel
			body := `{"query":"test"}`
			if tc.path == "/v1/tools/fetch" {
				routingName, body = constant.DeepSeekFetchModel, `{"url":"https://example.com/"}`
			}
			prices := map[string]float64{}
			if tc.price != nil {
				prices[routingName] = *tc.price
			}
			priceJSON, err := common.Marshal(prices)
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(priceJSON)))
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, tc.response)
			}))
			t.Cleanup(upstream.Close)
			user := model.User{Username: fmt.Sprintf("deepseek-price-%d", index), AffCode: fmt.Sprintf("ds-price-%d", index), Quota: startingQuota, Status: common.UserStatusEnabled}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: fmt.Sprintf("deepseek-price-token-%d", index), RemainQuota: startingQuota, Status: common.TokenStatusEnabled}
			require.NoError(t, db.Create(&token).Error)
			channel := model.Channel{Name: "deepseek-price", Key: "unused", Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(&channel).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, routingName)
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeDeepSeek)
			common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstream.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")
			request, err := helper.GetAndValidateRequest(c, types.RelayFormatDeepSeekTools)
			require.NoError(t, err)
			info, err := relaycommon.GenRelayInfo(c, types.RelayFormatDeepSeekTools, request, nil)
			require.NoError(t, err)
			info.UserId, info.TokenId, info.TokenKey = user.Id, token.Id, token.Key
			info.UserSetting.BillingPreference, info.ForcePreConsume = "wallet_only", true
			info.InitChannelMeta(c)
			info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(body)}
			priceData, err := helper.ModelPriceHelper(c, info, 0, request.GetTokenCountMeta())
			require.NoError(t, err)
			require.Nil(t, service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info))
			refunded := make(chan struct{}, 1)
			if tc.refund {
				const callback = "deepseek_price_refund_observed"
				require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "tokens" && tx.Error == nil {
						select {
						case refunded <- struct{}{}:
						default:
						}
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
			}
			if tc.text {
				info.RelayMode = relayconstant.RelayModeChatCompletions
				service.PostTextConsumeQuota(c, info, &dto.Usage{}, nil)
			} else {
				apiErr := DeepSeekToolsHelper(c, info)
				if status < http.StatusOK || status >= http.StatusMultipleChoices {
					require.NotNil(t, apiErr)
					require.NotNil(t, RefundFailedRequestBilling(c, info, apiErr))
				} else {
					require.Nil(t, apiErr)
					assert.Equal(t, status, recorder.Code)
					assert.Equal(t, tc.response, recorder.Body.String())
				}
			}
			if tc.refund {
				select {
				case <-refunded:
				case <-time.After(5 * time.Second):
					t.Fatal("request refund did not finish")
				}
			}
			require.NoError(t, db.First(&user, user.Id).Error)
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, startingQuota-tc.want, user.Quota)
			assert.Equal(t, startingQuota-tc.want, token.RemainQuota)
			assert.Equal(t, tc.want, user.UsedQuota)
			assert.Equal(t, tc.want, token.UsedQuota)
			var logs []model.Log
			require.NoError(t, db.Where("user_id = ?", user.Id).Find(&logs).Error)
			if tc.refund {
				assert.Empty(t, logs)
			} else {
				require.Len(t, logs, 1)
				assert.Equal(t, tc.want, logs[0].Quota)
				assert.Zero(t, logs[0].PromptTokens)
				assert.Zero(t, logs[0].CompletionTokens)
			}
		})
	}
}
