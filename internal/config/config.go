package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/yamlutil"
)

const DefaultProviderURL = "http://localhost:11434/v1"
const DefaultEmbeddingModel = "qwen3-embedding:4b"
const DefaultOllamaAPIURL = "http://localhost:11434/api"

type ModelEntry struct {
	ID      string `yaml:"id"`
	Details string `yaml:"details"`
}

type ProviderConfig struct {
	ProviderURL    string `yaml:"provider-url"`
	ProviderAPIKey string `yaml:"provider-api-key"`
	Insecure       bool   `yaml:"insecure"`
}

type Config struct {
	ProviderURL    string                    `yaml:"provider-url"`
	ProviderAPIKey string                    `yaml:"provider-api-key"`
	Providers      map[string]ProviderConfig `yaml:"providers"`
	Model          string                    `yaml:"model"`
	EmbeddingModel string                    `yaml:"embedding-model"`
	ModelList      []ModelEntry              `yaml:"model-list"`
	SidecarModel   string                    `yaml:"sidecar-model"`
	GuardModel     string                    `yaml:"guard-model"`
	MaxConcurrent  int                       `yaml:"max-concurrent"`
	AllowedPaths   []string                  `yaml:"allowed-paths"`
	RestrictedPath string                    `yaml:"restricted-path"`
	NoNet          bool                      `yaml:"no-net"`
	ForceMCP       bool                      `yaml:"force-mcp"`
	Shell          string                    `yaml:"shell"`
	RawVars        interface{}               `yaml:"vars"`
	Hooks          map[string]string         `yaml:"hooks"`
	Notify         notify.NotifyConfig       `yaml:"notify"`
	// Profiles are named bundles of CLI flag values, selectable with --profile.
	// Each value maps a flag name to a string the flag knows how to parse, e.g.
	//   profiles:
	//     batch: {unattended: "true", max-duration: "600"}
	Profiles map[string]map[string]string `yaml:"profiles"`
}

func (c *Config) GetVars() map[string]string {
	if c.RawVars == nil {
		return nil
	}
	result := make(map[string]string)
	switch vars := c.RawVars.(type) {
	case map[string]interface{}:
		if err := yamlutil.FlattenYAML(vars, "", result); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to flatten config vars: %v\n", err)
		}
	case map[interface{}]interface{}:
		stringMap := make(map[string]interface{}, len(vars))
		for k, v := range vars {
			stringMap[fmt.Sprintf("%v", k)] = v
		}
		if err := yamlutil.FlattenYAML(stringMap, "", result); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to flatten config vars: %v\n", err)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func LoadConfig() *Config {
	cfg := &Config{}
	homeDir, _ := os.UserHomeDir()
	homeConfigPath := filepath.Join(homeDir, ".config", "hufu", "hufu.yaml")

	cfg.mergeFromFile(homeConfigPath)
	cfg.mergeFromFile("hufu.yaml")

	return cfg
}

func (c *Config) mergeFromFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var fileCfg Config
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse %s: %v\n", path, err)
		return
	}
	if fileCfg.ProviderURL != "" {
		c.ProviderURL = fileCfg.ProviderURL
	}
	if fileCfg.Model != "" {
		c.Model = fileCfg.Model
	}
	if fileCfg.EmbeddingModel != "" {
		c.EmbeddingModel = fileCfg.EmbeddingModel
	}
	if len(fileCfg.ModelList) > 0 {
		c.ModelList = fileCfg.ModelList
	}
	if fileCfg.SidecarModel != "" {
		c.SidecarModel = fileCfg.SidecarModel
	}
	if fileCfg.GuardModel != "" {
		c.GuardModel = fileCfg.GuardModel
	}
	if fileCfg.MaxConcurrent > 0 {
		c.MaxConcurrent = fileCfg.MaxConcurrent
	}
	if len(fileCfg.AllowedPaths) > 0 {
		c.AllowedPaths = fileCfg.AllowedPaths
	}
	if len(fileCfg.Hooks) > 0 {
		if c.Hooks == nil {
			c.Hooks = make(map[string]string)
		}
		for k, v := range fileCfg.Hooks {
			c.Hooks[k] = v
		}
	}
	if fileCfg.Notify.Enabled() {
		c.mergeNotify(fileCfg.Notify)
	}
	if len(fileCfg.Profiles) > 0 {
		if c.Profiles == nil {
			c.Profiles = make(map[string]map[string]string)
		}
		// Later files (./hufu.yaml) override earlier ones (~/.config) per profile name.
		for name, flags := range fileCfg.Profiles {
			c.Profiles[name] = flags
		}
	}
	if len(fileCfg.Providers) > 0 {
		if c.Providers == nil {
			c.Providers = make(map[string]ProviderConfig)
		}
		for k, v := range fileCfg.Providers {
			if _, exists := c.Providers[k]; exists {
				existing := c.Providers[k]
				if v.ProviderURL != "" {
					existing.ProviderURL = v.ProviderURL
				}
				if v.ProviderAPIKey != "" {
					existing.ProviderAPIKey = v.ProviderAPIKey
				}
				if v.Insecure {
					existing.Insecure = true
				}
				c.Providers[k] = existing
			} else {
				c.Providers[k] = v
			}
		}
	}
	fileVars := fileCfg.GetVars()
	if len(fileVars) > 0 {
		curVars := c.GetVars()
		if curVars == nil {
			curVars = make(map[string]string)
		}
		for k, v := range fileVars {
			curVars[k] = v
		}
		merged := make(map[string]interface{}, len(curVars))
		for k, v := range curVars {
			merged[k] = v
		}
		c.RawVars = merged
	}
}

// ResolveProviderURL resolves provider URL following priority order:
// 1. CLI flag
// 2. team config (passed as parameter)
// 3. agent.ProviderURL (passed as parameter)
// 4. hufu.yaml in current directory
// 5. ~/.config/hufu/hufu.yaml
// 6. default
func ResolveProviderURL(cliFlag string, teamCfgProviderURL string, agentProviderURL string) string {
	if cliFlag != "" {
		return cliFlag
	}
	if teamCfgProviderURL != "" {
		return teamCfgProviderURL
	}
	if agentProviderURL != "" {
		return agentProviderURL
	}
	cfg := LoadConfig()
	if cfg.ProviderURL != "" {
		return cfg.ProviderURL
	}
	return DefaultProviderURL
}

// ResolveProviderAPIKey resolves provider API key following priority order:
// 1. CLI flag
// 2. team config (passed as parameter)
// 3. hufu.yaml in current directory or ~/.config/hufu/hufu.yaml
// 4. HUFU_PROVIDER_API_KEY environment variable
// 5. default (empty string, NewOllamaProvider will use "ollama" as fallback)
func ResolveProviderAPIKey(cliFlag string, teamCfgAPIKey string) string {
	if cliFlag != "" {
		return cliFlag
	}
	if teamCfgAPIKey != "" {
		return teamCfgAPIKey
	}
	cfg := LoadConfig()
	if cfg.ProviderAPIKey != "" {
		return cfg.ProviderAPIKey
	}
	if envKey := os.Getenv("HUFU_PROVIDER_API_KEY"); envKey != "" {
		return envKey
	}
	return ""
}

func ResolveEmbeddingModel(cliFlag string) string {
	if cliFlag != "" {
		return cliFlag
	}
	cfg := LoadConfig()
	if cfg.EmbeddingModel != "" {
		return cfg.EmbeddingModel
	}
	return DefaultEmbeddingModel
}

func (c *Config) ResolveModelList(teamList []ModelEntry) []ModelEntry {
	if len(teamList) > 0 {
		return teamList
	}
	return c.ModelList
}

func (c *Config) ResolveSidecarModel(teamSidecar string) string {
	if teamSidecar != "" {
		return teamSidecar
	}
	return c.SidecarModel
}

func (c *Config) ResolveMaxConcurrent(teamMax int) int {
	if teamMax > 0 {
		return teamMax
	}
	if c.MaxConcurrent > 0 {
		return c.MaxConcurrent
	}
	return 0
}

func (c *Config) ResolveGuardModel(teamGuard, teamSidecar string) string {
	if teamGuard != "" {
		return teamGuard
	}
	if c.GuardModel != "" {
		return c.GuardModel
	}
	if teamSidecar != "" {
		return teamSidecar
	}
	return c.SidecarModel
}

// ResolveModel returns the effective default model following priority:
// team.yaml model > hufu.yaml model.
func (c *Config) ResolveModel(teamModel string) string {
	if teamModel != "" {
		return teamModel
	}
	return c.Model
}

func (c *Config) GetHooks() map[string]string {
	if c.Hooks == nil {
		return nil
	}
	return c.Hooks
}

func (c *Config) mergeNotify(other notify.NotifyConfig) {
	if other.OSC {
		c.Notify.OSC = true
	}
	if other.Command != "" {
		c.Notify.Command = other.Command
	}
	if len(other.Events) > 0 {
		c.Notify.Events = other.Events
	}
}

func (c *Config) ResolveNotify(teamNotify notify.NotifyConfig) notify.NotifyConfig {
	result := notify.NotifyConfig{
		OSC:     c.Notify.OSC,
		Command: c.Notify.Command,
		Events:  c.Notify.Events,
	}
	if teamNotify.OSC {
		result.OSC = true
	}
	if teamNotify.Command != "" {
		result.Command = teamNotify.Command
	}
	if len(teamNotify.Events) > 0 {
		result.Events = teamNotify.Events
	}
	return result
}

// ProviderURLToOllamaAPI converts a provider URL (e.g. http://localhost:11434/v1)
// to the Ollama API URL (e.g. http://localhost:11434/api).
func ProviderURLToOllamaAPI(providerURL string) string {
	u := strings.TrimRight(providerURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	return u + "/api"
}

// MergeProviderConfigs merges team-level provider configs on top of hufu-level configs.
// Team values take precedence for each field.
func MergeProviderConfigs(hufuProviders, teamProviders map[string]ProviderConfig) map[string]ProviderConfig {
	result := make(map[string]ProviderConfig)
	for k, v := range hufuProviders {
		result[k] = v
	}
	for k, v := range teamProviders {
		existing, exists := result[k]
		if !exists {
			result[k] = v
			continue
		}
		if v.ProviderURL != "" {
			existing.ProviderURL = v.ProviderURL
		}
		if v.ProviderAPIKey != "" {
			existing.ProviderAPIKey = v.ProviderAPIKey
		}
		if v.Insecure {
			existing.Insecure = true
		}
		result[k] = existing
	}
	return result
}
