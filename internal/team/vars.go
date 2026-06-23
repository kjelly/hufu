package team

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/anomalyco/hufu/internal/yamlutil"
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
	return yamlutil.FlattenYAMLBytes(data, path)
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

var (
	placeholderRegex = regexp.MustCompile(`\{@\s*(.*?)\s*@\}`)
	dotPathRegex     = regexp.MustCompile(`\B\.([a-zA-Z0-9_]+(?:\.[a-zA-Z0-9_]+)*)`)
)

// FindMissingVars scans the team directory (team.yml/yaml and *.md agent files)
// for template placeholders like {@ .variable_name @} and returns a list of keys
// that are not present in the provided vars map.
func FindMissingVars(teamDir string, vars map[string]string) ([]string, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("invalid team directory: %w", err)
	}

	foundKeys := make(map[string]bool)

	scanFile := func(path string) error {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		content := string(raw)
		for _, phMatch := range placeholderRegex.FindAllStringSubmatch(content, -1) {
			expr := phMatch[1]
			for _, dpMatch := range dotPathRegex.FindAllStringSubmatch(expr, -1) {
				keyPath := dpMatch[1]
				parts := strings.Split(keyPath, ".")
				parentKey := parts[0]

				if parentKey == "TEAM_NAME" || parentKey == "AGENT_COUNT" || parentKey == "AGENT_NAMES" {
					continue
				}
				if _, ok := vars[keyPath]; ok {
					continue
				}
				if _, ok := vars[parentKey]; ok {
					continue
				}

				foundKeys[keyPath] = true
			}
		}
		return nil
	}

	for _, name := range []string{"team.yml", "team.yaml"} {
		if err := scanFile(filepath.Join(absDir, name)); err != nil {
			return nil, err
		}
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read team directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if err := scanFile(filepath.Join(absDir, entry.Name())); err != nil {
			return nil, err
		}
	}

	var missing []string
	for k := range foundKeys {
		missing = append(missing, k)
	}
	sort.Strings(missing)
	return missing, nil
}
