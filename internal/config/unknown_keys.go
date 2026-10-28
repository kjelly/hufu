package config

import (
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

// configKeys is the set of top-level keys Config declares. yaml.Unmarshal
// drops every other key without an error.
var configKeys = sync.OnceValue(func() map[string]bool {
	keys := make(map[string]bool)
	for field := range reflect.TypeFor[Config]().Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name != "-" {
			keys[name] = true
		}
	}
	return keys
})

// unknownConfigKeyHints says where a key that hufu.yaml does not accept
// belongs instead.
var unknownConfigKeyHints = map[string]string{
	"decision-primitives": "decision-primitives entries belong in team.yaml; hufu.yaml shares only decision-runtime backend settings",
}

// reportedConfigKeys keeps one warning per file and key: LoadConfig runs
// several times in a single command.
var reportedConfigKeys sync.Map

func unknownConfigKeys(data []byte) []string {
	var document map[string]yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil
	}
	var unknown []string
	for key := range document {
		if !configKeys()[key] {
			unknown = append(unknown, key)
		}
	}
	slices.Sort(unknown)
	return unknown
}

func warnUnknownConfigKeys(w io.Writer, path string, data []byte) {
	for _, key := range unknownConfigKeys(data) {
		if _, reported := reportedConfigKeys.LoadOrStore(path+"\x00"+key, struct{}{}); reported {
			continue
		}
		reason := "hufu.yaml has no such setting"
		if hint := unknownConfigKeyHints[key]; hint != "" {
			reason = hint
		}
		_, _ = fmt.Fprintf(w, "warning: %s: ignoring unknown key %q: %s\n", path, key, reason)
	}
}
