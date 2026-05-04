package yamlutil

import (
	"fmt"

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
