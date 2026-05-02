package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	EmbeddingModel string       `yaml:"embedding-model"`
	ModelList      []ModelEntry `yaml:"model-list"`
	SidecarModel   string       `yaml:"sidecar-model"`
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
	if fileCfg.EmbeddingModel != "" {
		c.EmbeddingModel = fileCfg.EmbeddingModel
	}
	if len(fileCfg.ModelList) > 0 {
		c.ModelList = fileCfg.ModelList
	}
	if fileCfg.SidecarModel != "" {
		c.SidecarModel = fileCfg.SidecarModel
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

// ProviderURLToOllamaAPI converts a provider URL (e.g. http://localhost:11434/v1)
// to the Ollama API URL (e.g. http://localhost:11434/api).
func ProviderURLToOllamaAPI(providerURL string) string {
	u := strings.TrimRight(providerURL, "/")
	u = strings.TrimSuffix(u, "/v1")
	return u + "/api"
}
