package model

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/request_plugin_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const sensitiveWordMigrationOptionKey = "migration.sensitive_words"

// MigrateSensitiveWords imports the legacy prompt filter into one global
// request-filter instance. Keep the legacy options for rolling upgrades and
// rollback. The marker survives edits or removal of the imported instance.
func MigrateSensitiveWords() error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	name := ""
	err := DB.Transaction(func(tx *gorm.DB) error {
		var stored Option
		marker := Option{Key: sensitiveWordMigrationOptionKey}
		err := tx.Where(&marker).Take(&stored).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("read sensitive word migration marker: %w", err)
		}
		values := make(map[string]string, 3)
		for _, key := range []string{"SensitiveWords", "CheckSensitiveEnabled", "CheckSensitiveOnPromptEnabled"} {
			var option Option
			err := tx.Where(&Option{Key: key}).First(&option).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read option %s: %w", key, err)
			}
			values[key] = option.Value
		}
		words := sensitiveWordLines(values["SensitiveWords"])
		if words == "" {
			return nil
		}
		// Claim the import before touching the instance list. Read the winning
		// value instead of using RowsAffected, which varies for MySQL no-op writes.
		marker.Value = rand.Text()
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker).Error; err != nil {
			return fmt.Errorf("claim sensitive word migration: %w", err)
		}
		if err := lockForUpdate(tx).Where(&Option{Key: marker.Key}).Take(&stored).Error; err != nil {
			return fmt.Errorf("read sensitive word migration claim: %w", err)
		}
		if stored.Value != marker.Value {
			return nil
		}
		enabled := values["CheckSensitiveEnabled"] == "true" && values["CheckSensitiveOnPromptEnabled"] == "true"
		name = sensitiveWordInstanceName()
		return addSensitiveWordInstance(tx, name, words, enabled)
	})
	if err != nil {
		return err
	}
	if name != "" {
		BumpSyncTopic(SyncTopicOptions)
		common.SysLog(common.LogText("moved the sensitive words to the global request filter instance %s", name))
	}
	return nil
}

// sensitiveWordLines is the old word list, one word per line, as the check
// read it: lines trimmed, empty lines dropped.
func sensitiveWordLines(value string) string {
	var words []string
	for line := range strings.SplitSeq(value, "\n") {
		if word := strings.TrimSpace(line); word != "" {
			words = append(words, word)
		}
	}
	return strings.Join(words, "\n")
}

func sensitiveWordInstanceName() string {
	if language := strings.ToLower(strings.TrimSpace(common.DefaultLanguage)); language != "" && !strings.HasPrefix(language, "zh") {
		return "Sensitive words"
	}
	return "敏感词"
}

// addSensitiveWordInstance appends the instance to the saved instance list.
// The old check matched words in any letter case.
func addSensitiveWordInstance(tx *gorm.DB, name, words string, enabled bool) error {
	var option Option
	err := lockForUpdate(tx).Where(&Option{Key: request_plugin_setting.InstancesOption}).First(&option).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("read option %s: %w", request_plugin_setting.InstancesOption, err)
	}
	var instances []request_plugin_setting.Instance
	if strings.TrimSpace(option.Value) != "" {
		if err := common.UnmarshalJsonStr(option.Value, &instances); err != nil {
			return fmt.Errorf("read option %s: %w", request_plugin_setting.InstancesOption, err)
		}
	}
	ids := make(map[string]bool, len(instances))
	for _, instance := range instances {
		ids[instance.ID] = true
	}
	id := ""
	for id == "" || ids[id] {
		id = "rp_" + strings.ToLower(rand.Text()[:8])
	}
	config, err := common.Marshal(map[string]any{
		"rules":                 []any{},
		"keywords":              words,
		"keywordsCaseSensitive": false,
		"message":               "",
		"replies":               false,
	})
	if err != nil {
		return err
	}
	instances = append(instances, request_plugin_setting.Instance{
		ID:      id,
		Name:    name,
		Plugin:  "request-filter",
		Enabled: enabled,
		Global:  true,
		Groups:  []string{},
		Models:  []string{},
		Config:  config,
	})
	encoded, err := common.Marshal(instances)
	if err != nil {
		return err
	}
	option.Key = request_plugin_setting.InstancesOption
	option.Value = string(encoded)
	if err := tx.Save(&option).Error; err != nil {
		return fmt.Errorf("write option %s: %w", request_plugin_setting.InstancesOption, err)
	}
	return nil
}
