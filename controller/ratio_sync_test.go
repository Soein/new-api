package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
)

func TestPricingSyncExpressionPriority(t *testing.T) {
	expression := `tier("base", p * 2 + c * 8 + cr * 0)`
	cases := []struct {
		name       string
		local      map[string]any
		source     map[string]any
		wantFields []string
	}{
		{"equal expressions suppress stale ratios", map[string]any{"billing_mode": map[string]string{"m": "tiered_expr"}, "billing_expr": map[string]string{"m": expression}}, map[string]any{"billing_mode": map[string]string{"m": "tiered_expr"}, "billing_expr": map[string]string{"m": expression}, "model_ratio": map[string]float64{"m": 3}, "model_price": map[string]float64{"m": 2}}, nil},
		{"local expression excludes legacy-only source", map[string]any{"billing_mode": map[string]string{"m": "tiered_expr"}, "billing_expr": map[string]string{"m": expression}}, map[string]any{"model_ratio": map[string]float64{"m": 3}, "completion_ratio": map[string]float64{"m": 2}}, nil},
		{"expression imports without legacy conflicts", map[string]any{"model_ratio": map[string]float64{"m": 1}}, map[string]any{"billing_mode": map[string]string{"m": "tiered_expr"}, "billing_expr": map[string]string{"m": expression}, "model_ratio": map[string]float64{"m": 3}, "model_price": map[string]float64{"m": 2}}, []string{"billing_mode", "billing_expr"}},
		{"inactive expression follows explicit ratio mode", map[string]any{"model_ratio": map[string]float64{"m": 1}}, map[string]any{"billing_mode": map[string]string{"m": "ratio"}, "billing_expr": map[string]string{"m": expression}, "model_ratio": map[string]float64{"m": 3}}, []string{"model_ratio"}},
		{"empty active expression never imports a false free price", map[string]any{}, map[string]any{"billing_mode": map[string]string{"m": "tiered_expr"}, "billing_expr": map[string]string{"m": " "}, "model_ratio": map[string]float64{"m": 0}}, nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			diff := buildDifferences(tt.local, []struct {
				name string
				data map[string]any
			}{{"source", tt.source}})
			fields := make([]string, 0, len(diff["m"]))
			for field := range diff["m"] {
				fields = append(fields, field)
			}
			assert.ElementsMatch(t, tt.wantFields, fields)
		})
	}
}

func TestRatioConfigExportsEffectiveExpressions(t *testing.T) {
	before := config.GlobalConfig.ExportAllConfigs()
	expose := ratio_setting.IsExposeRatioEnabled()
	t.Cleanup(func() {
		config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": before["billing_setting.billing_mode"], "billing_expr": before["billing_setting.billing_expr"]})
		ratio_setting.SetExposeRatioEnabled(expose)
	})
	config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": `{"sync-export":"tiered_expr"}`, "billing_expr": `{"sync-export":"tier(\"base\", p * 2)"}`})
	ratio_setting.SetExposeRatioEnabled(true)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/ratio_config", nil)
	GetRatioConfig(c)
	var response struct {
		Success bool
		Data    map[string]any
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, billing_setting.BillingModeTieredExpr, valueMap(response.Data["billing_mode"])["sync-export"])
	assert.Equal(t, `tier("base", p * 2)`, valueMap(response.Data["billing_expr"])["sync-export"])
}

func TestPricingSyncCompleteSourcesAndArrayFormats(t *testing.T) {
	before := config.GlobalConfig.ExportAllConfigs()
	oldRatios, oldCompletion := ratio_setting.ModelRatio2JSONString(), ratio_setting.CompletionRatio2JSONString()
	t.Cleanup(func() {
		config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": before["billing_setting.billing_mode"], "billing_expr": before["billing_setting.billing_expr"]})
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldRatios))
		require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(oldCompletion))
	})
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"sync-token":1}`))
	require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(`{"sync-token":2}`))
	expression := `len <= 200000 ? tier("short", p * 2 + c * 8 + cr * 0) : tier("long", p * 4 + c * 12)`
	expressions, err := common.Marshal(map[string]string{"sync-already": expression})
	require.NoError(t, err)
	config.UpdateConfigFromMap(config.GlobalConfig.Get("billing_setting"), map[string]string{"billing_mode": `{"sync-already":"tiered_expr"}`, "billing_expr": string(expressions)})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		if r.URL.Path == "/ratio_config" {
			data = map[string]any{
				"billing_mode":     map[string]string{"sync-already": "tiered_expr", "sync-expression": "tiered_expr"},
				"billing_expr":     map[string]string{"sync-already": expression, "sync-expression": expression},
				"model_ratio":      map[string]float64{"sync-already": 9, "sync-expression": 9, "sync-token": 1},
				"model_price":      map[string]float64{"sync-expression": 4},
				"completion_ratio": map[string]float64{"sync-token": 4},
				"cache_ratio":      map[string]float64{"sync-token": 0},
			}
		} else {
			data = []map[string]any{
				{"model_name": "sync-already", "model_ratio": 5, "completion_ratio": 3},
				{"model_name": "sync-expression", "model_ratio": 2, "model_price": 1},
				{"model_name": "sync-array-expression", "billing_mode": "tiered_expr", "billing_expr": expression, "quota_type": 1, "model_price": 0},
				{"model_name": "sync-unpriced"},
				{"model_name": "sync-invalid-expression", "billing_mode": "tiered_expr", "billing_expr": "", "model_ratio": 0},
				{"model_name": "sync-free", "model_ratio": 0, "completion_ratio": 0},
			}
		}
		encoded, err := common.Marshal(map[string]any{"success": true, "data": data})
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}))
	defer server.Close()
	var response struct {
		Success bool
		Data    struct {
			Differences map[string]map[string]dto.DifferenceItem
			Prices      map[string]struct {
				Current   map[string]any
				Upstreams map[string]map[string]any
			}
			TestResults []dto.TestResult `json:"test_results"`
		}
	}
	body := map[string]any{"upstreams": []map[string]any{
		{"id": 1, "name": "Expressions", "base_url": server.URL, "endpoint": "/ratio_config"},
		{"id": 2, "name": "Legacy", "base_url": server.URL, "endpoint": "/pricing"},
	}}
	recorder := modelManagementRequest(t, FetchUpstreamRatios, http.MethodPost, "/api/channel/fetch_upstream_ratios", body, &response)
	require.True(t, response.Success, recorder.Body.String())
	require.Len(t, response.Data.TestResults, 2)
	for _, result := range response.Data.TestResults {
		require.Equal(t, "success", result.Status, result.Error)
	}
	assert.NotContains(t, response.Data.Differences, "sync-already")
	assert.NotContains(t, response.Data.Differences, "sync-unpriced")
	assert.NotContains(t, response.Data.Differences, "sync-invalid-expression")
	assert.Equal(t, map[string]any{"billing_mode": "tiered_expr", "billing_expr": expression}, response.Data.Prices["sync-expression"].Upstreams["Expressions(1)"])
	assert.NotContains(t, response.Data.Prices["sync-expression"].Upstreams, "Legacy(2)")
	assert.Equal(t, map[string]any{"billing_mode": "tiered_expr", "billing_expr": expression}, response.Data.Prices["sync-array-expression"].Upstreams["Legacy(2)"])
	assert.Equal(t, float64(1), response.Data.Prices["sync-token"].Upstreams["Expressions(1)"]["model_ratio"], "unchanged base prices are included for a complete price preview")
	assert.Equal(t, float64(4), response.Data.Prices["sync-token"].Upstreams["Expressions(1)"]["completion_ratio"])
	assert.Equal(t, float64(0), response.Data.Prices["sync-token"].Upstreams["Expressions(1)"]["cache_ratio"])
	assert.Equal(t, float64(0), response.Data.Prices["sync-free"].Upstreams["Legacy(2)"]["model_ratio"])
	assert.NotContains(t, response.Data.Prices, "sync-already")
	body["include_unchanged"] = true
	recorder = modelManagementRequest(t, FetchUpstreamRatios, http.MethodPost, "/api/ratio_sync/fetch", body, &response)
	require.True(t, response.Success, recorder.Body.String())
	assert.Equal(t, map[string]any{"billing_mode": "tiered_expr", "billing_expr": expression}, response.Data.Prices["sync-already"].Upstreams["Expressions(1)"])
	assert.NotContains(t, response.Data.Prices["sync-already"].Upstreams, "Legacy(2)")
	assert.NotContains(t, response.Data.Prices, "sync-unpriced")
	assert.NotContains(t, response.Data.Prices, "sync-invalid-expression")
	assert.NotContains(t, response.Data.Differences, "sync-already")
}

func TestPricingSyncSharedNumericAliases(t *testing.T) {
	const first, selected, expressionModel = "gpt-4o-gizmo-sync-a", "gpt-4o-gizmo-sync-b", "gpt-4o-gizmo-sync-expression"
	const shared, standalone = "gpt-4o-gizmo-*", "sync-token-only"
	const expression = `tier("base", p * 2 + c * 8)`
	for _, tc := range []struct {
		name, option, field string
		initial, completion float64
		confidence          bool
	}{
		{"token", "ModelRatio", "model_ratio", 1, 4, true},
		{"untrusted token source", "ModelRatio", "model_ratio", 37.5, 1, false},
		{"fixed explicit zero", "ModelPrice", "model_price", 0, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelManagementDB(t, "sqlite", "")
			base, err := common.Marshal(map[string]float64{shared: tc.initial, first: 99})
			require.NoError(t, err)
			options := map[string]string{
				"ModelRatio":                   `{"sync-token-only":3}`,
				"AudioRatio":                   `{"gpt-4o-gizmo-*":5}`,
				"AudioCompletionRatio":         `{"gpt-4o-gizmo-*":6}`,
				"CacheRatio":                   `{"gpt-4o-gizmo-*":9,"gpt-4o-gizmo-sync-a":0}`,
				"CreateCacheRatio":             `{"gpt-4o-gizmo-*":9,"gpt-4o-gizmo-sync-a":0}`,
				"ImageRatio":                   `{"gpt-4o-gizmo-*":9,"gpt-4o-gizmo-sync-a":0}`,
				"billing_setting.billing_mode": `{"gpt-4o-gizmo-sync-expression":"tiered_expr"}`,
				"billing_setting.billing_expr": `{"gpt-4o-gizmo-sync-expression":"tier(\"base\", p * 2 + c * 8)"}`,
			}
			completion, err := common.Marshal(map[string]float64{shared: tc.completion})
			require.NoError(t, err)
			options["CompletionRatio"] = string(completion)
			if tc.option == "ModelRatio" {
				base, err = common.Marshal(map[string]float64{shared: tc.initial, first: 99, standalone: 3})
				require.NoError(t, err)
			}
			options[tc.option] = string(base)
			require.NoError(t, model.UpdateModelPricingOptions(options))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				price := tc.initial
				if r.URL.Path == "/selected" {
					price = 2
				}
				data := map[string]any{
					tc.field:       map[string]float64{first: price, selected: price},
					"billing_mode": map[string]string{expressionModel: "tiered_expr"},
					"billing_expr": map[string]string{expressionModel: expression},
				}
				if tc.field == "model_ratio" {
					data["model_ratio"] = map[string]float64{first: price, selected: price, standalone: 3}
					data["completion_ratio"] = map[string]float64{first: tc.completion, selected: tc.completion}
					data["audio_ratio"] = map[string]float64{first: 5, selected: 5}
					data["audio_completion_ratio"] = map[string]float64{first: 6, selected: 6}
				} else {
					data["model_ratio"] = map[string]float64{standalone: 3}
				}
				encoded, marshalErr := common.Marshal(gin.H{"success": true, "data": data})
				if marshalErr != nil {
					http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(encoded)
			}))
			defer server.Close()
			request := gin.H{"include_unchanged": true, "upstreams": []gin.H{
				{"name": "Initially same", "base_url": server.URL, "endpoint": "/same"},
				{"name": "Selected", "base_url": server.URL, "endpoint": "/selected"},
			}}
			for _, saved := range []bool{false, true} {
				if saved {
					snapshot, snapshotErr := model.GetModelPricingSnapshot([]string{selected})
					require.NoError(t, snapshotErr)
					pricing := snapshot.Entries[0].Configured
					pricing[tc.option] = float64(2)
					var response struct{ Success bool }
					recorder := modelManagementRequest(t, UpdateModelPricingConfig, http.MethodPatch, "/api/option/model_pricing", gin.H{"changes": []model.ModelPricingChange{{ModelName: selected, ExpectedVersion: snapshot.Entries[0].Version, Pricing: pricing}}}, &response)
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					require.True(t, response.Success, recorder.Body.String())
					stored, snapshotErr := model.GetModelPricingSnapshot([]string{first, selected})
					require.NoError(t, snapshotErr)
					for _, entry := range stored.Entries {
						assert.Equal(t, float64(2), entry.Configured[tc.option])
					}
				}
				var response struct {
					Success bool
					Data    struct {
						Differences map[string]map[string]dto.DifferenceItem
						Prices      map[string]struct {
							Current   map[string]any
							Upstreams map[string]map[string]any
						}
						TestResults []dto.TestResult `json:"test_results"`
					}
				}
				recorder := modelManagementRequest(t, FetchUpstreamRatios, http.MethodPost, "/api/ratio_sync/fetch", request, &response)
				require.True(t, response.Success, recorder.Body.String())
				require.Len(t, response.Data.TestResults, 2)
				for _, result := range response.Data.TestResults {
					require.Equal(t, "success", result.Status, result.Error)
				}
				current, source, upstream := tc.initial, "Selected", float64(2)
				if saved {
					current, source, upstream = 2, "Initially same", tc.initial
				}
				for _, name := range []string{first, selected} {
					row := response.Data.Prices[name]
					assert.Equal(t, current, row.Current[tc.field], "saved=%v model=%s", saved, name)
					item := response.Data.Differences[name][tc.field]
					assert.Equal(t, current, item.Current)
					assert.Equal(t, map[string]any{source: upstream}, item.Upstreams)
					confidence := true
					if saved {
						confidence = tc.confidence
					}
					assert.Equal(t, map[string]bool{source: confidence}, item.Confidence, "a formerly unchanged source keeps confidence after a shared-price update")
					assert.Equal(t, tc.initial, row.Upstreams["Initially same"][tc.field])
					assert.Equal(t, float64(2), row.Upstreams["Selected"][tc.field])
					if tc.field == "model_ratio" {
						assert.Equal(t, tc.completion, row.Current["completion_ratio"])
						assert.Equal(t, float64(5), row.Current["audio_ratio"])
						assert.Equal(t, float64(6), row.Current["audio_completion_ratio"])
						for _, field := range []string{"cache_ratio", "create_cache_ratio", "image_ratio"} {
							if name == first {
								assert.Equal(t, float64(0), row.Current[field])
							} else {
								assert.NotContains(t, row.Current, field, "exact fields must not inherit the wildcard")
							}
						}
					}
				}
				assert.Equal(t, map[string]any{"billing_mode": "ratio", "model_ratio": float64(3)}, response.Data.Prices[standalone].Current)
				assert.Equal(t, map[string]any{"billing_mode": "tiered_expr", "billing_expr": expression}, response.Data.Prices[expressionModel].Current)
				assert.NotContains(t, response.Data.Differences, expressionModel)
				exposed := ratio_setting.GetExposedData()
				assert.Equal(t, float64(99), valueMap(exposed[tc.field])[first], "projection must not mutate cached configuration")
				assert.NotContains(t, valueMap(exposed[tc.field]), selected)
			}
		})
	}
}
