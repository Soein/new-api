package model

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	filterdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestChannelValidateSettingsRejectsInvalidHTTPTransport(t *testing.T) {
	tests := []struct {
		name    string
		setting dto.ChannelSettings
		wantErr string
	}{
		{
			name:    "auto with shards is valid",
			setting: dto.ChannelSettings{HTTPProtocol: "auto", HTTP2ConnectionShards: 4},
		},
		{
			name:    "http1 with shards greater than one rejected",
			setting: dto.ChannelSettings{HTTPProtocol: "http1", HTTP2ConnectionShards: 2},
			wantErr: "http2_connection_shards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{}
			channel.SetSetting(tt.setting)
			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestAdvancedCustomChannelRequiresModelListRouteOnlyWhenUpdateChecksEnabled(t *testing.T) {
	inferenceRoute := dto.AdvancedCustomRoute{
		IncomingPath: "/v1/chat/completions",
		UpstreamPath: "/v1/chat/completions",
		Converter:    "none",
	}

	tests := []struct {
		name          string
		checksEnabled bool
		routes        []dto.AdvancedCustomRoute
		wantErr       string
	}{
		{
			name:   "legacy channel without discovery route remains valid",
			routes: []dto.AdvancedCustomRoute{inferenceRoute},
		},
		{
			name:          "enabled checks require discovery route",
			checksEnabled: true,
			routes:        []dto.AdvancedCustomRoute{inferenceRoute},
			wantErr:       dto.AdvancedCustomModelListPath,
		},
		{
			name:          "enabled checks accept discovery route",
			checksEnabled: true,
			routes: []dto.AdvancedCustomRoute{
				inferenceRoute,
				{
					IncomingPath: dto.AdvancedCustomModelListPath,
					UpstreamPath: dto.AdvancedCustomModelListPath,
					Converter:    "none",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{Type: constant.ChannelTypeAdvancedCustom}
			channel.SetOtherSettings(dto.ChannelOtherSettings{
				UpstreamModelUpdateCheckEnabled: tt.checksEnabled,
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: tt.routes,
				},
			})

			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestInferencePresetSettingsAndDatabaseRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "presets.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			table := db.Table("inference_preset_channels").Session(&gorm.Session{})
			require.NoError(t, table.AutoMigrate(&Channel{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable("inference_preset_channels")) })
			var version string
			if dialect == "sqlite" {
				require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
			} else {
				require.NoError(t, db.Raw("select version()").Scan(&version).Error)
			}
			t.Logf("%s version: %s", dialect, version)
			for _, channelType := range []int{constant.ChannelTypeVLLM, constant.ChannelTypeSGLang} {
				t.Run(fmt.Sprint(channelType), func(t *testing.T) {
					channel := &Channel{Type: channelType, Key: "EMPTY", Name: "inference", Status: common.ChannelStatusEnabled}
					require.NoError(t, channel.ValidateSettings())
					require.NotNil(t, channel.GetOtherSettings().AdvancedCustom)
					require.NoError(t, table.Create(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						assert.Equal(t, channelType, loaded.Type)
						assert.Empty(t, loaded.OtherSettings)
						defaults := loaded.GetOtherSettings().AdvancedCustom
						require.NotNil(t, defaults)
						assert.True(t, defaults.SupportsPath("/v1/messages"))
						assert.Empty(t, loaded.OtherSettings, "reading defaults must not rewrite saved settings")
						defaults.Routes[0].UpstreamPath = "/changed-locally"
						assert.Equal(t, "/v1/chat/completions", loaded.GetOtherSettings().AdvancedCustom.Routes[0].UpstreamPath)
					}
					// 兼容旧数据：更新渠道 setting 时不得将运行时推理预设回写至 channels.settings
					var loadedForSetting Channel
					require.NoError(t, table.First(&loadedForSetting, channel.Id).Error)
					loadedSetting := loadedForSetting.GetSetting()
					loadedSetting.ResponsesWebSocketEnabled = true
					loadedSetting.ResponseTimeThresholdSec = 2.0
					loadedForSetting.SetSetting(loadedSetting)
					require.NoError(t, table.Save(&loadedForSetting).Error)

					var reloadedForSetting Channel
					require.NoError(t, table.First(&reloadedForSetting, channel.Id).Error)
					assert.Empty(t, reloadedForSetting.OtherSettings, "updating channel.setting must not write back runtime preset to other_settings")
					assert.True(t, reloadedForSetting.GetSetting().ResponsesWebSocketEnabled)
					assert.Equal(t, 2.0, reloadedForSetting.GetSetting().ResponseTimeThresholdSec)

					settings := dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/chat/completions", UpstreamPath: "/custom/chat", Models: []string{"allowed"}, Auth: &dto.AdvancedCustomRouteAuth{Type: "none"}}}}}
					channel.SetOtherSettings(settings)
					require.NoError(t, channel.ValidateSettings())
					require.NoError(t, table.Save(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						actual := loaded.GetOtherSettings().AdvancedCustom
						require.Equal(t, common.GetAdvancedCustomPreset(channelType), actual, "named channels must ignore editable advanced_custom overrides")
						assert.Equal(t, channel.OtherSettings, loaded.OtherSettings)
						for _, tc := range []struct {
							path, model string
							allowed     bool
						}{
							{"/v1/chat/completions", "allowed", true},
							{"/v1/chat/completions", "other", true},
							{"/v1/messages", "allowed", true},
							{"/v1/images/generations", "allowed", false},
						} {
							ok, _ := ChannelSatisfiesFilters(&loaded, tc.model, []filterdto.ChannelFilter{{Kind: filterdto.FilterRequestPath, RequestPath: tc.path}})
							assert.Equal(t, tc.allowed, ok)
						}
						endpoints := getPricingEndpointTypesForAbility(AbilityWithChannel{ChannelType: channelType, Ability: Ability{Model: "allowed", ChannelId: loaded.Id}}, map[int]*dto.AdvancedCustomConfig{loaded.Id: actual})
						expectedEndpoints := []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse, constant.EndpointTypeAnthropic, constant.EndpointTypeEmbeddings}
						if channelType == constant.ChannelTypeSGLang {
							expectedEndpoints = append(expectedEndpoints, constant.EndpointTypeJinaRerank)
						}
						assert.ElementsMatch(t, expectedEndpoints, endpoints)
					}
				})
			}
			t.Run("incremental_channel_settings_compatibility", func(t *testing.T) {
				// 兼容旧数据：旧 JSON 未显式设置 responses_websocket_enabled 与 ollama_openai_chat 时均解析为 false
				legacySettingJSON := `{"proxy":"http://proxy.internal:8080","response_time_threshold_sec":3.5}`
				legacyOtherSettingsJSON := `{"azure_responses_version":"2024-05-01-preview"}`
				compatChannel := &Channel{
					Type:          constant.ChannelTypeOllama,
					Key:           "EMPTY",
					Name:          "compat-channel",
					Status:        common.ChannelStatusEnabled,
					Setting:       common.GetPointer(legacySettingJSON),
					OtherSettings: legacyOtherSettingsJSON,
				}
				require.NoError(t, table.Create(compatChannel).Error)

				var loadedLegacy Channel
				require.NoError(t, table.First(&loadedLegacy, compatChannel.Id).Error)
				assert.False(t, loadedLegacy.GetSetting().ResponsesWebSocketEnabled, "legacy setting without responses_websocket_enabled must default to false")
				assert.False(t, loadedLegacy.GetOtherSettings().OllamaOpenAIChat, "legacy settings without ollama_openai_chat must default to false")
				assert.Equal(t, 3.5, loadedLegacy.GetSetting().ResponseTimeThresholdSec, "local response_time_threshold_sec must be preserved")
				assert.Equal(t, "http://proxy.internal:8080", loadedLegacy.GetSetting().Proxy)
				assert.Equal(t, "2024-05-01-preview", loadedLegacy.GetOtherSettings().AzureResponsesVersion)

				// true 保存重读：WS 设置位于 channels.setting，Ollama 设置位于 channels.settings，且 ResponseTimeThresholdSec 保持
				setting := loadedLegacy.GetSetting()
				setting.ResponsesWebSocketEnabled = true
				loadedLegacy.SetSetting(setting)

				otherSetting := loadedLegacy.GetOtherSettings()
				otherSetting.OllamaOpenAIChat = true
				loadedLegacy.SetOtherSettings(otherSetting)

				require.NoError(t, table.Save(&loadedLegacy).Error)

				var loadedTrue Channel
				require.NoError(t, table.First(&loadedTrue, compatChannel.Id).Error)
				require.NotNil(t, loadedTrue.Setting)
				assert.Contains(t, *loadedTrue.Setting, `"responses_websocket_enabled":true`, "responses_websocket_enabled must be stored in channels.setting")
				assert.Contains(t, loadedTrue.OtherSettings, `"ollama_openai_chat":true`, "ollama_openai_chat must be stored in channels.settings")
				assert.True(t, loadedTrue.GetSetting().ResponsesWebSocketEnabled)
				assert.True(t, loadedTrue.GetOtherSettings().OllamaOpenAIChat)
				assert.Equal(t, 3.5, loadedTrue.GetSetting().ResponseTimeThresholdSec)

				// 再关闭后为 false 重读，且本地 ResponseTimeThresholdSec 保持
				setting = loadedTrue.GetSetting()
				setting.ResponsesWebSocketEnabled = false
				loadedTrue.SetSetting(setting)

				otherSetting = loadedTrue.GetOtherSettings()
				otherSetting.OllamaOpenAIChat = false
				loadedTrue.SetOtherSettings(otherSetting)

				require.NoError(t, table.Save(&loadedTrue).Error)

				var loadedFalse Channel
				require.NoError(t, table.First(&loadedFalse, compatChannel.Id).Error)
				assert.False(t, loadedFalse.GetSetting().ResponsesWebSocketEnabled)
				assert.False(t, loadedFalse.GetOtherSettings().OllamaOpenAIChat)
				assert.Equal(t, 3.5, loadedFalse.GetSetting().ResponseTimeThresholdSec)
			})
		})
	}
}
