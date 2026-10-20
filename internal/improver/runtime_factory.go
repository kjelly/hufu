package improver

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

// RuntimeSettings are host-owned provider dependencies. Credentials stay in
// memory and never enter the frozen artifact or comparison projection.
type RuntimeSettings struct {
	ProviderURL    string
	ProviderAPIKey string
	Providers      map[string]config.ProviderConfig
}

// RuntimeOverlay is the explicit bounded execution policy applied equally to
// both arms. Authored capabilities have already passed the strict profile gate;
// this overlay cannot make an unsupported authored team eligible.
type RuntimeOverlay struct {
	Version          int             `json:"version"`
	Profile          string          `json:"profile"`
	Generation       ProbeGeneration `json:"generation"`
	MaxSteps         int             `json:"max_steps"`
	MaxRetries       int             `json:"max_retries"`
	MaxConcurrent    int             `json:"max_concurrent"`
	TimeoutSeconds   int64           `json:"timeout_seconds"`
	MaxTokens        int64           `json:"max_tokens"`
	WorkerMemory     string          `json:"worker_memory"`
	MemoryLearning   string          `json:"memory_learning"`
	Semantic         string          `json:"semantic_retrieval"`
	AllowedTools     []string        `json:"allowed_tools"`
	ClosedSequence   []string        `json:"closed_sequence"`
	Grounded         bool            `json:"requires_grounded_result"`
	ForbidArtifacts  bool            `json:"forbid_artifacts"`
	ForbidReplay     bool            `json:"forbid_replay"`
	UsageAccounting  string          `json:"usage_accounting"`
	ResultSchemaHash string          `json:"result_schema_hash"`
}

// ProbeGeneration preserves the runtime values using durable telemetry names.
// The runtime's untagged GenerationParams wire format remains unchanged.
type ProbeGeneration struct {
	Model           string `json:"model"`
	ContextWindow   int    `json:"context_window"`
	Temperature     string `json:"temperature"`
	MaxTokens       string `json:"max_output"`
	TopP            string `json:"top_p"`
	TopK            string `json:"top_k"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// RuntimeFactory owns immutable input bytes. It never retains or reuses a
// coordinator, worker, provider cursor, session, or control workspace.
type RuntimeFactory struct {
	config      paired.Config
	variants    map[string]Tree
	roleSchemas map[string][]byte
	settings    RuntimeSettings
	overlays    map[string]RuntimeOverlay
}

func NewRuntimeFactory(prepared Prepared, settings RuntimeSettings) (*RuntimeFactory, error) {
	cfg, err := cloneRuntimeValue(prepared.Config)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateEnvelope(len(prepared.Fixture.Cases)); err != nil {
		return nil, err
	}
	if err := prepared.Fixture.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(settings.ProviderURL) == "" {
		return nil, errors.New("explicit runtime provider endpoint required")
	}
	baseline, ok := prepared.Variants["baseline"]
	if !ok {
		return nil, errors.New("frozen baseline is required")
	}
	// Re-materialization owns all normalized state and refuses stale public
	// Variant fields. Only the frozen private tree is authoritative.
	baseline, surfaces, err := materializeTree(baseline.Tree, cfg.Vars, cfg.Worker, baseline.BuildDigest, cfg.AllowedSurfaces)
	if err != nil {
		return nil, err
	}
	settings.Providers = maps.Clone(settings.Providers)
	factory := &RuntimeFactory{config: cfg, variants: map[string]Tree{},
		settings: settings, overlays: map[string]RuntimeOverlay{}, roleSchemas: map[string][]byte{}}
	for slot, variant := range prepared.Variants {
		owned, _, err := materializeTree(variant.Tree, cfg.Vars, cfg.Worker, baseline.BuildDigest, cfg.AllowedSurfaces)
		if err != nil {
			return nil, fmt.Errorf("variant %s: %w", slot, err)
		}
		if err := CompareCandidate(baseline, owned, surfaces); err != nil {
			return nil, fmt.Errorf("variant %s: %w", slot, err)
		}
		files := make(map[string][]byte, len(owned.Tree.files))
		for name, raw := range owned.Tree.files {
			files[name] = bytes.Clone(raw)
		}
		factory.variants[slot] = Tree{files: files}
	}
	for role, model := range cfg.Models {
		schema := prepared.Schema
		if role != "executor" {
			schema, err = paired.RoleSchema(role)
			if err != nil {
				return nil, err
			}
		}
		compiled, err := team.CompileResultContractDocument("paired-result.json", schema)
		if err != nil {
			return nil, err
		}
		factory.roleSchemas[role] = bytes.Clone(schema)
		generation, err := resolveProbeGeneration(baseline, cfg, model)
		if err != nil {
			return nil, fmt.Errorf("role %s: %w", role, err)
		}
		factory.overlays[role] = RuntimeOverlay{
			Version: 1, Profile: cfg.Profile, Generation: ProbeGeneration(generation), MaxSteps: 2, MaxRetries: 0, MaxConcurrent: 1,
			TimeoutSeconds: cfg.Limits.PerStageSeconds, MaxTokens: cfg.Limits.PerStageTokens,
			WorkerMemory: "off", MemoryLearning: "off", Semantic: "off",
			AllowedTools: []string{"submit_result"}, ClosedSequence: []string{"submit_result"},
			Grounded: true, ForbidArtifacts: true, ForbidReplay: true, UsageAccounting: "canonical_cost_events", ResultSchemaHash: compiled.SchemaSHA256,
		}
	}
	return factory, nil
}

func cloneRuntimeValue[T any](input T) (T, error) {
	var result T
	raw, err := json.Marshal(input)
	if err == nil {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}

func resolveProbeGeneration(baseline Variant, cfg paired.Config, model paired.Model) (agent.GenerationParams, error) {
	worker := baseline.Agents[cfg.Worker].Generation
	teamGeneration := baseline.Config.Generation
	pick := func(worker, team, fallback string) string {
		if worker != "" {
			return worker
		}
		if team != "" {
			return team
		}
		return fallback
	}
	generation := agent.GenerationParams{
		Model: model.Model, ContextWindow: worker.ContextWindow,
		Temperature:     pick(worker.Temperature, teamGeneration.Temperature, "0.2"),
		TopP:            pick(worker.TopP, teamGeneration.TopP, "0.9"),
		TopK:            pick(worker.TopK, teamGeneration.TopK, ""),
		ReasoningEffort: pick(worker.ReasoningEffort, teamGeneration.ReasoningEffort, ""),
		MaxTokens:       pick(worker.MaxTokens, teamGeneration.MaxTokens, "4096"),
	}
	if cfg.ExecutionMode == "fixture" {
		generation.Model = "local/scripted"
	}
	if generation.ContextWindow <= 0 {
		generation.ContextWindow = teamGeneration.ContextWindow
	}
	if generation.ContextWindow <= 0 && cfg.ExecutionMode == "fixture" {
		generation.ContextWindow = 32768
	}
	if generation.ContextWindow <= 0 {
		return agent.GenerationParams{}, errors.New("structured_only requires an explicit positive context-window for llm mode")
	}
	output, err := strconv.ParseInt(generation.MaxTokens, 10, 64)
	if err != nil || output <= 0 {
		return agent.GenerationParams{}, errors.New("positive integer max-tokens required")
	}
	// A bounded overlay caps output without enlarging an authored smaller
	// value. Input and protocol-repair reservations share the stage budget.
	output = min(output, cfg.Limits.PerStageTokens/2, int64(generation.ContextWindow)/2)
	if output <= 0 {
		return agent.GenerationParams{}, errors.New("stage token budget cannot fit an output reservation")
	}
	generation.MaxTokens = strconv.FormatInt(output, 10)
	return generation, nil
}

type probeRuntime struct {
	coordinator *team.Coordinator
	session     *team.TeamSession
	overlay     RuntimeOverlay
	policy      PolicyProjection
	root        string
	closed      atomic.Bool
}

func (r *probeRuntime) close() error {
	if r.closed.Load() {
		return nil
	}
	err := r.coordinator.Close()
	if err == nil {
		r.closed.Store(true)
	}
	return err
}

// open requires a new attempt directory under the controller-owned child
// area. It never opens a prior child for execution; recovery reads evidence.
func (f *RuntimeFactory) open(root, slot, role string, boundary agent.ProviderBoundaryFactory) (result *probeRuntime, retErr error) {
	tree, ok := f.variants[slot]
	if !ok {
		return nil, errors.New("runtime variant is not frozen")
	}
	overlay, ok := f.overlays[role]
	if !ok {
		return nil, errors.New("runtime role is not frozen")
	}
	if (f.config.ExecutionMode == "fixture") != (boundary != nil) {
		return nil, errors.New("fixture requires an explicit offline boundary; llm requires the ordinary runtime boundary")
	}
	parent, err := openSourceRoot(filepath.Dir(root))
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, parent.Close()) }()
	if err := parent.Mkdir(filepath.Base(root), 0o700); err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	teamRoot, control, subject := filepath.Join(root, "team"), filepath.Join(root, "control"), filepath.Join(root, "subject")
	if err := tree.WriteNew(teamRoot); err != nil {
		return nil, err
	}
	for _, dir := range []string{control, subject} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return nil, err
		}
	}
	session, err := team.LoadTeamWithOptions(teamRoot, f.config.Vars, nil, team.DefaultProviderRegistry,
		team.TeamLoadOptions{SkillSearchPaths: []string{filepath.Join(teamRoot, "skills")}})
	if err != nil {
		return nil, err
	}
	if err := validateEffectiveProfile(session, f.config.Worker); err != nil {
		return nil, err
	}
	session.Workspace = control
	if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
		return nil, err
	}
	applyRuntimeOverlay(session, overlay, f.settings)
	compiled, err := team.CompileResultContractDocument("paired-result.json", f.roleSchemas[role])
	if err != nil {
		return nil, err
	}
	if authored, exists := session.AgentResultContracts[strings.ToLower(f.config.Worker)]; role == "executor" && exists && authored.SchemaSHA256 != compiled.SchemaSHA256 {
		return nil, errors.New("unsupported_profile: worker result contract differs from frozen fixture schema")
	}
	if role != "executor" {
		worker := exactWorker(session, f.config.Worker)
		worker.System = refinementSystemPrompt
		worker.Capabilities = team.ExtractCapabilitiesFromSystem(worker.System)
		worker.Skills = ""
	}
	session.ResultContracts = map[string]*team.CompiledResultContract{compiled.ID: compiled}
	session.AgentResultContracts = map[string]team.ResultContractRef{strings.ToLower(f.config.Worker): {
		ID: compiled.ID, SchemaSHA256: compiled.SchemaSHA256, RequireStructured: true,
	}}
	var options []team.CoordinatorOption
	if boundary != nil {
		options = append(options, team.WithProviderBoundaryFactory(boundary))
	}
	c, err := team.NewCoordinator(session, f.settings.ProviderURL, f.settings.ProviderAPIKey, nil, nil, team.RoleModels{},
		1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false, options...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, c.Close())
		}
	}()
	c.SetFreshSession(true)
	c.SetSessionData(team.NewSession())
	c.SetBudget(overlay.TimeoutSeconds, overlay.MaxTokens)
	if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
		return nil, err
	}
	roots, err := RegisterRoots(map[string]string{"subject_root": subject, "team_root": teamRoot, "control_root": control})
	if err != nil {
		return nil, err
	}
	policy, err := ProjectPolicy(c.ExecutionPolicySnapshot(), roots)
	if err != nil {
		return nil, err
	}
	return &probeRuntime{coordinator: c, session: session, overlay: overlay, policy: policy, root: root}, nil
}

func applyRuntimeOverlay(session *team.TeamSession, overlay RuntimeOverlay, settings RuntimeSettings) {
	cfg := &session.Config
	cfg.Generation = agent.GenerationParams(overlay.Generation)
	cfg.WorkerModel, cfg.CoordinatorModel = overlay.Generation.Model, overlay.Generation.Model
	// hufu-local is the subagent-provider spelling accepted by the paired
	// contract; the execution registry's LLM backend identity is ollama.
	cfg.DefaultLLMBackend = execution.OllamaBackendName
	cfg.MaxRetries, cfg.MaxConcurrent, cfg.MaxSteps = overlay.MaxRetries, overlay.MaxConcurrent, overlay.MaxSteps
	cfg.Timeout, cfg.MaxWallClock, cfg.MaxTotalTokens = overlay.TimeoutSeconds, overlay.TimeoutSeconds, overlay.MaxTokens
	cfg.WorkerMemory.Mode, cfg.MemoryLearning.Mode, cfg.SemanticRetrieval.Mode = agent.WorkerMemoryOff, agent.MemoryLearningOff, "off"
	cfg.ToolsAllowed, cfg.NoNet, cfg.Unattended = []string{"submit_result"}, true, true
	cfg.Providers = maps.Clone(settings.Providers)
	cfg.ProviderURL, cfg.ProviderAPIKey = settings.ProviderURL, settings.ProviderAPIKey
	// Record every provider invocation and explicit usage presence even when
	// the host has no dollar price catalog. Unknown price is not zero price.
	cfg.CostPolicy = cost.RunPolicy{Configured: true, UnknownPricePolicy: cost.UnknownPriceAllow}
	for _, def := range session.Agents {
		def.Generation = agent.GenerationParams(overlay.Generation)
		def.Memory.Mode = agent.WorkerMemoryOff
		def.MaxRetries, def.MaxSteps, def.Timeout = overlay.MaxRetries, overlay.MaxSteps, overlay.TimeoutSeconds
	}
}

func (r *probeRuntime) task(worker, input string, verifier paired.Verifier) team.TaskDef {
	spec := verifier.RuntimeSpec()
	replay := false
	return team.TaskDef{
		Agent: worker, Goal: input, SideEffect: team.SideEffectNone, VerifySpec: &spec,
		Execution: team.ExecutionContract{
			Kind: team.ExecutionKindInline, RequiresResult: true, RequiresGroundedResult: true, RequiresVerification: true,
			ForbidArtifacts: true, AllowsReplay: &replay, ToolSequence: []string{"submit_result"},
		},
	}
}
