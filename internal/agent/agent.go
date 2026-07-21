package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// SideEffect is the default side-effect classification for tasks delegated
	// to this agent (none/workspace_write/external_write/infra_mutation/
	// credential_mutation). Empty = infer from Tools at task creation time.
	SideEffect string
	// Recovery is the default interrupted-task recovery policy for this agent
	// (retry/reconcile/manual/never). Empty = derive from SideEffect at resume.
	Recovery string
	// ReconcileTool is an optional read-only probe command used during crash
	// recovery to classify whether an interrupted task completed.
	ReconcileTool string
	Generation    GenerationParams
	ExtraModels   []string
}

type TeamConfig struct {
	Name              string
	Description       string
	MaxRounds         int
	MaxSteps          int
	WorkspaceDir      string
	Timeout           int64
	VerifyTimeout     int64
	MaxRetries        int
	Generation        GenerationParams
	Skills            string
	SkillsExclude     string
	ProviderURL       string
	ProviderAPIKey    string
	Providers         map[string]config.ProviderConfig
	ModelList         []config.ModelEntry
	SidecarModel      string
	GuardModel        string
	JudgeModel        string
	PlanReviewerModel string
	MaxConcurrent     int
	// EscalateOnRetry makes every task retry escalate to the next stronger
	// model in ModelList (ordered weakest→strongest) by default.
	EscalateOnRetry   bool
	Notify            notify.NotifyConfig
	AllowedPaths      []string
	RestrictedPath    string
	NoNet             bool
	ForceMCP          bool
	ProjectContext    bool
	Shell             string
	Vars              map[string]interface{}
	WorkerContextSize int
	ToolsAllowed      []string // List of explicitly allowed tools
	Preflight         []CapabilityRequirement

	// Unattended runs the team without any blocking human interaction:
	// ask_user returns a safe default instead of reading stdin, --steps/--tui
	// are disabled, and only explicitly-allowed tools may run (deny-by-default).
	Unattended bool
	// AutoApprove lets ask_user auto-select clearly safe options when one is
	// available. Dangerous or ambiguous choices still prompt the user.
	AutoApprove bool
	// MaxWallClock caps total run wall-clock time in seconds (0 = unlimited).
	// When exceeded, the coordinator force-enters wrap-up and refuses new tasks.
	MaxWallClock int64
	// MaxTotalTokens caps cumulative LLM token usage across the run (0 = unlimited).
	MaxTotalTokens int64
	// Acceptance is an optional shell command run when the coordinator finishes;
	// a non-zero exit marks the whole run as not-accepted (reported/notified).
	Acceptance string
	// Rollback is an optional shell command run on acceptance failure in unattended mode
	Rollback string
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

// ListModelNames queries the provider's OpenAI-compatible /models endpoint
// and returns the available model names (without provider prefix). Returns an
// error when the endpoint is unreachable or unsupported; callers should treat
// that as "cannot validate", not as "model missing".
func (p *OllamaProvider) ListModelNames(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(p.baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query %s: status %s", url, resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	names := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	return names, nil
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

// Name returns the provider's prefix name (e.g. "ollama").
func (p *OllamaProvider) Name() string {
	return p.name
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
		return nil, fmt.Errorf("no model specified for agent %q\n  Set --model <name>, add 'model:' to your team's team.yaml, or add 'model:' to ~/.config/hufu/hufu.yaml\n  Run 'hufu doctor' to see which model is currently resolved", cfg.Def.Name)
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

// impliedTools maps a tool to companions that should be granted alongside it
// automatically. wait_for runs the exact same command through the exact same
// consent check and sudo allowlist as bash/sudo — it is a single tool call
// that replaces an LLM-driven sleep-and-recheck loop, not a new capability.
// A real run burned dozens of round trips on "sleep 5 && check status"
// because wait_for existed but no team.yaml opted into it; expanding the
// implication here means every team gets it the moment it grants bash or
// sudo, with no YAML to remember to update.
var impliedTools = map[string][]string{
	"bash": {"wait_for"},
	"sudo": {"wait_for"},
}

// ExpandImpliedTools appends tools implied by ones already present in a
// comma-separated tool list (see impliedTools), skipping tools already
// listed. An empty or "all" list is returned unchanged: it already grants
// everything. Call this wherever an agent's tool string is first assembled
// (team.yaml agent frontmatter, the default team, CLI-provided lists) so
// SelectTools and the runtime permission allowlist — which both consume the
// same string — see the expansion for free.
func ExpandImpliedTools(toolNames string) string {
	if toolNames == "" || toolNames == "all" {
		return toolNames
	}
	fields := strings.Split(toolNames, ",")
	have := make(map[string]bool, len(fields))
	for _, t := range fields {
		have[strings.TrimSpace(t)] = true
	}
	var add []string
	for _, t := range fields {
		for _, implied := range impliedTools[strings.TrimSpace(t)] {
			if !have[implied] {
				have[implied] = true
				add = append(add, implied)
			}
		}
	}
	if len(add) == 0 {
		return toolNames
	}
	return toolNames + "," + strings.Join(add, ",")
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
