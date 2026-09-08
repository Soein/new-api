package service

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"gorm.io/gorm"
)

const historicalTaskPluginCacheCapacity = 128

var historicalTaskPluginCache = struct {
	sync.Mutex
	entries map[string]*pluginruntime.LoadedPlugin
	order   []string
}{entries: make(map[string]*pluginruntime.LoadedPlugin)}

// TaskPluginAdmittedForNewRequest closes the cross-node registry sync window
// for database overrides before any untrusted plugin hook executes.
func TaskPluginAdmittedForNewRequest(plugin *pluginruntime.LoadedPlugin) bool {
	if plugin == nil {
		return false
	}
	if plugin.Layer != pluginruntime.PluginLayerOverride {
		return true
	}
	if model.DB == nil {
		return false
	}
	active, err := model.GetTaskPluginVersion(plugin.Meta.Key, "")
	return err == nil && active.Enabled && active.Version == plugin.Meta.Version && active.SourceHash == plugin.SourceHash
}

func taskPluginSnapshotMatches(plugin *pluginruntime.LoadedPlugin, snapshot *model.TaskPluginSnapshot, requireSourceHash bool) bool {
	if plugin == nil || snapshot == nil || plugin.Meta.Key != snapshot.Key || plugin.Meta.Version != snapshot.Version {
		return false
	}
	if snapshot.APIVersion != 0 && plugin.Meta.APIVersion != snapshot.APIVersion {
		return false
	}
	if requireSourceHash && snapshot.SourceHash == "" {
		return false
	}
	return snapshot.SourceHash == "" || plugin.SourceHash == snapshot.SourceHash
}

func loadHistoricalTaskPlugin(cacheKey string) (*pluginruntime.LoadedPlugin, bool) {
	historicalTaskPluginCache.Lock()
	defer historicalTaskPluginCache.Unlock()
	plugin, ok := historicalTaskPluginCache.entries[cacheKey]
	return plugin, ok
}

func cacheHistoricalTaskPlugin(cacheKey string, plugin *pluginruntime.LoadedPlugin) *pluginruntime.LoadedPlugin {
	historicalTaskPluginCache.Lock()
	defer historicalTaskPluginCache.Unlock()
	if cached := historicalTaskPluginCache.entries[cacheKey]; cached != nil {
		return cached
	}
	if len(historicalTaskPluginCache.order) == historicalTaskPluginCacheCapacity {
		delete(historicalTaskPluginCache.entries, historicalTaskPluginCache.order[0])
		historicalTaskPluginCache.order = historicalTaskPluginCache.order[1:]
	}
	historicalTaskPluginCache.entries[cacheKey] = plugin
	historicalTaskPluginCache.order = append(historicalTaskPluginCache.order, cacheKey)
	return plugin
}

func restoreHistoricalOverrideTaskPlugin(snapshot *model.TaskPluginSnapshot, override *model.TaskPlugin, requireSourceHash bool) *pluginruntime.LoadedPlugin {
	if !pluginruntime.DefaultRegistry.Enabled() || override == nil || !override.Enabled {
		return nil
	}
	if requireSourceHash && (snapshot.SourceHash == "" || override.SourceHash != snapshot.SourceHash) {
		return nil
	}
	active, err := model.GetTaskPluginVersion(snapshot.Key, "")
	if err == nil && !active.Enabled {
		return nil
	}
	if err != nil && (!errors.Is(err, gorm.ErrRecordNotFound) || override.DeletedAt == nil) {
		return nil
	}

	cacheKey := fmt.Sprintf("%s\x00%s\x00%s", override.Key, override.Version, override.SourceHash)
	if plugin, ok := loadHistoricalTaskPlugin(cacheKey); ok {
		if taskPluginSnapshotMatches(plugin, snapshot, requireSourceHash) {
			return plugin
		}
		return nil
	}
	plugin, compileErr := pluginruntime.CompilePlugin(override.Source, pluginruntime.Options{
		Key: snapshot.Key, Version: snapshot.Version,
	})
	if compileErr != nil || plugin.SourceHash != override.SourceHash || !taskPluginSnapshotMatches(plugin, snapshot, requireSourceHash) {
		logger.LogError(context.Background(), fmt.Sprintf(
			"Exact task plugin %s@%s could not be restored from historical source",
			snapshot.Key, snapshot.Version,
		))
		return nil
	}
	plugin.Layer = pluginruntime.PluginLayerOverride
	return cacheHistoricalTaskPlugin(cacheKey, plugin)
}

// ResolveExactTaskPluginForTask restores the immutable plugin source identity
// captured in a task snapshot. Tasks without a snapshot are deliberately left
// to their caller's legacy compatibility policy.
func ResolveExactTaskPluginForTask(task *model.Task) (*pluginruntime.LoadedPlugin, *pluginruntime.RoutingGeneration, bool) {
	if task == nil || task.PrivateData.Execution == nil || task.PrivateData.Execution.TaskPlugin == nil {
		return nil, nil, false
	}
	snapshot := task.PrivateData.Execution.TaskPlugin
	if snapshot.Key == "" || snapshot.Version == "" {
		return nil, nil, false
	}
	generation := &pluginruntime.RoutingGeneration{Number: snapshot.Generation}

	switch snapshot.Layer {
	case pluginruntime.PluginLayerFactory:
		if !pluginruntime.DefaultRegistry.FactoryEnabled(snapshot.Key) {
			return nil, generation, false
		}
		plugin, ok := pluginruntime.DefaultRegistry.FactoryPluginVersion(snapshot.Key, snapshot.Version)
		if !ok || plugin.Layer != pluginruntime.PluginLayerFactory || !taskPluginSnapshotMatches(plugin, snapshot, true) {
			return nil, generation, false
		}
		return plugin, generation, true
	case pluginruntime.PluginLayerOverride:
		override, err := model.GetTaskPluginVersionForExecution(snapshot.Key, snapshot.Version)
		if err != nil {
			logger.LogError(context.Background(), fmt.Sprintf(
				"Exact task plugin %s@%s lookup failed: %v",
				snapshot.Key, snapshot.Version, err,
			))
			return nil, generation, false
		}
		plugin := restoreHistoricalOverrideTaskPlugin(snapshot, override, true)
		return plugin, generation, plugin != nil
	case "":
		// Legacy rows predate layer and source-hash pinning. They can be
		// restored only when key+version identifies exactly one source layer.
	default:
		return nil, generation, false
	}

	override, err := model.GetTaskPluginVersionForExecution(snapshot.Key, snapshot.Version)
	if err == nil {
		if _, ok := pluginruntime.DefaultRegistry.FactoryPluginVersion(snapshot.Key, snapshot.Version); ok {
			logger.LogWarn(context.Background(), fmt.Sprintf(
				"Legacy task plugin %s@%s has ambiguous factory and override sources; refusing to execute",
				snapshot.Key, snapshot.Version,
			))
			return nil, generation, false
		}
		plugin := restoreHistoricalOverrideTaskPlugin(snapshot, override, false)
		return plugin, generation, plugin != nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.LogError(context.Background(), fmt.Sprintf(
			"Exact task plugin %s@%s lookup failed: %v",
			snapshot.Key, snapshot.Version, err,
		))
		return nil, generation, false
	}

	plugin, ok := pluginruntime.DefaultRegistry.FactoryPluginVersion(snapshot.Key, snapshot.Version)
	if !pluginruntime.DefaultRegistry.FactoryEnabled(snapshot.Key) || !ok || plugin.Layer != pluginruntime.PluginLayerFactory || !taskPluginSnapshotMatches(plugin, snapshot, false) {
		logger.LogWarn(context.Background(), fmt.Sprintf(
			"Exact task plugin %s@%s is unavailable; refusing to use the current active version",
			snapshot.Key, snapshot.Version,
		))
		return nil, generation, false
	}
	return plugin, generation, true
}
