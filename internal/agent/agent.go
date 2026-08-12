package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/notify"
	"github.com/kjelly/hufu/internal/tools"
)

const DefaultMaxSteps = 30
const DefaultCoordinatorMaxSteps = 20

// Default generation settings favor deterministic, reliable agent behavior
// for tool use and code changes while retaining enough sampling diversity.
const (
	DefaultTemperature = "0.2"
	DefaultTopP        = "0.9"
	DefaultMaxTokens   = "16384"
)

type GenerationParams struct {
	Model       string
	Temperature string
	MaxTokens   string
	TopP        string
	TopK        string
	// ReasoningEffort controls how much a reasoning-capable model "thinks"
	// before answering: high, medium, low, or none. Passed through to
	// Ollama's OpenAI-compatible reasoning_effort request field.
	ReasoningEffort string
}

// ValidReasoningEfforts are the reasoning_effort values Ollama's
// OpenAI-compatible endpoint accepts.
var ValidReasoningEfforts = map[string]bool{
	"high":   true,
	"medium": true,
	"low":    true,
	"none":   true,
}

// ProviderCapabilities describes which generation-parameter knobs a wire
// protocol actually forwards to the backend. Fantasy silently drops an
// AgentOption/AgentCall field the active provider implementation doesn't
// read — it does not error — so a sampler configured against an
// unsupporting provider looks like it "did nothing" with no indication why.
type ProviderCapabilities struct {
	TopK            bool
	ReasoningEffort bool
}

// OpenAICompatCapabilities describes what OllamaProvider (backed by
// Fantasy's openaicompat provider, the only wire protocol Hufu currently
// speaks — see NewOllamaProvider) actually forwards. Ollama's
// OpenAI-compatible /v1/chat/completions endpoint has no top_k field but
// does support reasoning_effort.
var OpenAICompatCapabilities = ProviderCapabilities{TopK: false, ReasoningEffort: true}

var unsupportedSamplerWarnSeen sync.Map

// warnUnsupportedSamplerOnce logs once per (agent, param) pair that a
// configured sampler has no effect against the active provider, instead of
// leaving the mismatch to look like a silent no-op.
func warnUnsupportedSamplerOnce(agentName, param string) {
	key := agentName + ":" + param
	if _, loaded := unsupportedSamplerWarnSeen.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Printf("warning: agent %q configures %s, but Hufu's OpenAI-compatible provider (Ollama) does not forward it to the backend — the setting has no effect", agentName, param)
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
	// MemoryID is the stable worker identity for per-worker memory. When
	// empty, the runtime falls back to the normalized agent Name. Renaming
	// an agent while preserving MemoryID keeps its memory continuity.
	MemoryID string
	// Memory is the per-worker memory policy resolved from agent frontmatter,
	// team defaults, and built-in defaults (in that precedence order).
	Memory      WorkerMemoryPolicy
	Generation  GenerationParams
	ExtraModels []string
}

// WorkerMemoryMode controls whether a worker has private memory and how long
// it persists. The default is off — all memory remains shared.
type WorkerMemoryMode string

const (
	WorkerMemoryOff        WorkerMemoryMode = "off"
	WorkerMemorySession    WorkerMemoryMode = "session"
	WorkerMemoryPersistent WorkerMemoryMode = "persistent"
)

// WorkerMemoryPolicy is the resolved per-worker memory configuration. TTL
// fields are string durations (e.g. "168h", "0") parsed at load time; an
// empty string uses the built-in default, "0" means no expiry.
type WorkerMemoryPolicy struct {
	Mode          WorkerMemoryMode `yaml:"mode" json:"mode"`
	AutoRecall    bool             `yaml:"auto-recall" json:"auto_recall"`
	AutoSave      bool             `yaml:"auto-save" json:"auto_save"`
	MaxItems      int              `yaml:"max-items" json:"max_items"`
	MaxTokens     int              `yaml:"max-tokens" json:"max_tokens"`
	SessionTTL    string           `yaml:"session-ttl" json:"session_ttl"`
	PersistentTTL string           `yaml:"persistent-ttl" json:"persistent_ttl"`
}

// DefaultWorkerMemoryPolicy returns the built-in defaults: mode=off,
// auto-recall=true, auto-save=true, max-items=5, max-tokens=1500,
// session-ttl=168h, persistent-ttl=0 (no expiry).
func DefaultWorkerMemoryPolicy() WorkerMemoryPolicy {
	return WorkerMemoryPolicy{
		Mode:          WorkerMemoryOff,
		AutoRecall:    true,
		AutoSave:      true,
		MaxItems:      5,
		MaxTokens:     1500,
		SessionTTL:    "168h",
		PersistentTTL: "0",
	}
}

type TeamConfig struct {
	Name        string
	Description string
	// MaxRounds bounds normal coordinator progress; it is not a task retry
	// budget. Teams with a declared checkpoint workflow can set
	// MinimumCoordinatorRounds so validation rejects an undersized limit.
	MaxRounds                int
	MinimumCoordinatorRounds int
	MaxSteps                 int
	WorkspaceDir             string
	Timeout                  int64
	VerifyTimeout            int64
	MaxRetries               int
	Generation               GenerationParams
	Skills                   string
	SkillsExclude            string
	ProviderURL              string
	ProviderAPIKey           string
	Providers                map[string]config.ProviderConfig
	ModelList                []config.ModelEntry
	SidecarModel             string
	GuardModel               string
	JudgeModel               string
	PlanReviewerModel        string
	MaxConcurrent            int
	// MaxCoordinatorTurns bounds automatic continuation turns after a
	// coordinator step limit. Zero uses the built-in safe default.
	MaxCoordinatorTurns int
	// EscalateOnRetry makes every task retry escalate to the next stronger
	// model in ModelList (ordered weakest→strongest) by default.
	EscalateOnRetry bool
	Notify          notify.NotifyConfig
	AllowedPaths    []string
	RestrictedPath  string
	NoNet           bool
	ForceMCP        bool
	ProjectContext  bool
	Shell           string
	Vars            map[string]interface{}
	// WorkerContextSize bounds the injected project context (AGENTS.md) in
	// tokens, not characters — see coordinator's getWorkerContextSize.
	WorkerContextSize int
	ToolsAllowed      []string // List of explicitly allowed tools
	ToolsDenied       []string // List of tools never exposed to workers in this team
	Delegation        DelegationPolicy
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
	// a non-zero exit marks the run as not-accepted.
	Acceptance     string
	AcceptanceSpec *AcceptanceSpec
	// AcceptanceMode controls whether a failed acceptance command is advisory
	// or blocks the run. It is kept separate from AcceptanceSpec.Mode for
	// compatibility with the historical goal-mode field.
	AcceptanceMode string
	// Rollback is an optional shell command run on acceptance failure in unattended mode.
	Rollback string
	// ExecutionProfile specifies named defaults like strict-verification.
	ExecutionProfile string
	// GoalMode specifies outcome vs exploratory mode.
	GoalMode    string
	Reliability ReliabilityConfig
	// WorkerMemory is the team-level default worker memory policy. Individual
	// agents can override it via their frontmatter `memory:` block.
	WorkerMemory WorkerMemoryPolicy
}

// DelegationPolicy makes a team's coordinator dispatch contract executable.
// It is deliberately expressed in terms of configured worker names rather
// than provider, project, or task-domain concepts.
type DelegationPolicy struct {
	// AllowedWorkers limits which configured workers the coordinator may
	// dispatch. An empty list preserves the legacy behavior of allowing every
	// configured worker; a non-empty list is both a schema and runtime boundary.
	AllowedWorkers []string
	// InitialBatch is the ordered worker set required for the first delegation.
	// It is enforced only when RequireExactInitialBatch is true.
	InitialBatch []string
	// RequireExactInitialBatch rejects a first delegation whose cardinality or
	// worker order differs from InitialBatch.
	RequireExactInitialBatch bool
	// InitialCoordinatorTool, when non-empty, requires the coordinator's first
	// tool call to use this generic tool name. It prevents exploratory
	// coordinator actions from preceding a configured initial batch.
	InitialCoordinatorTool string
	// BindInitialTaskContracts replaces the execution and output contracts of
	// the configured first batch with same-named static team task contracts.
	// It lets a team freeze safety-critical initial checkpoints without making
	// the coordinator reproduce a long provider JSON object.
	BindInitialTaskContracts bool
	// BindTaskGoalContracts replaces the execution and output contracts of a
	// task that matches a static team task contract's agent and goal selector.
	// It is for later closed checkpoints whose task goal can be dynamic while
	// their execution shape must remain coordinator-independent.
	BindTaskGoalContracts bool
	// NoRedispatchAfterSuccess lists workers that may not be delegated again
	// after one of their tasks reached a successful terminal result.
	NoRedispatchAfterSuccess []string
	// ForbidContextFiles removes file-based coordinator-to-worker handoffs for
	// this team. Typed task results remain available, but a coordinator cannot
	// attach workspace/shared files to a delegated task. Teams that do not set
	// this keep the legacy context_files behavior.
	ForbidContextFiles bool
	// TaskGoalInvariants are optional, team-declared text boundaries checked
	// before a delegated task creates a TODO or starts a worker.  The runtime
	// only compares literals; provider- and project-specific content remains
	// in the team configuration.
	TaskGoalInvariants []TaskGoalInvariant
}

// TaskGoalInvariant constrains a task selected by worker and a required goal
// substring. Integrations supply the selector and contract details; the
// runtime only enforces generic text and execution-shape boundaries before a
// TODO is created or a worker can start.
type TaskGoalInvariant struct {
	Agent                    string             `yaml:"agent" json:"agent"`
	WhenGoalContains         string             `yaml:"when-goal-contains" json:"when_goal_contains"`
	RequiredLiterals         []string           `yaml:"required-literals" json:"required_literals"`
	ForbiddenLiterals        []string           `yaml:"forbidden-literals" json:"forbidden_literals"`
	RequiredToolSequence     []string           `yaml:"required-tool-sequence" json:"required_tool_sequence"`
	ForbiddenExecutionFields []string           `yaml:"forbidden-execution-fields" json:"forbidden_execution_fields"`
	RequiredTaskReference    *TaskGoalReference `yaml:"required-task-reference" json:"required_task_reference,omitempty"`
	// RequiredTaskReferences is the plural form for a sealed producer set. Each
	// reference must resolve to a distinct completed Todo before a consumer is
	// created. It is intentionally independent from result content: teams own
	// the typed agreement rules, while Hufu owns producer identity and state.
	RequiredTaskReferences []TaskGoalReference `yaml:"required-task-references" json:"required_task_references,omitempty"`
}

// TaskGoalReference makes one line-oriented goal field a runtime-validated
// reference to an already completed TODO. This keeps task identity separate
// from artifact identity and avoids trusting a coordinator's prose claim about
// where an opaque ID came from.
type TaskGoalReference struct {
	GoalPrefix   string `yaml:"goal-prefix" json:"goal_prefix"`
	Agent        string `yaml:"agent" json:"agent"`
	TaskContains string `yaml:"task-contains" json:"task_contains"`
}

// ReliabilityConfig bounds diagnostic and repair work that repeats without
// improving a mandatory outcome criterion.
type ReliabilityConfig struct {
	// Rollout selects the HF-AR-006 reliability-evaluation rollout stage:
	// shadow, warn-only, strict-opt-in, or strict-default. Empty preserves
	// the staged default resolved from whether an acceptance contract exists.
	Rollout                           string `yaml:"rollout" json:"rollout,omitempty"`
	MaxDiagnosticTasksWithoutProgress int    `yaml:"max-diagnostic-tasks-without-progress" json:"max_diagnostic_tasks_without_progress,omitempty"`
	MaxSameFailureFingerprint         int    `yaml:"max-same-failure-fingerprint" json:"max_same_failure_fingerprint,omitempty"`
	MaxRepairsPerCriterion            int    `yaml:"max-repairs-per-criterion" json:"max_repairs_per_criterion,omitempty"`
	// MaxSystemicFailureTasks is the threshold of distinct tasks that have
	// observed an equivalent (component, operation, class, digest) failure
	// before the anti-thrashing circuit breaker escalates to a systemic
	// defect. Defaults to 3 (§6.2). An explicit YAML 0 disables the
	// feature; MaxSystemicFailureTasksSet records whether the value was
	// explicitly set (so reliabilityConfig() can honor a zero override
	// rather than restoring the default). Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
	MaxSystemicFailureTasks    int  `yaml:"max-systemic-failure-tasks" json:"max_systemic_failure_tasks,omitempty"`
	MaxSystemicFailureTasksSet bool `yaml:"-" json:"-"`
	HardEnforcement            bool `yaml:"hard-enforcement" json:"hard_enforcement,omitempty"`
	WarnOnly                   bool `yaml:"warn-only" json:"warn_only,omitempty"`
	// VerifierLintMode controls the pre-dispatch verifier assertiveness
	// lint (§4.3). "error" (default) rejects non-asserting verifiers before
	// dispatch; "warn" emits a warning event but still dispatches; "off"
	// disables the lint entirely. Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
	VerifierLintMode string `yaml:"verifier-lint" json:"verifier_lint,omitempty"`
	// MaxTokensWithoutProgress is the no-progress budget on cumulative LLM
	// tokens consumed since the last objective criterion advancement (§8.1).
	// An explicit YAML 0 disables this one counter; unset restores the
	// default. MaxTokensWithoutProgressSet records whether the value was
	// explicitly set so reliabilityConfig() honors a zero override rather
	// than restoring the default. Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
	MaxTokensWithoutProgress    int  `yaml:"max-tokens-without-progress" json:"max_tokens_without_progress,omitempty"`
	MaxTokensWithoutProgressSet bool `yaml:"-" json:"-"`
	// MaxTokensPerAttempt bounds a single worker attempt before it can consume
	// the entire run budget. Zero disables this per-attempt circuit breaker.
	// It counts the *new* content an attempt accumulates — how much the request
	// context grows plus what the model generates — and not the conversation
	// resent on every step. Counting resent history instead makes the limit an
	// implicit step ceiling that shrinks as injected context grows, so a task
	// needing many small tool calls fails while consuming almost nothing.
	MaxTokensPerAttempt    int  `yaml:"max-tokens-per-attempt" json:"max_tokens_per_attempt,omitempty"`
	MaxTokensPerAttemptSet bool `yaml:"-" json:"-"`
	// MaxTurnsWithoutProgress is the no-progress budget on coordinator turns
	// since the last objective criterion advancement (§8.1). Same 0-disables
	// semantics as MaxTokensWithoutProgress. Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
	MaxTurnsWithoutProgress    int  `yaml:"max-turns-without-progress" json:"max_turns_without_progress,omitempty"`
	MaxTurnsWithoutProgressSet bool `yaml:"-" json:"-"`
	// MaxTasksWithoutProgress is the no-progress budget on tasks created
	// since the last objective criterion advancement (§8.1). Same 0-disables
	// semantics as MaxTokensWithoutProgress. Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
	MaxTasksWithoutProgress    int  `yaml:"max-tasks-without-progress" json:"max_tasks_without_progress,omitempty"`
	MaxTasksWithoutProgressSet bool `yaml:"-" json:"-"`
}

// DefaultReliabilityConfig returns default reliability anti-thrashing limits.
// By default, HardEnforcement is true and limits are enforced.
func DefaultReliabilityConfig() ReliabilityConfig {
	return ReliabilityConfig{
		MaxDiagnosticTasksWithoutProgress: 3,
		MaxSameFailureFingerprint:         2,
		MaxRepairsPerCriterion:            2,
		MaxSystemicFailureTasks:           3,
		HardEnforcement:                   true,
		VerifierLintMode:                  VerifierLintError,
		// No-progress budget defaults (§8.1). Sized generously so a healthy
		// run is never tripped, but a run that burns tokens/turns/tasks
		// without any objective criterion advancement is bounded. Refs:
		// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
		MaxTokensWithoutProgress: 2_000_000,
		MaxTurnsWithoutProgress:  8,
		MaxTasksWithoutProgress:  12,
		// A single runaway agent must not be able to consume the full run
		// budget before the task/round boundary observes it.
		MaxTokensPerAttempt: 500_000,
	}
}

// VerifierLintMode constants for ReliabilityConfig.VerifierLintMode.
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
const (
	VerifierLintError = "error"
	VerifierLintWarn  = "warn"
	VerifierLintOff   = "off"
)

// NormalizeVerifierLintMode returns a valid VerifierLintMode, defaulting
// to "error" when empty or unrecognized. This ensures the transitional
// switch always has a defined value even when YAML omits it.
func NormalizeVerifierLintMode(mode string) string {
	switch mode {
	case VerifierLintError, VerifierLintWarn, VerifierLintOff:
		return mode
	default:
		return VerifierLintError
	}
}

type VerificationType string

const (
	VerifyCommandExit VerificationType = "command_exit"
	VerifyFileExists  VerificationType = "file_exists"
	VerifyFileAbsent  VerificationType = "file_absent"
	VerifyJSONAssert  VerificationType = "json_assert"
)

type VerificationSpec struct {
	Type       VerificationType `json:"type,omitempty" yaml:"type,omitempty"`
	Mode       string           `json:"mode,omitempty" yaml:"mode,omitempty"`
	Command    string           `json:"command,omitempty" yaml:"command,omitempty"`
	Path       string           `json:"path,omitempty" yaml:"path,omitempty"`
	Assertions []JSONAssertion  `json:"assertions,omitempty" yaml:"assertions,omitempty"`
}

type JSONAssertion struct {
	Path   string `json:"path" yaml:"path"`
	Equals any    `json:"equals" yaml:"equals"`
}

type AcceptanceSpec struct {
	Commands                 []string              `yaml:"commands" json:"commands,omitempty"`
	RequiredArtifacts        []string              `yaml:"required-artifacts" json:"required_artifacts,omitempty"`
	RequireNoUnresolvedTasks bool                  `yaml:"require-no-unresolved-tasks" json:"require_no_unresolved_tasks,omitempty"`
	Mode                     string                `yaml:"mode,omitempty" json:"mode,omitempty"`
	Verifications            []VerificationSpec    `yaml:"verifications" json:"verifications,omitempty"`
	Criteria                 []AcceptanceCriterion `yaml:"criteria" json:"criteria,omitempty"`
}

// AcceptanceCriterion is a stable, named acceptance requirement. Existing
// commands/verifications remain supported and are translated by the team
// package for backward compatibility.
type AcceptanceCriterion struct {
	ID        string           `yaml:"id" json:"id"`
	Required  bool             `yaml:"required" json:"required"`
	DependsOn []string         `yaml:"depends-on" json:"depends_on,omitempty"`
	Verify    VerificationSpec `yaml:"verify" json:"verify"`
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

// OllamaShowContextTimeout bounds a single /api/show probe so an unreachable
// or slow endpoint cannot stall team startup; callers still control overall
// deadline via ctx.
const OllamaShowContextTimeout = 3 * time.Second

// DetectOllamaContextLength queries Ollama's native /api/show endpoint (not
// the OpenAI-compatible /v1 surface most of this package talks to) for
// modelName's configured context length. baseURL is the OpenAI-compatible
// base (e.g. "http://localhost:11434/v1"); the trailing /v1 is stripped to
// reach Ollama's native API root.
//
// Ollama reports context length as one of several architecture-prefixed
// model_info keys (e.g. "qwen2.context_length", "llama.context_length")
// rather than a single stable field, so this scans for any key ending in
// ".context_length" and returns the largest value found.
//
// Returns 0 with a nil error when no such key is present (e.g. talking to a
// non-Ollama OpenAI-compatible endpoint) — callers should treat that as "fall
// back to the static registry", not as failure.
func DetectOllamaContextLength(ctx context.Context, baseURL, apiKey, modelName string) (int, error) {
	root := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	reqBody, err := json.Marshal(map[string]string{"model": modelName})
	if err != nil {
		return 0, fmt.Errorf("encode /api/show request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("build /api/show request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: OllamaShowContextTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("query %s/api/show: %w", root, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("query %s/api/show: status %s", root, resp.Status)
	}
	var payload struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode /api/show response: %w", err)
	}
	best := 0
	for key, v := range payload.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		if f, ok := v.(float64); ok && int(f) > best {
			best = int(f)
		}
	}
	return best, nil
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
		if OpenAICompatCapabilities.TopK {
			opts = append(opts, fantasy.WithTopK(int64(topK)))
		} else {
			warnUnsupportedSamplerOnce(cfg.Def.Name, "top-k")
		}
	}
	if effort := cfg.Def.Generation.ReasoningEffort; effort != "" || cfg.TeamConfig.Generation.ReasoningEffort != "" {
		if effort == "" {
			effort = cfg.TeamConfig.Generation.ReasoningEffort
		}
		if ValidReasoningEfforts[effort] {
			opts = append(opts, fantasy.WithProviderOptions(openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
				ReasoningEffort: new(openai.ReasoningEffort(effort)),
			})))
		}
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
	"bash":     {"wait_for"},
	"sudo":     {"wait_for"},
	"terminal": {"terminal_start", "terminal_write", "terminal_read", "terminal_wait", "terminal_close", "terminal_list", "terminal_reconcile"},
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

// EffectiveToolNames returns the names of the tools SelectTools would hand to a
// model for toolNames. It is the authoritative answer to "what can this agent
// see?", and therefore the authoritative input to the runtime permission
// allowlist: a tool the model is shown but not granted is a trap, because the
// stream authorization gate aborts the whole attempt when the model calls it.
//
// Deriving the allowlist from this function instead of from the declared tool
// string is what keeps alwaysIncludeTools (which SelectTools forces in
// regardless of the declaration) from being silently unauthorized.
func EffectiveToolNames(allTools []fantasy.AgentTool, toolNames string) []string {
	selected := SelectTools(allTools, toolNames)
	names := make([]string, 0, len(selected))
	for _, t := range selected {
		if name := strings.TrimSpace(t.Info().Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// ResolveMaxSteps exposes the agent/team step-budget precedence so callers that
// need the number before CreateAgent runs (for example to reserve
// result-finalization headroom) compute the same value the agent will use.
func ResolveMaxSteps(agentSteps, teamSteps int) int {
	return resolveMaxSteps(agentSteps, teamSteps)
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
