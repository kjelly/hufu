package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/notify"
	"github.com/kjelly/hufu/internal/yamlutil"
)

// DefaultLocalProviderURL is the literal loopback endpoint used by the local
// provider. A literal address is required so no-net validation never needs
// DNS resolution to determine whether the endpoint is local.
const DefaultLocalProviderURL = "http://127.0.0.1:11434/v1"

// DefaultProviderURL is retained for compatibility with existing callers.
const DefaultProviderURL = DefaultLocalProviderURL
const DefaultEmbeddingModel = "ollama/nomic-embed-text:latest"
const DefaultOllamaAPIURL = "http://127.0.0.1:11434/api"

type ModelEntry struct {
	ID      string `yaml:"id"`
	Details string `yaml:"details"`
}

type ProviderConfig struct {
	ProviderURL    string `yaml:"provider-url"`
	ProviderAPIKey string `yaml:"provider-api-key"`
	// IntrospectionType selects the provider-owned runtime metadata adapter.
	// Named providers default to openai-compatible when omitted; the local
	// provider retains its historical Ollama adapter compatibility.
	IntrospectionType string `yaml:"introspection-type"`
	Insecure          bool   `yaml:"insecure"`
	// MaxConcurrent bounds how many tasks may run concurrently against this
	// specific provider, independent of the team-wide max-concurrent. A local
	// model dispatched by many workers is not the same as many workers able to
	// usefully run concurrent inference. Zero (the default) means "no
	// additional limit beyond the team-wide one".
	MaxConcurrent int `yaml:"max-concurrent"`
}

// BackendConfig is the canonical target-backend configuration. Legacy
// providers and subagent-providers are mapped into this runtime shape by the
// command setup layer while remaining readable during migration.
type BackendConfig struct {
	Kind           string   `yaml:"kind"`
	Type           string   `yaml:"type"`
	BaseURL        string   `yaml:"base-url"`
	ProviderAPIKey string   `yaml:"provider-api-key"`
	Command        []string `yaml:"command"`
	Protocol       string   `yaml:"protocol"`
	StartupTimeout string   `yaml:"startup-timeout"`
	InterruptGrace string   `yaml:"interrupt-grace"`
	ShutdownGrace  string   `yaml:"shutdown-grace"`
	ExecutionWorld string   `yaml:"execution-world"`
	InheritEnv     []string `yaml:"inherit-env"`
	MaxConcurrent  int      `yaml:"max-concurrent"`
	present        map[string]bool
}

// UnmarshalYAML records field presence in addition to values. Canonical
// backend overlays must distinguish an omitted setting (inherit a built-in
// default) from an explicitly empty scalar or empty atomic list (replace it).
func (b *BackendConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain BackendConfig
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*b = BackendConfig(decoded)
	b.present = make(map[string]bool)
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		b.present[value.Content[i].Value] = true
	}
	return nil
}

// Has reports whether the backend field occurred in YAML. Programmatic
// configurations have no syntax-presence record; for those, non-zero values
// are considered supplied to preserve the established Go construction API.
func (b BackendConfig) Has(field string) bool {
	if b.present != nil {
		return b.present[field]
	}
	switch field {
	case "kind":
		return b.Kind != ""
	case "type":
		return b.Type != ""
	case "base-url":
		return b.BaseURL != ""
	case "provider-api-key":
		return b.ProviderAPIKey != ""
	case "command":
		return b.Command != nil
	case "protocol":
		return b.Protocol != ""
	case "startup-timeout":
		return b.StartupTimeout != ""
	case "interrupt-grace":
		return b.InterruptGrace != ""
	case "shutdown-grace":
		return b.ShutdownGrace != ""
	case "execution-world":
		return b.ExecutionWorld != ""
	case "inherit-env":
		return b.InheritEnv != nil
	case "max-concurrent":
		return b.MaxConcurrent != 0
	default:
		return false
	}
}

// Validate checks provider configuration values that affect runtime adapter
// selection. An omitted type is valid because named providers have an
// explicit openai-compatible default.
func (p ProviderConfig) Validate() error {
	switch strings.ToLower(strings.TrimSpace(p.IntrospectionType)) {
	case "", "ollama", "openai-compatible":
		return nil
	default:
		return fmt.Errorf("unsupported introspection-type %q (want ollama or openai-compatible)", p.IntrospectionType)
	}
}

type Config struct {
	ProviderURL       string                    `yaml:"provider-url"`
	ProviderAPIKey    string                    `yaml:"provider-api-key"`
	Providers         map[string]ProviderConfig `yaml:"providers"`
	Backends          map[string]BackendConfig  `yaml:"backends"`
	Model             string                    `yaml:"model"`
	WorkerModel       string                    `yaml:"worker-model"`
	CoordinatorModel  string                    `yaml:"coordinator-model"`
	DefaultLLMBackend string                    `yaml:"default-llm-backend"`
	EmbeddingModel    string                    `yaml:"embedding-model"`
	ModelList         []ModelEntry              `yaml:"model-list"`
	SidecarModel      string                    `yaml:"sidecar-model"`
	GuardModel        string                    `yaml:"guard-model"`
	JudgeModel        string                    `yaml:"judge-model"`
	PlanReviewerModel string                    `yaml:"plan-reviewer-model"`
	MaxConcurrent     int                       `yaml:"max-concurrent"`
	StallThreshold    string                    `yaml:"stall-threshold"`
	AllowedPaths      []string                  `yaml:"allowed-paths"`
	RestrictedPath    string                    `yaml:"restricted-path"`
	NoNet             bool                      `yaml:"no-net"`
	ForceMCP          bool                      `yaml:"force-mcp"`
	ProjectContext    bool                      `yaml:"project-context"`
	Shell             string                    `yaml:"shell"`
	RawVars           interface{}               `yaml:"vars"`
	Hooks             map[string]string         `yaml:"hooks"`
	Notify            notify.NotifyConfig       `yaml:"notify"`
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
	c.mergeScalarFields(&fileCfg)
	c.mergeHooks(fileCfg.Hooks)
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
	c.mergeProviders(fileCfg.Providers)
	if len(fileCfg.Backends) > 0 {
		if c.Backends == nil {
			c.Backends = make(map[string]BackendConfig)
		}
		for name, backend := range fileCfg.Backends {
			c.Backends[name] = backend
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

func (c *Config) mergeScalarFields(fileCfg *Config) {
	for _, field := range []struct{ dst, src *string }{
		{&c.ProviderURL, &fileCfg.ProviderURL}, {&c.Model, &fileCfg.Model},
		{&c.WorkerModel, &fileCfg.WorkerModel}, {&c.CoordinatorModel, &fileCfg.CoordinatorModel},
		{&c.DefaultLLMBackend, &fileCfg.DefaultLLMBackend}, {&c.EmbeddingModel, &fileCfg.EmbeddingModel},
		{&c.SidecarModel, &fileCfg.SidecarModel}, {&c.PlanReviewerModel, &fileCfg.PlanReviewerModel},
		{&c.GuardModel, &fileCfg.GuardModel}, {&c.JudgeModel, &fileCfg.JudgeModel},
		{&c.StallThreshold, &fileCfg.StallThreshold},
	} {
		if *field.src != "" {
			*field.dst = *field.src
		}
	}
	if len(fileCfg.ModelList) > 0 {
		c.ModelList = fileCfg.ModelList
	}
	if fileCfg.MaxConcurrent > 0 {
		c.MaxConcurrent = fileCfg.MaxConcurrent
	}
	if len(fileCfg.AllowedPaths) > 0 {
		c.AllowedPaths = fileCfg.AllowedPaths
	}
	c.NoNet = c.NoNet || fileCfg.NoNet
	c.ForceMCP = c.ForceMCP || fileCfg.ForceMCP
	c.ProjectContext = c.ProjectContext || fileCfg.ProjectContext
}

func (c *Config) mergeHooks(hooks map[string]string) {
	if len(hooks) == 0 {
		return
	}
	if c.Hooks == nil {
		c.Hooks = make(map[string]string)
	}
	for k, v := range hooks {
		c.Hooks[k] = v
	}
}

func (c *Config) mergeProviders(providers map[string]ProviderConfig) {
	if len(providers) == 0 {
		return
	}
	if c.Providers == nil {
		c.Providers = make(map[string]ProviderConfig)
	}
	for k, v := range providers {
		existing, exists := c.Providers[k]
		if !exists {
			c.Providers[k] = v
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
		if v.IntrospectionType != "" {
			existing.IntrospectionType = v.IntrospectionType
		}
		c.Providers[k] = existing
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
// 5. default (empty string; the OpenAI-compatible local provider decides
// whether an Authorization header is needed)
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

// ResolveStallThreshold parses the stall-threshold duration string (e.g. "30m", "1800s", "1800")
// following priority: team.yaml stall-threshold > hufu.yaml stall-threshold.
// Returns 0 if none is configured or if parsing fails.
func (c *Config) ResolveStallThreshold(teamStall string) time.Duration {
	val := strings.TrimSpace(teamStall)
	if val == "" {
		val = strings.TrimSpace(c.StallThreshold)
	}
	if val == "" {
		return 0
	}
	if d, err := time.ParseDuration(val); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
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

// ResolveJudgeModel falls back to the sidecar model (not the main model):
// judging is a cheap classification task, and defaulting to the sidecar
// avoids silently doubling main-model cost. An empty result disables the
// judge, and multi-model results use the concatenation merge.
func (c *Config) ResolveJudgeModel(teamJudge, teamSidecar string) string {
	if teamJudge != "" {
		return teamJudge
	}
	if c.JudgeModel != "" {
		return c.JudgeModel
	}
	if teamSidecar != "" {
		return teamSidecar
	}
	return c.SidecarModel
}

func (c *Config) ResolvePlanReviewerModel(teamPlanReviewer, teamModel string) string {
	if teamPlanReviewer != "" {
		return teamPlanReviewer
	}
	if c.PlanReviewerModel != "" {
		return c.PlanReviewerModel
	}
	if teamModel != "" {
		return teamModel
	}
	return c.Model
}

// ResolveModel returns the effective default model following priority:
// team.yaml model > hufu.yaml model.
func (c *Config) ResolveModel(teamModel string) string {
	if teamModel != "" {
		return teamModel
	}
	return c.Model
}

// ResolveWorkerModel applies the canonical worker-model hierarchy while
// retaining legacy model values as the final compatibility fallback.
func (c *Config) ResolveWorkerModel(teamWorkerModel, legacyTeamModel string) string {
	if teamWorkerModel != "" {
		return teamWorkerModel
	}
	if c.WorkerModel != "" {
		return c.WorkerModel
	}
	return c.ResolveModel(legacyTeamModel)
}

// ResolveCoordinatorModel applies the independent coordinator target
// hierarchy. Legacy model remains a read-only fallback for existing configs.
func (c *Config) ResolveCoordinatorModel(teamCoordinatorModel, legacyTeamModel string) string {
	if teamCoordinatorModel != "" {
		return teamCoordinatorModel
	}
	if c.CoordinatorModel != "" {
		return c.CoordinatorModel
	}
	return c.ResolveModel(legacyTeamModel)
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
		if v.IntrospectionType != "" {
			existing.IntrospectionType = v.IntrospectionType
		}
		result[k] = existing
	}
	return result
}
