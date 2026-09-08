package relay

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	jspluginadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetTaskAdaptorMapsMigratedPlatformsToFactoryPlugins(t *testing.T) {
	platforms := []constant.TaskPlatform{
		constant.TaskPlatformSuno,
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeAli)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeDoubaoVideo)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeVolcEngine)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeGemini)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeMiniMax)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeJimeng)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeKling)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeVidu)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeSora)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeOpenAI)),
		constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeVertexAi)),
	}
	for _, platform := range platforms {
		_, isJS := GetTaskAdaptor(platform).(*jspluginadaptor.TaskAdaptor)
		assert.True(t, isJS, "platform %s should use its factory plugin", platform)
	}
}

func TestGetTaskAdaptorUsesPlatformAsThirdPartyPluginKey(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })
	t.Cleanup(func() {
		pluginruntime.DefaultRegistry.Unregister("registry-fallback")
	})
	source := `
export const meta = {apiVersion: 1, key: "registry-fallback", name: "Registry Fallback", version: "1.0.0", author: {name: "Test"}, channelTypes: [1999], models: ["fallback-v1"], fetchMode: "per_task"};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/submit"}; }
export function parseSubmitResponse(ctx, resp) { return {taskId: "id", taskData: resp.body}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/tasks/" + ctx.taskId}; }
export function parseTaskResult(ctx, body) { return {taskId: body.id, status: "SUCCESS"}; }
`
	loaded, err := pluginruntime.DefaultRegistry.Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: loaded.Meta.Key, APIVersion: loaded.Meta.APIVersion, Version: loaded.Meta.Version,
		Source: source, SourceHash: loaded.SourceHash, Enabled: true,
	}))

	adaptor := GetTaskAdaptor(constant.TaskPlatform("registry-fallback"))
	require.NotNil(t, adaptor)
	assert.Equal(t, "Registry Fallback", adaptor.GetChannelName())
}

func TestGetTaskAdaptorReturnsNilForUnknownPlatform(t *testing.T) {
	assert.Nil(t, GetTaskAdaptor(constant.TaskPlatform("missing-task-platform")))
}

func TestGetTaskAdaptorForTaskRestoresPinnedOverrideVersion(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	const key = "historical-adaptor"
	t.Cleanup(func() { _ = pluginruntime.DefaultRegistry.Unregister(key) })
	pluginSource := func(version, name string) string {
		return `
export const meta = {apiVersion: 1, key: "` + key + `", name: "` + name + `", version: "` + version + `", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task", protocols: ["openai_video"]};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
export function listArtifacts() { return []; }
export function buildContentRequest() { return {}; }
export const protocols = {openai_video: {decodeRequest: function(ctx) { return ctx; }, render: function() { return {metadata: {renderer: "` + name + `"}}; }}};
`
	}
	v1Source := pluginSource("1.0.0", "Historical V1")
	v2Source := pluginSource("2.0.0", "Current V2")
	v1Hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v1Source)))
	v2Hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v2Source)))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0", Source: v1Source, SourceHash: v1Hash, Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "2.0.0", Source: v2Source, SourceHash: v2Hash, Enabled: true,
	}))
	require.NoError(t, model.ActivateTaskPlugin(key, "2.0.0"))
	_, err = pluginruntime.DefaultRegistry.Register(v2Source, pluginruntime.Options{Key: key, Version: "2.0.0"})
	require.NoError(t, err)
	_, err = model.DeleteTaskPluginVersion(key, "1.0.0")
	require.NoError(t, err)

	task := &model.Task{Platform: constant.TaskPlatform(key)}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: key, Version: "1.0.0", APIVersion: 1,
	}}

	adaptor := GetTaskAdaptorForTask(task)
	require.NotNil(t, adaptor)
	assert.Equal(t, "Historical V1", adaptor.GetChannelName())
	converter, ok := adaptor.(channel.OpenAIVideoConverter)
	require.True(t, ok)
	rendered, err := converter.ConvertToOpenAIVideo(task)
	require.NoError(t, err)
	assert.Contains(t, string(rendered), `"renderer":"Historical V1"`)
	task.PrivateData.Execution.TaskPlugin.APIVersion = 0
	assert.NotNil(t, GetTaskAdaptorForTask(task), "legacy snapshots without api_version still pin by key and version")
	_, err = model.DeleteTaskPluginVersion(key, "1.0.0", true)
	require.NoError(t, err)
	assert.Nil(t, GetTaskAdaptorForTask(task), "force-deleting a tombstone must permanently revoke historical execution")

	task.PrivateData.Execution.TaskPlugin.Version = "0.0.0-unavailable"
	assert.Nil(t, GetTaskAdaptorForTask(task), "missing historical source must fail closed instead of using the active version")
}

func TestGetTaskAdaptorForTaskRejectsDisabledHistoricalOverride(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	const key = "revoked-historical-adaptor"
	pluginSource := func(version, name string) string {
		return `
export const meta = {apiVersion: 1, key: "` + key + `", name: "` + name + `", version: "` + version + `", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	}
	v1Source := pluginSource("1.0.0", "Historical V1")
	v2Source := pluginSource("2.0.0", "Current V2")
	v1Hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v1Source)))
	v2Hash := fmt.Sprintf("%x", sha256.Sum256([]byte(v2Source)))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "1.0.0", Source: v1Source, SourceHash: v1Hash, Enabled: true,
	}))
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "2.0.0", Source: v2Source, SourceHash: v2Hash, Enabled: true,
	}))
	require.NoError(t, model.ActivateTaskPlugin(key, "2.0.0"))
	require.NoError(t, model.SetTaskPluginEnabled(key, false))

	task := &model.Task{Platform: constant.TaskPlatform(key)}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: key, Version: "1.0.0", APIVersion: 1, Layer: pluginruntime.PluginLayerOverride, SourceHash: v1Hash,
	}}

	assert.Nil(t, GetTaskAdaptorForTask(task), "disabling the active override must revoke historical executions for the key")
}

func TestGetTaskAdaptorForTaskKeepsFactoryIdentityWhenSameVersionOverrideExists(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	const key = "google"
	factorySource, err := plugins.Source(key)
	require.NoError(t, err)
	factory, ok := pluginruntime.DefaultRegistry.Snapshot().Factory, false
	var factoryName, factoryVersion string
	for _, meta := range factory {
		if meta.Key == key {
			ok = true
			factoryName = meta.Name
			factoryVersion = meta.Version
			break
		}
	}
	require.True(t, ok)
	factoryHash := fmt.Sprintf("%x", sha256.Sum256([]byte(factorySource)))
	overrideSource := `
export const meta = {apiVersion: 1, key: "google", name: "Injected Override", version: "` + factoryVersion + `", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: factoryVersion, Source: overrideSource,
		SourceHash: fmt.Sprintf("%x", sha256.Sum256([]byte(overrideSource))), Enabled: true,
	}))

	task := &model.Task{Platform: constant.TaskPlatform(key)}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: key, Version: factoryVersion, APIVersion: 1,
		Layer: pluginruntime.PluginLayerFactory, SourceHash: factoryHash,
	}}
	adaptor := GetTaskAdaptorForTask(task)
	require.NotNil(t, adaptor)
	assert.Equal(t, factoryName, adaptor.GetChannelName())

	task.PrivateData.Execution.TaskPlugin.Layer = ""
	task.PrivateData.Execution.TaskPlugin.SourceHash = ""
	assert.Nil(t, GetTaskAdaptorForTask(task), "legacy snapshots must fail closed when factory and override identities are ambiguous")
}

func TestGetTaskAdaptorForTaskRestoresLegacyFactoryWhenOverrideVersionDiffers(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	const key = "google"
	var factoryName, factoryVersion string
	for _, meta := range pluginruntime.DefaultRegistry.Snapshot().Factory {
		if meta.Key == key {
			factoryName = meta.Name
			factoryVersion = meta.Version
			break
		}
	}
	require.NotEmpty(t, factoryVersion)
	overrideSource := `
export const meta = {apiVersion: 1, key: "google", name: "Different Override", version: "999.0.0", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: key, APIVersion: 1, Version: "999.0.0", Source: overrideSource,
		SourceHash: fmt.Sprintf("%x", sha256.Sum256([]byte(overrideSource))), Enabled: true,
	}))

	task := &model.Task{Platform: constant.TaskPlatform(key)}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: key, Version: factoryVersion, APIVersion: 1,
	}}
	adaptor := GetTaskAdaptorForTask(task)
	require.NotNil(t, adaptor)
	assert.Equal(t, factoryName, adaptor.GetChannelName())
}

func TestGetTaskAdaptorForTaskFailsClosedWhenHistoricalFactoryVersionIsUnavailable(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	require.NotNil(t, GetTaskAdaptor(constant.TaskPlatform("google")), "the current factory plugin must be available for the control assertion")
	task := &model.Task{Platform: constant.TaskPlatform("google")}
	task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{
		Key: "google", Version: "0.9.0-removed", APIVersion: 1,
	}}

	assert.Nil(t, GetTaskAdaptorForTask(task), "a removed factory source must never fall forward to the active factory version")
}

func TestGetTaskAdaptorForTaskRestoresArchivedFactoryIdentity(t *testing.T) {
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })

	for _, test := range []struct {
		key, hash string
	}{
		{"alibaba", "336dc982f878047fa1c9dd8a31280964f9f44e158020a95c26dbbdc6e9501676"},
		{"sunoapi", "4d5d1253b051202a31b1432f8888a7eef19957ccdd908ecf2b503b9759111930"},
	} {
		t.Run(test.key, func(t *testing.T) {
			registry := pluginruntime.DefaultRegistry
			active, ok := registry.Get(test.key)
			require.True(t, ok)
			require.NotEqual(t, "1.0.1", active.Meta.Version)
			task := &model.Task{Platform: constant.TaskPlatform(test.key)}
			snapshot := &model.TaskPluginSnapshot{
				Key: test.key, Version: "1.0.1", APIVersion: 1, Generation: 42,
				Layer: pluginruntime.PluginLayerFactory, SourceHash: test.hash,
			}
			task.PrivateData.Execution = &model.TaskExecutionSnapshot{TaskPlugin: snapshot}
			require.NotNil(t, GetTaskAdaptorForTask(task), "upgrades must retain the original factory task adaptor")
			restored, generation, ok := service.ResolveExactTaskPluginForTask(task)
			require.True(t, ok)
			assert.Equal(t, test.hash, restored.SourceHash)
			assert.Equal(t, pluginruntime.PluginLayerFactory, restored.Layer)
			assert.Equal(t, uint64(42), generation.Number)
			current, ok := registry.Get(test.key)
			require.True(t, ok)
			assert.Same(t, active, current, "historical execution must not replace new-request routing")

			for _, hash := range []string{"", active.SourceHash} {
				snapshot.SourceHash = hash
				assert.Nil(t, GetTaskAdaptorForTask(task), "a factory version requires its exact original source hash")
			}
			snapshot.SourceHash = test.hash
			snapshot.APIVersion = 2
			assert.Nil(t, GetTaskAdaptorForTask(task))
			snapshot.APIVersion = 1

			previousDisabled := registry.Snapshot().DisabledFactory
			t.Cleanup(func() { registry.SetDisabledFactoryKeys(previousDisabled); registry.SetEnabled(true) })
			registry.SetDisabledFactoryKeys(append(append([]string(nil), previousDisabled...), test.key))
			assert.Nil(t, GetTaskAdaptorForTask(task), "disabling a factory must also stop its history")
			registry.SetDisabledFactoryKeys(previousDisabled)
			registry.SetEnabled(false)
			assert.Nil(t, GetTaskAdaptorForTask(task), "the master switch must also stop historical execution")
			registry.SetEnabled(true)

			snapshot.Layer, snapshot.SourceHash = "", ""
			require.NotNil(t, GetTaskAdaptorForTask(task), "unambiguous legacy snapshots may restore archived factories")
			source := `export const meta = {apiVersion: 1, key: "` + test.key + `", name: "Override", version: "1.0.1", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }`
			require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
				Key: test.key, Version: "1.0.1", APIVersion: 1, Enabled: true,
				Source: source, SourceHash: fmt.Sprintf("%x", sha256.Sum256([]byte(source))),
			}))
			assert.Nil(t, GetTaskAdaptorForTask(task), "legacy factory/override ambiguity must include archived versions")
			snapshot.Layer, snapshot.SourceHash = pluginruntime.PluginLayerFactory, test.hash
			require.NotNil(t, GetTaskAdaptorForTask(task), "a same-version override must not mask a pinned factory")
		})
	}
}

func TestGetTaskAdaptorForRequestUsesExactPinnedPlugin(t *testing.T) {
	source := `
export const meta = {apiVersion: 1, key: "pinned-request", name: "Pinned Generation", version: "1.0.0", author: {name: "Test"}, models: ["pinned-v1"], fetchMode: "per_task"};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/submit"}; }
export function parseSubmitResponse(ctx) { return {taskId: "one"}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/query"}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	pinned, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: pinned.Meta.Key, APIVersion: pinned.Meta.APIVersion, Version: pinned.Meta.Version,
		Source: source, SourceHash: pinned.SourceHash, Enabled: true,
	}))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/vendor/submit", nil)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: pinned})

	platform, adaptor := getTaskAdaptorForRequest(c, constant.TaskPlatform("missing-task-platform"))
	require.NotNil(t, adaptor)
	assert.Equal(t, constant.TaskPlatform("pinned-request"), platform)
	assert.Equal(t, "Pinned Generation", adaptor.GetChannelName())
}

func TestGetTaskAdaptorForRequestCanonicalizesNativePins(t *testing.T) {
	source := `
export const meta = {apiVersion: 1, key: "native-pin", name: "Native Pin", version: "1.0.0", author: {name: "Test"}, models: ["native-v1"], fetchMode: "per_task", usageSchema: {seconds: {type: "number", unit: "second"}}};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl + "/submit"}; }
export function parseSubmitResponse() { return {taskId: "one"}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl + "/query"}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	pinned, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	originalDB := model.DB
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.TaskPluginState{}, &model.TaskPlugin{}))
	model.DB = database
	t.Cleanup(func() { model.DB = originalDB })
	require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{
		Key: pinned.Meta.Key, APIVersion: pinned.Meta.APIVersion, Version: pinned.Meta.Version,
		Source: source, SourceHash: pinned.SourceHash, Enabled: true,
	}))
	generation := &pluginruntime.RoutingGeneration{Number: 77}

	tests := []struct {
		name string
		set  func(*gin.Context)
	}{
		{
			name: "route",
			set: func(c *gin.Context) {
				c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: pinned})
			},
		},
		{
			name: "endpoint",
			set: func(c *gin.Context) {
				c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{Generation: generation, Plugin: pinned})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/native/submit", nil)
			test.set(c)

			platform, adaptor := getTaskAdaptorForRequest(c, constant.TaskPlatform("missing"))
			require.NotNil(t, adaptor)
			assert.Equal(t, constant.TaskPlatform("native-pin"), platform)
			canonicalValue, exists := c.Get(pluginruntime.ContextKeyPinnedPlugin)
			require.True(t, exists)
			canonical, ok := canonicalValue.(pluginruntime.PinnedPlugin)
			require.True(t, ok)
			assert.Same(t, generation, canonical.Generation)
			assert.Same(t, pinned, canonical.Plugin)
		})
	}

	_, err = model.DeleteTaskPluginVersion(pinned.Meta.Key, pinned.Meta.Version)
	require.NoError(t, err)
	staleContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	staleContext.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: generation,
		Plugin:     pinned,
	})
	_, adaptor := getTaskAdaptorForRequest(staleContext, constant.TaskPlatform("native-pin"))
	assert.Nil(t, adaptor, "a stale registry generation must not admit a tombstoned override")
}

func TestGetTaskAdaptorForRequestPinsLegacyMappedPlugin(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/video_1/remix", nil)
	legacyPlatform := constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeSora))

	platform, adaptor := getTaskAdaptorForRequest(c, legacyPlatform)

	require.NotNil(t, adaptor)
	assert.Equal(t, legacyPlatform, platform)
	pinnedValue, exists := c.Get(pluginruntime.ContextKeyPinnedPlugin)
	require.True(t, exists)
	pinned, ok := pinnedValue.(pluginruntime.PinnedPlugin)
	require.True(t, ok)
	require.NotNil(t, pinned.Generation)
	require.NotNil(t, pinned.Plugin)
	assert.Equal(t, "sora", pinned.Plugin.Meta.Key)
	assert.Same(t, pinned.Generation, pluginruntime.DefaultRegistry.Generation())
}
