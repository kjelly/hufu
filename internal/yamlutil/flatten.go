package yamlutil

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// FlattenYAML recursively flattens a nested map into dot-separated keys.
// Nested maps are joined with '.'; scalar values are stringified.
// Lists are not supported and return an error.
func FlattenYAML(m map[string]interface{}, prefix string, result map[string]string) error {
	for k, v := range m {
		fullKey := k
		if prefix != "" {
			fullKey = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]interface{}:
			if err := FlattenYAML(val, fullKey, result); err != nil {
				return err
			}
		case map[interface{}]interface{}:
			stringMap := make(map[string]interface{}, len(val))
			for ik, iv := range val {
				stringMap[fmt.Sprintf("%v", ik)] = iv
			}
			if err := FlattenYAML(stringMap, fullKey, result); err != nil {
				return err
			}
		case []interface{}:
			return fmt.Errorf("yaml variable %q is a list; only scalar values are supported in var files", fullKey)
		default:
			result[fullKey] = fmt.Sprintf("%v", val)
		}
	}
	return nil
}

// FlattenYAMLBytes parses YAML bytes and flattens the result into dot-separated keys.
func FlattenYAMLBytes(data []byte, path string) (map[string]string, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse YAML %s: %w", path, err)
	}

	result := make(map[string]string)
	if err := FlattenYAML(raw, "", result); err != nil {
		return nil, fmt.Errorf("failed to process YAML %s: %w", path, err)
	}
	return result, nil
}

// ParseSimpleYAML reads key: value lines from data, ignoring comments and blank lines.
// Used as a fallback parser when go-yaml.v3 fails on simple frontmatter.
func ParseSimpleYAML(data string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 &&
			((val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result
}
