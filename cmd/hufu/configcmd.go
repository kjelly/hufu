package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/config"
)

var configJSON bool

var configCmd = &cobra.Command{Use: "config", Short: "Inspect and manage hufu configuration", Args: cobra.NoArgs, RunE: runConfig}
var configProfilesCmd = &cobra.Command{Use: "list-profiles", Short: "List configured profiles", Args: cobra.NoArgs, RunE: runConfigProfiles}
var configShowCmd = &cobra.Command{Use: "show <profile>", Short: "Show a profile", Args: cobra.ExactArgs(1), RunE: runConfigShow}
var configGetCmd = &cobra.Command{Use: "get <key>", Short: "Read a key from the user config", Args: cobra.ExactArgs(1), RunE: runConfigGet}
var configSetCmd = &cobra.Command{Use: "set <key> <value>", Short: "Set a key in the user config", Args: cobra.ExactArgs(2), RunE: runConfigSet}
var configInitCmd = &cobra.Command{Use: "init", Short: "Create a user config if absent", Args: cobra.NoArgs, RunE: runConfigInit}

func init() {
	configCmd.PersistentFlags().BoolVar(&configJSON, "json", false, "Write JSON to stdout")
	configCmd.AddCommand(configProfilesCmd, configShowCmd, configGetCmd, configSetCmd, configInitCmd)
}

func runConfig(_ *cobra.Command, _ []string) error {
	cfg := config.LoadConfig()
	if configJSON {
		return json.NewEncoder(os.Stdout).Encode(cfg)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, string(data))
	return err
}

func runConfigProfiles(_ *cobra.Command, _ []string) error {
	cfg := config.LoadConfig()
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if configJSON {
		return json.NewEncoder(os.Stdout).Encode(names)
	}
	for _, name := range names {
		if _, err := fmt.Fprintln(os.Stdout, name); err != nil {
			return err
		}
	}
	return nil
}

func runConfigShow(_ *cobra.Command, args []string) error {
	profile, ok := config.LoadConfig().Profiles[args[0]]
	if !ok {
		return fmt.Errorf("profile %q not found", args[0])
	}
	if configJSON {
		return json.NewEncoder(os.Stdout).Encode(profile)
	}
	data, err := yaml.Marshal(profile)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, string(data))
	return err
}

func userConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "hufu", "hufu.yaml"), nil
}
func readUserConfigMap() (map[string]interface{}, string, error) {
	path, err := userConfigPath()
	if err != nil {
		return nil, "", err
	}
	result := map[string]interface{}{}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, path, nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	return result, path, nil
}
func mapValue(values map[string]interface{}, key string) (interface{}, bool) {
	var value interface{} = values
	for _, part := range strings.Split(key, ".") {
		next, ok := value.(map[string]interface{})
		if !ok {
			return nil, false
		}
		value, ok = next[part]
		if !ok {
			return nil, false
		}
	}
	return value, true
}
func setMapValue(values map[string]interface{}, key, raw string) {
	parts := strings.Split(key, ".")
	current := values
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			current[part] = next
		}
		current = next
	}
	var value interface{} = raw
	_ = yaml.Unmarshal([]byte(raw), &value)
	current[parts[len(parts)-1]] = value
}

func runConfigGet(_ *cobra.Command, args []string) error {
	values, _, err := readUserConfigMap()
	if err != nil {
		return err
	}
	value, ok := mapValue(values, args[0])
	if !ok {
		return fmt.Errorf("key %q not found", args[0])
	}
	if configJSON {
		return json.NewEncoder(os.Stdout).Encode(value)
	}
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(os.Stdout, string(data))
	return err
}
func runConfigSet(_ *cobra.Command, args []string) error {
	values, path, err := readUserConfigMap()
	if err != nil {
		return err
	}
	setMapValue(values, args[0], args[1])
	data, err := yaml.Marshal(values)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
func runConfigInit(_ *cobra.Command, _ []string) error {
	_, path, err := readUserConfigMap()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("# hufu configuration\n# model: local-model\n# provider-url: http://127.0.0.1:8080/v1\n"), 0o600)
}
