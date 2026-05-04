package team

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseVarFlags parses --var key=value flags into a variable map.
// Each entry is split on the first '='; values after the first '=' are preserved.
// Later entries overwrite earlier ones.
func ParseVarFlags(flags []string) map[string]string {
	result := make(map[string]string)
	for _, f := range flags {
		idx := strings.Index(f, "=")
		if idx < 0 {
			result[f] = ""
			continue
		}
		key := f[:idx]
		val := f[idx+1:]
		result[key] = val
	}
	return result
}

// LoadVarsFile reads a variable file and returns key-value pairs.
// Supports:
//   - .yaml / .yml: parsed as flat key-value pairs (nested keys are joined with '.')
//   - other files: KEY=VALUE lines (comments with #, blank lines skipped)
func LoadVarsFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read var file %s: %w", path, err)
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		return parseYAMLVars(data, path)
	default:
		return parseEnvVars(data, path)
	}
}

func parseYAMLVars(data []byte, path string) (map[string]string, error) {
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse YAML var file %s: %w", path, err)
	}

	result := make(map[string]string)
	if err := flattenYAML(raw, "", result); err != nil {
		return nil, fmt.Errorf("failed to process YAML var file %s: %w", path, err)
	}
	return result, nil
}

func flattenYAML(m map[string]interface{}, prefix string, result map[string]string) error {
	for k, v := range m {
		fullKey := k
		if prefix != "" {
			fullKey = prefix + "." + k
		}
		switch val := v.(type) {
		case map[string]interface{}:
			if err := flattenYAML(val, fullKey, result); err != nil {
				return err
			}
		case map[interface{}]interface{}:
			stringMap := make(map[string]interface{}, len(val))
			for ik, iv := range val {
				stringMap[fmt.Sprintf("%v", ik)] = iv
			}
			if err := flattenYAML(stringMap, fullKey, result); err != nil {
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

func parseEnvVars(data []byte, path string) (map[string]string, error) {
	result := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading var file %s: %w", path, err)
	}
	return result, nil
}

// MergeVars merges multiple variable maps in order; later maps overwrite earlier ones.
func MergeVars(maps ...map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

// ResolveVars resolves all variable sources into a single variable map.
// Processing order: var-files first (in order), then --var flags (in order).
// Later entries overwrite earlier ones.
func ResolveVars(varFiles []string, varFlags []string) (map[string]string, error) {
	var maps []map[string]string

	for _, vf := range varFiles {
		m, err := LoadVarsFile(vf)
		if err != nil {
			return nil, err
		}
		maps = append(maps, m)
	}

	if len(varFlags) > 0 {
		maps = append(maps, ParseVarFlags(varFlags))
	}

	if len(maps) == 0 {
		return nil, nil
	}

	return MergeVars(maps...), nil
}
