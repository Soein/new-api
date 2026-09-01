package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TaskPluginChannelRef struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

func GetTaskPluginUsage(key string) ([]TaskPluginChannelRef, int64, error) {
	var channels []Channel
	if err := DB.Where("type = ? AND status = ?", constant.ChannelTypeTaskPlugin, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, 0, err
	}
	refs := make([]TaskPluginChannelRef, 0)
	for _, channel := range channels {
		if channel.GetSetting().TaskPluginKey == key {
			refs = append(refs, TaskPluginChannelRef{Id: channel.Id, Name: channel.Name})
		}
	}
	var inFlight int64
	err := DB.Model(&Task{}).Where("platform = ? AND status NOT IN ?", key, []TaskStatus{TaskStatusSuccess, TaskStatusFailure}).Count(&inFlight).Error
	return refs, inFlight, err
}

// CountUnfinishedTasksUsingTaskPluginVersion counts durable task snapshots
// that still need this exact override source for polling and settlement. The
// filtering is intentionally done in Go because private_data JSON operators
// differ across SQLite, MySQL, and PostgreSQL.
func CountUnfinishedTasksUsingTaskPluginVersion(plugin *TaskPlugin) (int64, error) {
	if plugin == nil {
		return 0, errors.New("task plugin is required")
	}
	var count int64
	var cursor int64
	for {
		var tasks []Task
		err := DB.
			Select("id", "private_data").
			Where("id > ? AND platform = ? AND status NOT IN ?", cursor, plugin.Key, []TaskStatus{TaskStatusSuccess, TaskStatusFailure}).
			Order("id").
			Limit(256).
			Find(&tasks).Error
		if err != nil {
			return 0, err
		}
		for i := range tasks {
			execution := tasks[i].PrivateData.Execution
			if execution == nil || execution.TaskPlugin == nil {
				continue
			}
			snapshot := execution.TaskPlugin
			if snapshot.Key != plugin.Key || snapshot.Version != plugin.Version {
				continue
			}
			switch snapshot.Layer {
			case "override":
				if snapshot.SourceHash == plugin.SourceHash {
					count++
				}
			case "":
				if snapshot.SourceHash == "" || snapshot.SourceHash == plugin.SourceHash {
					count++
				}
			}
		}
		if len(tasks) < 256 {
			break
		}
		cursor = tasks[len(tasks)-1].ID
	}
	return count, nil
}

type TaskPlugin struct {
	Id         int64  `json:"id"`
	Key        string `json:"key" gorm:"size:128;not null;uniqueIndex:uk_task_plugin_key_version,priority:1"`
	APIVersion int    `json:"api_version" gorm:"not null"`
	Version    string `json:"version" gorm:"size:64;not null;uniqueIndex:uk_task_plugin_key_version,priority:2"`
	Source     string `json:"source" gorm:"type:text;not null"`
	SourceHash string `json:"source_hash" gorm:"size:64;not null"`
	Enabled    bool   `json:"enabled" gorm:"not null"`
	Active     bool   `json:"active" gorm:"not null;index"`
	Revision   int64  `json:"-" gorm:"not null;default:0"`
	DeletedAt  *int64 `json:"-" gorm:"index"`
	CreatedAt  int64  `json:"created_at" gorm:"not null"`
	Remark     string `json:"remark" gorm:"type:text"`
}

// TaskPluginState is the durable serialization point for one plugin key.
// Keeping the epoch outside version rows also serializes concurrent first
// uploads, where there is no existing version row to lock.
type TaskPluginState struct {
	Key      string `gorm:"size:128;primaryKey"`
	Revision int64  `gorm:"not null;default:0"`
}

func SaveTaskPlugin(plugin *TaskPlugin) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		state, err := lockTaskPluginState(tx, plugin.Key)
		if err != nil {
			return err
		}
		var versions []TaskPlugin
		if err := lockForUpdate(tx).Where(&TaskPlugin{Key: plugin.Key}).Order("id").Find(&versions).Error; err != nil {
			return err
		}
		revision, err := nextTaskPluginRevision(state, versions)
		if err != nil {
			return err
		}
		var existing TaskPlugin
		err = tx.Where(&TaskPlugin{Key: plugin.Key, Version: plugin.Version}).First(&existing).Error
		if err == nil {
			if existing.SourceHash != plugin.SourceHash {
				return errors.New("plugin key and version already exist with different source")
			}
			active := existing.Active
			if existing.DeletedAt != nil {
				active = !hasVisibleActiveTaskPlugin(versions, existing.Id)
			}
			if err = advanceTaskPluginRevision(tx, state, plugin.Key, revision); err != nil {
				return err
			}
			if err = tx.Model(&existing).Updates(map[string]any{
				"active": active, "deleted_at": nil, "enabled": plugin.Enabled,
				"remark": plugin.Remark, "revision": revision,
			}).Error; err != nil {
				return err
			}
			existing.Active = active
			existing.DeletedAt = nil
			existing.Enabled = plugin.Enabled
			existing.Remark = plugin.Remark
			existing.Revision = revision
			*plugin = existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		plugin.CreatedAt = time.Now().Unix()
		plugin.Active = !hasVisibleActiveTaskPlugin(versions, 0)
		plugin.Revision = revision
		if err = advanceTaskPluginRevision(tx, state, plugin.Key, revision); err != nil {
			return err
		}
		return tx.Create(plugin).Error
	})
}

func ListTaskPluginVersions(key string) ([]TaskPlugin, error) {
	var plugins []TaskPlugin
	err := DB.Where(&TaskPlugin{Key: key}).Where("deleted_at IS NULL").Order("created_at DESC, id DESC").Find(&plugins).Error
	return plugins, err
}

func ListTaskPlugins() ([]TaskPlugin, error) {
	var plugins []TaskPlugin
	err := DB.
		Where("deleted_at IS NULL").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "created_at"}, Desc: true}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}, Desc: true}).
		Find(&plugins).Error
	return plugins, err
}

func GetTaskPluginVersion(key, version string) (*TaskPlugin, error) {
	var plugin TaskPlugin
	query := DB.Where(&TaskPlugin{Key: key}).Where("deleted_at IS NULL")
	if version == "" {
		query = query.Where(&TaskPlugin{Active: true})
	} else {
		query = query.Where(&TaskPlugin{Version: version})
	}
	if err := query.First(&plugin).Error; err != nil {
		return nil, err
	}
	return &plugin, nil
}

// GetTaskPluginVersionForExecution includes tombstoned versions so requests
// pinned before an administrative deletion can still poll and settle against
// the exact reviewed source. Tombstones are never eligible for new routing.
func GetTaskPluginVersionForExecution(key, version string) (*TaskPlugin, error) {
	if key == "" || version == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var plugin TaskPlugin
	if err := DB.Where(&TaskPlugin{Key: key, Version: version}).First(&plugin).Error; err != nil {
		return nil, err
	}
	return &plugin, nil
}

func ListActiveTaskPlugins() ([]TaskPlugin, error) {
	snapshot, err := GetTaskPluginSyncSnapshot()
	return snapshot.Plugins, err
}

type TaskPluginSyncSnapshot struct {
	Plugins  []TaskPlugin
	Revision string
}

// GetTaskPluginSyncSnapshot returns the enabled override set together with a
// deterministic revision of every active database override. Nodes can compare
// the revision even though their local routing-generation counters differ.
func GetTaskPluginSyncSnapshot() (TaskPluginSyncSnapshot, error) {
	var activePlugins []TaskPlugin
	if err := DB.Where("active = ? AND deleted_at IS NULL", true).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "key"}}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "version"}}).
		Order(clause.OrderByColumn{Column: clause.Column{Name: "id"}}).
		Find(&activePlugins).Error; err != nil {
		return TaskPluginSyncSnapshot{}, err
	}

	type revisionEntry struct {
		Key        string `json:"key"`
		APIVersion int    `json:"api_version"`
		Version    string `json:"version"`
		SourceHash string `json:"source_hash"`
		Enabled    bool   `json:"enabled"`
	}
	entries := make([]revisionEntry, 0, len(activePlugins))
	enabledPlugins := make([]TaskPlugin, 0, len(activePlugins))
	for _, plugin := range activePlugins {
		entries = append(entries, revisionEntry{
			Key:        plugin.Key,
			APIVersion: plugin.APIVersion,
			Version:    plugin.Version,
			SourceHash: plugin.SourceHash,
			Enabled:    plugin.Enabled,
		})
		if plugin.Enabled {
			enabledPlugins = append(enabledPlugins, plugin)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Key != entries[j].Key {
			return entries[i].Key < entries[j].Key
		}
		return entries[i].Version < entries[j].Version
	})
	payload, err := common.Marshal(entries)
	if err != nil {
		return TaskPluginSyncSnapshot{}, err
	}
	digest := sha256.Sum256(payload)
	return TaskPluginSyncSnapshot{
		Plugins:  enabledPlugins,
		Revision: hex.EncodeToString(digest[:]),
	}, nil
}

type TaskPluginActivationState struct {
	Id      int64
	Key     string
	Version string
	Enabled bool
}

type TaskPluginActivation struct {
	Previous TaskPluginActivationState
	Target   TaskPluginActivationState
	Revision int64
	Changed  bool
}

func ActivateTaskPlugin(key, version string) error {
	_, err := ActivateTaskPluginWithState(key, version)
	return err
}

// ActivateTaskPluginWithState atomically switches the active version and
// returns the exact database states needed to compensate a failed publication.
func ActivateTaskPluginWithState(key, version string) (TaskPluginActivation, error) {
	activation := TaskPluginActivation{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		state, err := lockTaskPluginState(tx, key)
		if err != nil {
			return err
		}
		var versions []TaskPlugin
		if err := lockForUpdate(tx).
			Where(&TaskPlugin{Key: key}).
			Order("id").
			Find(&versions).Error; err != nil {
			return err
		}
		var previous *TaskPlugin
		var target *TaskPlugin
		for i := range versions {
			if versions[i].Active && versions[i].DeletedAt == nil {
				previous = &versions[i]
			}
			if versions[i].Version == version && versions[i].DeletedAt == nil {
				target = &versions[i]
			}
		}
		if target == nil || previous == nil {
			return gorm.ErrRecordNotFound
		}
		activation.Previous = taskPluginActivationState(*previous)
		activation.Target = taskPluginActivationState(*target)

		if previous.Id == target.Id {
			if target.Enabled {
				return nil
			}
			revision, err := nextTaskPluginRevision(state, versions)
			if err != nil {
				return err
			}
			if err = advanceTaskPluginRevision(tx, state, key, revision); err != nil {
				return err
			}
			result := tx.Model(&TaskPlugin{}).
				Where("id = ? AND active = ? AND deleted_at IS NULL", target.Id, true).
				Updates(map[string]any{"enabled": true, "revision": revision})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("task plugin activation changed concurrently")
			}
			activation.Revision = revision
			activation.Changed = true
			return nil
		}

		revision, err := nextTaskPluginRevision(state, versions)
		if err != nil {
			return err
		}
		if err = advanceTaskPluginRevision(tx, state, key, revision); err != nil {
			return err
		}
		result := tx.Model(&TaskPlugin{}).
			Where("id = ? AND active = ? AND deleted_at IS NULL", previous.Id, true).
			Updates(map[string]any{"active": false, "revision": revision})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("task plugin activation changed concurrently")
		}
		result = tx.Model(&TaskPlugin{}).
			Where("id = ? AND active = ? AND deleted_at IS NULL", target.Id, false).
			Updates(map[string]any{"active": true, "enabled": true, "revision": revision})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("task plugin activation changed concurrently")
		}
		activation.Revision = revision
		activation.Changed = true
		return nil
	})
	return activation, err
}

// RestoreTaskPluginActivation compensates a failed runtime publication only
// while the activation's target is still active. A newer activation wins.
func RestoreTaskPluginActivation(activation TaskPluginActivation) (bool, error) {
	restored := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		if activation.Previous.Key == "" ||
			activation.Previous.Key != activation.Target.Key ||
			activation.Previous.Id == 0 || activation.Target.Id == 0 {
			return errors.New("invalid task plugin activation state")
		}
		if !activation.Changed {
			return nil
		}
		state, err := lockTaskPluginState(tx, activation.Target.Key)
		if err != nil {
			return err
		}
		if state.Revision != activation.Revision {
			return nil
		}

		var versions []TaskPlugin
		if err := lockForUpdate(tx).
			Where(&TaskPlugin{Key: activation.Target.Key}).
			Order("id").
			Find(&versions).Error; err != nil {
			return err
		}
		var current *TaskPlugin
		var previous *TaskPlugin
		for i := range versions {
			if versions[i].Active && versions[i].DeletedAt == nil {
				current = &versions[i]
			}
			if versions[i].Id == activation.Previous.Id {
				previous = &versions[i]
			}
		}
		if current == nil || current.Id != activation.Target.Id || current.Revision != activation.Revision {
			return nil
		}
		if previous == nil || previous.DeletedAt != nil {
			return nil
		}
		revision, err := nextTaskPluginRevision(state, versions)
		if err != nil {
			return err
		}

		if activation.Previous.Id == activation.Target.Id {
			if err = advanceTaskPluginStateRevision(tx, state, revision); err != nil {
				return err
			}
			result := tx.Model(&TaskPlugin{}).
				Where("id = ? AND active = ? AND revision = ? AND deleted_at IS NULL", activation.Target.Id, true, activation.Revision).
				Updates(map[string]any{"enabled": activation.Previous.Enabled, "revision": revision})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("task plugin activation changed concurrently")
			}
			if err = setTaskPluginRevisionExcept(tx, activation.Target.Key, activation.Target.Id, revision); err != nil {
				return err
			}
			restored = true
			return nil
		}

		if err = advanceTaskPluginStateRevision(tx, state, revision); err != nil {
			return err
		}
		result := tx.Model(&TaskPlugin{}).
			Where("id = ? AND active = ? AND revision = ? AND deleted_at IS NULL", activation.Target.Id, true, activation.Revision).
			Updates(map[string]any{"active": false, "enabled": activation.Target.Enabled, "revision": revision})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("task plugin activation changed concurrently")
		}
		if err = setTaskPluginRevisionExcept(tx, activation.Target.Key, activation.Target.Id, revision); err != nil {
			return err
		}
		result = tx.Model(&TaskPlugin{}).
			Where("id = ? AND active = ? AND deleted_at IS NULL", activation.Previous.Id, false).
			Updates(map[string]any{"active": true, "enabled": activation.Previous.Enabled, "revision": revision})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("task plugin activation changed concurrently")
		}
		restored = true
		return nil
	})
	return restored, err
}

func taskPluginActivationState(plugin TaskPlugin) TaskPluginActivationState {
	return TaskPluginActivationState{
		Id: plugin.Id, Key: plugin.Key, Version: plugin.Version, Enabled: plugin.Enabled,
	}
}

func SetTaskPluginEnabled(key string, enabled bool) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		state, err := lockTaskPluginState(tx, key)
		if err != nil {
			return err
		}
		var versions []TaskPlugin
		if err := lockForUpdate(tx).Where(&TaskPlugin{Key: key}).Order("id").Find(&versions).Error; err != nil {
			return err
		}
		var active *TaskPlugin
		for i := range versions {
			if versions[i].Active && versions[i].DeletedAt == nil {
				active = &versions[i]
				break
			}
		}
		if active == nil {
			return gorm.ErrRecordNotFound
		}
		revision, err := nextTaskPluginRevision(state, versions)
		if err != nil {
			return err
		}
		if err = advanceTaskPluginRevision(tx, state, key, revision); err != nil {
			return err
		}
		result := tx.Model(&TaskPlugin{}).
			Where("id = ? AND active = ? AND deleted_at IS NULL", active.Id, true).
			Updates(map[string]any{"enabled": enabled, "revision": revision})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("task plugin status changed concurrently")
		}
		return nil
	})
}

type TaskPluginDeleteResult struct {
	DeletedActive bool
	Promoted      *TaskPlugin
}

func DeleteTaskPluginVersion(key, version string, revokeHistorical ...bool) (TaskPluginDeleteResult, error) {
	result := TaskPluginDeleteResult{}
	err := DB.Transaction(func(tx *gorm.DB) error {
		state, err := lockTaskPluginState(tx, key)
		if err != nil {
			return err
		}
		var versions []TaskPlugin
		if err := lockForUpdate(tx).Where(&TaskPlugin{Key: key}).Order("id").Find(&versions).Error; err != nil {
			return err
		}
		var plugin *TaskPlugin
		revoke := len(revokeHistorical) > 0 && revokeHistorical[0]
		for i := range versions {
			if versions[i].Version == version && (versions[i].DeletedAt == nil || revoke) {
				plugin = &versions[i]
				break
			}
		}
		if plugin == nil {
			return gorm.ErrRecordNotFound
		}
		wasActive := plugin.Active
		result.DeletedActive = wasActive
		revision, err := nextTaskPluginRevision(state, versions)
		if err != nil {
			return err
		}
		if err = advanceTaskPluginRevision(tx, state, key, revision); err != nil {
			return err
		}
		if plugin.DeletedAt != nil {
			if !revoke {
				return gorm.ErrRecordNotFound
			}
			return tx.Model(plugin).Updates(map[string]any{"enabled": false, "revision": revision}).Error
		}
		deletedAt := time.Now().Unix()
		updates := map[string]any{"active": false, "deleted_at": deletedAt, "revision": revision}
		if revoke {
			updates["enabled"] = false
		}
		if err = tx.Model(plugin).Updates(updates).Error; err != nil {
			return err
		}
		if !wasActive {
			return nil
		}

		var promoted TaskPlugin
		err = tx.
			Where(&TaskPlugin{Key: key}).
			Where("deleted_at IS NULL").
			Order("created_at DESC, id DESC").
			First(&promoted).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = tx.Model(&promoted).Updates(map[string]any{"active": true, "revision": revision}).Error; err != nil {
			return err
		}
		promoted.Active = true
		result.Promoted = &promoted
		return nil
	})
	return result, err
}

func hasVisibleActiveTaskPlugin(versions []TaskPlugin, excludeID int64) bool {
	for i := range versions {
		if versions[i].Id != excludeID && versions[i].Active && versions[i].DeletedAt == nil {
			return true
		}
	}
	return false
}

func nextTaskPluginRevision(state *TaskPluginState, versions []TaskPlugin) (int64, error) {
	revision := state.Revision
	for i := range versions {
		if versions[i].Revision > revision {
			revision = versions[i].Revision
		}
	}
	if revision == math.MaxInt64 {
		return 0, errors.New("task plugin revision exhausted")
	}
	return revision + 1, nil
}

func lockTaskPluginState(tx *gorm.DB, key string) (*TaskPluginState, error) {
	if key == "" {
		return nil, errors.New("task plugin key is required")
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&TaskPluginState{Key: key}).Error; err != nil {
		return nil, err
	}
	var state TaskPluginState
	if err := lockForUpdate(tx).Where(&TaskPluginState{Key: key}).First(&state).Error; err != nil {
		return nil, err
	}
	return &state, nil
}

func advanceTaskPluginStateRevision(tx *gorm.DB, state *TaskPluginState, revision int64) error {
	result := tx.Model(&TaskPluginState{}).
		Where("revision = ?", state.Revision).
		Where(&TaskPluginState{Key: state.Key}).
		Update("revision", revision)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("task plugin state changed concurrently")
	}
	state.Revision = revision
	return nil
}

func advanceTaskPluginRevision(tx *gorm.DB, state *TaskPluginState, key string, revision int64) error {
	if err := advanceTaskPluginStateRevision(tx, state, revision); err != nil {
		return err
	}
	return tx.Model(&TaskPlugin{}).Where(&TaskPlugin{Key: key}).Update("revision", revision).Error
}

func setTaskPluginRevisionExcept(tx *gorm.DB, key string, id, revision int64) error {
	return tx.Model(&TaskPlugin{}).Where(&TaskPlugin{Key: key}).Where("id <> ?", id).Update("revision", revision).Error
}
