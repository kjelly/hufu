package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/notify"
	"github.com/anomalyco/hufu/internal/tools"
)

const DefaultMaxSteps = 30
const DefaultCoordinatorMaxSteps = 20

type GenerationParams struct {
	Model       string
	Temperature string
	MaxTokens   string
	TopP        string
	TopK        string
}

type MCPInputConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"desc"`
	Type        string `yaml:"type"` // string, number, boolean
	Required    bool   `yaml:"required"`
}

// UnmarshalYAML allows MCPInputConfig to be defined as a simple string or an object
func (i *MCPInputConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		i.Name = s
		i.Required = true
		i.Type = "string"
		return nil
	}
	type plain MCPInputConfig
	var p plain
	if err := unmarshal(&p); err != nil {
		return err
	}
	*i = MCPInputConfig(p)
	if i.Type == "" {
		i.Type = "string"
	}
	return nil
}

type MCPToolConfig struct {
	Cmd    string           `yaml:"cmd"`
	Desc   string           `yaml:"desc"`
	Inputs []MCPInputConfig `yaml:"inputs"`
	Shell  string           `yaml:"shell"`
	Dir    string           `yaml:"dir"`
}

type AgentDef struct {
	Name           string
	FileAlias      string
	Description    string
	Tools          string
	Role           string
	System         string
	Capabilities   string
	Skills         string
	Guard          []string
	Timeout        int64
	MaxRetries     int
	MaxSteps       int
	AllowedPaths   []string
	RestrictedPath string
	NoNet          bool
	ForceMCP       bool
	Shell          string
	MCPTools       map[string]MCPToolConfig
	ProviderURL    string
	Generation     GenerationParams
	ExtraModels    []string
}

type TeamConfig struct {
	Name          string
	Description   string
	MaxRounds     int
	MaxSteps      int
	WorkspaceDir  string
	Timeout       int64
	MaxRetries    int
	Generation    GenerationParams
	Skills        string
	SkillsExclude string
	ProviderURL    string
	ProviderAPIKey string
	Providers     map[string]config.ProviderConfig
	ModelList     []config.ModelEntry
	SidecarModel  string
	GuardModel    string
	MaxConcurrent int
	Notify        notify.NotifyConfig
	AllowedPaths   []string
	RestrictedPath string
	NoNet            bool
	ForceMCP         bool
	Shell            string
	Vars             map[string]interface{}
	WorkerContextSize int
	ToolsAllowed   []string // List of explicitly allowed tools
}

type OllamaProvider struct {
	provider fantasy.Provider
	baseURL  string
	apiKey   string
	name     string
}

func NewOllamaProvider(baseURL, apiKey, name string) (*OllamaProvider, error) {
	if baseURL == "" {
		baseURL = config.DefaultProviderURL
	}
	if apiKey == "" {
		apiKey = "ollama"
	}
	if name == "" {
		name = "ollama"
	}
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
		openaicompat.WithName(name),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama provider: %w", err)
	}
	return &OllamaProvider{provider: provider, baseURL: baseURL, apiKey: apiKey, name: name}, nil
}

func (p *OllamaProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	model := strings.TrimPrefix(modelID, p.name+"/")
	return p.provider.LanguageModel(ctx, model)
}

// ParseModelProvider extracts the provider prefix and model name from a model ID.
// "ollama/qwen3:8b" → ("ollama", "qwen3:8b")
// "qwen3:8b" → ("", "qwen3:8b")
func ParseModelProvider(modelID string) (provider, modelName string) {
	if idx := strings.Index(modelID, "/"); idx >= 0 {
		return modelID[:idx], modelID[idx+1:]
	}
	return "", modelID
}

// ProviderManager manages multiple OllamaProviders, one per provider prefix.
// It lazy-initializes providers on first use based on the model ID prefix.
type ProviderManager struct {
	defaultProvider *OllamaProvider
	providers       map[string]*OllamaProvider
	configs         map[string]config.ProviderConfig
	mu              sync.RWMutex
}

func NewProviderManager(defaultURL, defaultKey string, providerConfigs map[string]config.ProviderConfig) (*ProviderManager, error) {
	defaultProv, err := NewOllamaProvider(defaultURL, defaultKey, "ollama")
	if err != nil {
		return nil, fmt.Errorf("failed to create default provider: %w", err)
	}
	if providerConfigs == nil {
		providerConfigs = make(map[string]config.ProviderConfig)
	}
	return &ProviderManager{
		defaultProvider: defaultProv,
		providers:       make(map[string]*OllamaProvider),
		configs:         providerConfigs,
	}, nil
}

// GetProvider returns the OllamaProvider for the given modelID, and the
// stripped model name (without the provider prefix). Unknown providers
// fall back to the default (ollama) provider.
func (pm *ProviderManager) GetProvider(modelID string) *OllamaProvider {
	prefix, _ := ParseModelProvider(modelID)
	name := prefix
	if name == "" {
		name = "ollama"
	}

	// Fast path: check cache with read lock
	pm.mu.RLock()
	if p, ok := pm.providers[name]; ok {
		pm.mu.RUnlock()
		return p
	}
	pm.mu.RUnlock()

	// Slow path: maybe initialize
	pm.mu.Lock()
	defer pm.mu.Unlock()
	// Double-check after acquiring write lock
	if p, ok := pm.providers[name]; ok {
		return p
	}

	// Check for per-provider config
	cfg, hasCfg := pm.configs[name]
	if hasCfg {
		url := cfg.ProviderURL
		if url == "" {
			url = pm.defaultProvider.baseURL
		}
		key := cfg.ProviderAPIKey
		if key == "" {
			key = pm.defaultProvider.apiKey
		}
		p, err := NewOllamaProvider(url, key, name)
		if err == nil {
			pm.providers[name] = p
			return p
		}
	}
	// Fall back to default provider
	return pm.defaultProvider
}

// DefaultProvider returns the default (ollama) provider.
func (pm *ProviderManager) DefaultProvider() *OllamaProvider {
	return pm.defaultProvider
}

type AgentConfig struct {
	Def        *AgentDef
	TeamConfig *TeamConfig
	WorkDir    string
	MaxSteps   int
}

func resolveMaxSteps(agentSteps, teamSteps int) int {
	if agentSteps > 0 {
		return agentSteps
	}
	if teamSteps > 0 {
		return teamSteps
	}
	return DefaultMaxSteps
}

func CreateAgent(ctx context.Context, ollama *OllamaProvider, cfg AgentConfig, agentTools []fantasy.AgentTool) (fantasy.Agent, error) {
	modelStr := cfg.Def.Generation.Model
	if modelStr == "" {
		modelStr = cfg.TeamConfig.Generation.Model
	}
	if modelStr == "" {
		return nil, fmt.Errorf("no model specified for agent %q", cfg.Def.Name)
	}

	lm, err := ollama.LanguageModel(ctx, modelStr)
	if err != nil {
		return nil, fmt.Errorf("failed to create language model for %q: %w", cfg.Def.Name, err)
	}

	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(cfg.Def.System),
		fantasy.WithTools(agentTools...),
	}

	if maxTokens := parseModelInt(cfg.Def.Generation.MaxTokens, cfg.TeamConfig.Generation.MaxTokens); maxTokens > 0 {
		opts = append(opts, fantasy.WithMaxOutputTokens(int64(maxTokens)))
	}
	if temp := parseModelFloat(cfg.Def.Generation.Temperature, cfg.TeamConfig.Generation.Temperature); temp >= 0 {
		opts = append(opts, fantasy.WithTemperature(temp))
	}
	if topP := parseModelFloat(cfg.Def.Generation.TopP, cfg.TeamConfig.Generation.TopP); topP >= 0 {
		opts = append(opts, fantasy.WithTopP(topP))
	}
	if topK := parseModelInt(cfg.Def.Generation.TopK, cfg.TeamConfig.Generation.TopK); topK > 0 {
		opts = append(opts, fantasy.WithTopK(int64(topK)))
	}

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = resolveMaxSteps(cfg.Def.MaxSteps, cfg.TeamConfig.MaxSteps)
	}
	if maxSteps > 0 {
		opts = append(opts, fantasy.WithStopConditions(fantasy.StepCountIs(maxSteps)))
	}

	return fantasy.NewAgent(lm, opts...), nil
}

func RunAgent(ctx context.Context, agent fantasy.Agent, prompt string) (string, error) {
	result, err := agent.Generate(ctx, fantasy.AgentCall{
		Prompt: prompt,
	})
	if err != nil {
		return "", err
	}
	return result.Response.Content.Text(), nil
}

var alwaysIncludeTools = map[string]bool{
	"request_agent": true,
	"todo":          true,
	"random":        true,
	"memory_save":   true,
	"memory_query":  true,
	"load_skill":    true,
	"stm_write":     true,
	"ltm_update":    true,
	"team_info":     true,
}

func SelectTools(allTools []fantasy.AgentTool, toolNames string) []fantasy.AgentTool {
	if toolNames == "" || toolNames == "all" {
		return allTools
	}
	requested := map[string]bool{}
	for _, name := range strings.Split(toolNames, ",") {
		n := strings.TrimSpace(name)
		requested[n] = true
	}

	var selected []fantasy.AgentTool
	for _, t := range allTools {
		if requested[t.Info().Name] || alwaysIncludeTools[t.Info().Name] {
			selected = append(selected, t)
		} else if t.Info().Name == "view" && requested["read"] {
			selected = append(selected, t)
		} else if t.Info().Name == "glob" && requested["find"] {
			selected = append(selected, t)
		}
	}
	return selected
}

func BuildAllAgentTools(workDir string, opts ...tools.ToolOption) []fantasy.AgentTool {
	allOpts := append([]tools.ToolOption{tools.WithWorkDir(workDir)}, opts...)
	return tools.AllTools(allOpts...)
}

func parseModelInt(primary, fallback string) int {
	if primary != "" {
		if v, err := strconv.Atoi(primary); err == nil {
			return v
		}
	}
	if fallback != "" {
		if v, err := strconv.Atoi(fallback); err == nil {
			return v
		}
	}
	return -1
}

func parseModelFloat(primary, fallback string) float64 {
	if primary != "" {
		if v, err := strconv.ParseFloat(primary, 64); err == nil {
			return v
		}
	}
	if fallback != "" {
		if v, err := strconv.ParseFloat(fallback, 64); err == nil {
			return v
		}
	}
	return -1
}
