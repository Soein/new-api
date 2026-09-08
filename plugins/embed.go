package plugins

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
)

// Previous factory sources must remain byte-identical: task snapshots pin their
// SHA-256 hashes. Archive a replaced version before updating its current source.
//
//go:embed tasks/*/plugin.js tasks/*/history/*/plugin.js
var taskPlugins embed.FS

func init() {
	entries, err := fs.ReadDir(taskPlugins, "tasks")
	if err != nil {
		panic(fmt.Sprintf("read embedded task plugins: %v", err))
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key := entry.Name()
		source, sourceErr := Source(key)
		if sourceErr != nil {
			panic(fmt.Sprintf("read embedded task plugin %s: %v", key, sourceErr))
		}
		if _, registerErr := jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: key}); registerErr != nil {
			panic(fmt.Sprintf("register embedded task plugin %s: %v", key, registerErr))
		}
		history, historyErr := fs.Glob(taskPlugins, "tasks/"+key+"/history/*/plugin.js")
		if historyErr != nil {
			panic(fmt.Sprintf("read embedded task plugin history %s: %v", key, historyErr))
		}
		for _, path := range history {
			archived, readErr := taskPlugins.ReadFile(path)
			if readErr != nil {
				panic(fmt.Sprintf("read embedded task plugin history %s: %v", path, readErr))
			}
			version := strings.Split(path, "/")[3]
			if _, registerErr := jsplugin.DefaultRegistry.RegisterFactoryHistory(string(archived), jsplugin.Options{Key: key, Version: version}); registerErr != nil {
				panic(fmt.Sprintf("register embedded task plugin history %s: %v", path, registerErr))
			}
		}
	}
}

// Source returns the embedded factory source for a task plugin key.
func Source(key string) (string, error) {
	source, err := taskPlugins.ReadFile("tasks/" + key + "/plugin.js")
	if err != nil {
		return "", err
	}
	return string(source), nil
}
