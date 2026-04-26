package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const DefaultProviderURL = "http://localhost:11434/v1"

type Config struct {
	ProviderURL string `yaml:"provider-url"`
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