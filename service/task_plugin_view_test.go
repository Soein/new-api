package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

func TestBuildTaskPluginViewRewritesOnlyStructuredTaskIDFields(t *testing.T) {
	const (
		privateTaskID = "upstream-task-123"
		publicTaskID  = "task_public_123"
		resultURL     = "https://cdn.example.com/results/upstream-task-123/video.mp4"
	)

	taskData, err := common.Marshal(map[string]any{
		"task_id": privateTaskID,
		"id":      privateTaskID,
		"taskId":  privateTaskID,
		"url":     resultURL,
		"message": "completed upstream-task-123",
		"nested": []any{
			map[string]any{
				"task_id": privateTaskID,
				"url":     resultURL,
			},
			privateTaskID,
		},
		privateTaskID: "opaque map key",
	})
	require.NoError(t, err)
	task := &model.Task{
		TaskID: publicTaskID,
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: privateTaskID,
		},
		Data: taskData,
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)

	data, ok := view.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, data["task_id"])
	assert.Equal(t, publicTaskID, data["id"])
	assert.Equal(t, publicTaskID, data["taskId"])
	assert.Equal(t, resultURL, data["url"])
	assert.Equal(t, "completed upstream-task-123", data["message"])
	assert.Equal(t, "opaque map key", data[privateTaskID])

	nested, ok := data["nested"].([]any)
	require.True(t, ok)
	nestedData, ok := nested[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, nestedData["task_id"])
	assert.Equal(t, resultURL, nestedData["url"])
	assert.Equal(t, privateTaskID, nested[1])

}

func TestTaskExecutionSnapshotCapturesExactPluginSourceIdentity(t *testing.T) {
	const source = `
export const meta = {apiVersion: 1, key: "snapshot-identity", name: "Snapshot Identity", version: "1.0.0", author: {name: "Test"}, models: ["doc"], fetchMode: "per_task"};
export function buildSubmitRequest() { return {}; }
export function parseSubmitResponse() { return {}; }
export function buildQueryRequest() { return {}; }
export function parseTaskResult() { return {status: "SUCCESS"}; }
`
	registry := pluginruntime.NewRegistry()
	plugin, err := registry.RegisterFactory(source, pluginruntime.Options{})
	require.NoError(t, err)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	ctx.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{
		Generation: registry.Generation(),
		Plugin:     plugin,
	})

	snapshot := TaskExecutionSnapshotFromContext(ctx)
	require.NotNil(t, snapshot)
	require.NotNil(t, snapshot.TaskPlugin)
	assert.Equal(t, pluginruntime.PluginLayerFactory, snapshot.TaskPlugin.Layer)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(source))), snapshot.TaskPlugin.SourceHash)
}

func TestBuildTaskPluginViewOmitsPrivatePollState(t *testing.T) {
	task := &model.Task{
		TaskID: "task_public_view",
		Data:   []byte(`{"ok":true}`),
		PrivateData: model.TaskPrivateData{
			PluginState:  []byte(`{"req_key":"secret"}`),
			PollFailures: 7,
		},
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	assert.NotContains(t, payload, "plugin_state")
	assert.NotContains(t, payload, "poll_failures")
	assert.NotContains(t, payload, "private_data")
}
