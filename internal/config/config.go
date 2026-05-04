package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anomalyco/hufu/internal/yamlutil"
	"gopkg.in/yaml.v3"
)

const DefaultProviderURL = "http://localhost:11434/v1"
const DefaultEmbeddingModel = "qwen3-embedding:4b"
const DefaultOllamaAPIURL = "http://localhost:11434/api"

type ModelEntry struct {
	ID      string `yaml:"id"`
	Details string `yaml:"details"`
}

type Config struct {
	ProviderURL    string       `yaml:"provider-url"`
	Model          string       `yaml:"model"`
	EmbeddingModel string       `yaml:"embedding-model"`
	ModelList      []ModelEntry `yaml:"model-list"`
	SidecarModel   string       `yaml:"sidecar-model"`
	AllowedPaths   []string     `yaml:"allowed-paths"`
	RawVars        interface{}  `yaml:"vars"`
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
	if len(fileCfg.AllowedPaths) > 0 {
		c.AllowedPaths = fileCfg.AllowedPaths
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

// ResolveModel returns the effective default model following priority:
// team.yaml model > hufu.yaml model.
func (c *Config) ResolveModel(teamModel string) string {
	if teamModel != "" {
		return teamModel
	}
	return c.Model
}

// ProviderURLToOllamaAPI converts a provider URL (e.g. http://localhost:11434/v1)
// to the Ollama API URL (e.g. http://localhost:11434/api).
func ProviderURLToOllamaAPI(providerURL string) string {
	u := strings.TrimRight(providerURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	return u + "/api"
}
