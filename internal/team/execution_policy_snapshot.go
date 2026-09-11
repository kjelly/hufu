package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// executionPolicySnapshotVersion is incremented only when the canonical
// fingerprint input changes. Older sessions remain readable, but cannot resume
// interrupted work without an equivalent current snapshot.
const executionPolicySnapshotVersion = 3

// ExecutionPolicySnapshot is the durable, secret-free execution admission
// record. It freezes all settings that can change scheduling, routing, or an
// external worker's execution world before the first task or provider call.
//
// Environment values are represented only by a presence bit and a SHA-256
// digest. The actual allowlisted child environment remains process-local and is
// never written to session.json or the event store.
type ExecutionPolicySnapshot struct {
	Version           int                              `json:"version"`
	TeamMaxConcurrent int                              `json:"team_max_concurrent"`
	DefaultLLMBackend string                           `json:"default_llm_backend"`
	Backends          []ExecutionBackendPolicySnapshot `json:"backends"`
	ModelRoutes       []ExecutionModelRouteSnapshot    `json:"model_routes"`
	ExecutionWorlds   []ExecutionWorldPolicySnapshot   `json:"execution_worlds"`
	ConfigurationHash string                           `json:"configuration_hash"`
}

// ExecutionBackendPolicySnapshot records one canonical backend limiter.
type ExecutionBackendPolicySnapshot struct {
	Backend       string `json:"backend"`
	Kind          string `json:"kind"`
	ProviderKey   string `json:"provider_key,omitempty"`
	MaxConcurrent int    `json:"max_concurrent"`
	// IdentityHash is a redacted digest of the backend configuration that
	// determines where or how work executes. It covers an LLM transport's
	// upstream/type/credential revision or a Codex app-server's launch and
	// protocol configuration without retaining raw endpoint, credential, or
	// command values in durable state.
	IdentityHash string `json:"identity_hash"`
}

// ExecutionModelRouteSnapshot records the resolved target for every configured
// model that can be selected during the run, including auxiliary role models.
type ExecutionModelRouteSnapshot struct {
	Model          string `json:"model"`
	Backend        string `json:"backend"`
	ProviderKey    string `json:"provider_key,omitempty"`
	LegacyProvider string `json:"legacy_provider,omitempty"`
}

// ExecutionWorldPolicySnapshot records the execution-world and allowlisted
// child environment for one external backend without persisting raw values.
type ExecutionWorldPolicySnapshot struct {
	Backend                    string                                 `json:"backend"`
	ExecutionWorld             string                                 `json:"execution_world"`
	ProjectRootHash            string                                 `json:"project_root_hash"`
	CoordinatorNetworkDisabled bool                                   `json:"coordinator_network_disabled"`
	AgentNetworkPolicies       []ExecutionAgentNetworkPolicySnapshot  `json:"agent_network_policies,omitempty"`
	InheritEnv                 []ExecutionEnvironmentVariableSnapshot `json:"inherit_env,omitempty"`
}

// ExecutionAgentNetworkPolicySnapshot records the effective task-network
// boundary for a configured agent that can route to an external backend. It
// is deliberately an effective boolean: both coordinator-wide and
// agent-specific no-net settings are resolved before the first task.
type ExecutionAgentNetworkPolicySnapshot struct {
	Agent           string `json:"agent"`
	NetworkDisabled bool   `json:"network_disabled"`
}

// ExecutionEnvironmentVariableSnapshot is a redacted, deterministic record of
// one allowlisted environment variable. HOME and CODEX_HOME are represented by
// the same structure as every other inherited variable so changing either is an
// admission failure before another Codex process can start.
type ExecutionEnvironmentVariableSnapshot struct {
	Name      string `json:"name"`
	Present   bool   `json:"present"`
	ValueHash string `json:"value_hash,omitempty"`
}

// executionPolicyState combines the durable public snapshot with process-local
// values needed to launch an external backend. It is created once by
// NewCoordinator and never mutated; accessors return defensive copies.
type executionPolicyState struct {
	snapshot             *ExecutionPolicySnapshot
	backendByName        map[string]ExecutionBackendPolicySnapshot
	modelRouteByModel    map[string]ExecutionModelRouteSnapshot
	environmentByBackend map[string][]string
	codexWorldByBackend  map[string]executionPolicyCodexWorldState
}

// executionPolicyCodexWorldState retains the root path needed to run a child
// process without placing that path in durable state. Its digest is persisted
// in ExecutionWorldPolicySnapshot. Network policy is durable because it has no
// secret material, but is copied here so execution does not consult mutable
// coordinator or agent configuration after admission.
type executionPolicyCodexWorldState struct {
	projectRoot                string
	coordinatorNetworkDisabled bool
	networkDisabledByAgent     map[string]bool
}

// executionPolicyModelInput identifies a model at the configuration source
// that selects it. Bare model IDs are not globally unique routes: a worker's
// subagent-provider can deliberately send a bare ID to an external backend,
// while the same ID can be used by a coordinator-side LLM role. Keeping the
// compatibility provider in the snapshot makes that distinction durable and
// causes a changed agent/team provider binding to fail admission.
type executionPolicyModelInput struct {
	model          string
	legacyProvider string
}

func configuredCodexProviderConfigs(c *Coordinator) map[string]agent.SubagentProviderConfig {
	codexConfig := agent.SubagentProviderConfig{
		Type:           codexAppServerProviderType,
		Command:        []string{"codex", "app-server"},
		Protocol:       "app-server-v2",
		ExecutionWorld: localExecutionWorldName,
		InheritEnv:     []string{"PATH", "CODEX_HOME"},
		MaxConcurrent:  codexDefaultMaxConcurrent,
	}
	providers := map[string]agent.SubagentProviderConfig{
		codexSubagentProviderName: codexConfig,
	}
	if c == nil || c.session == nil {
		return providers
	}
	if configured, ok := c.session.Config.SubagentProviders[codexSubagentProviderName]; ok && configured.Type == codexAppServerProviderType {
		providers[codexSubagentProviderName] = mergeCodexBackendConfig(codexConfig, configured)
	}
	for name, cfg := range c.session.Config.SubagentProviders {
		if name == codexSubagentProviderName || execution.IsOllamaBackend(name) || cfg.Type != codexAppServerProviderType {
			continue
		}
		providers[execution.CanonicalTargetBackendName(name)] = enforceCodexBackendConcurrency(cfg)
	}
	return providers
}

func (c *Coordinator) executionPolicyModelInputs() []executionPolicyModelInput {
	if c == nil || c.session == nil {
		return nil
	}
	seen := make(map[string]struct{})
	inputs := make([]executionPolicyModelInput, 0)
	add := func(model, legacyProvider string) {
		model = strings.TrimSpace(model)
		legacyProvider = strings.TrimSpace(legacyProvider)
		if model == "" {
			return
		}
		key := model + "\x00" + legacyProvider
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		inputs = append(inputs, executionPolicyModelInput{model: model, legacyProvider: legacyProvider})
	}

	for _, def := range c.session.Agents {
		if def == nil {
			continue
		}
		legacyProvider := strings.TrimSpace(def.SubagentProvider)
		if legacyProvider == "" {
			legacyProvider = c.session.Config.SubagentProviderDefault
		}
		add(c.resolveAgentModel(def, ""), legacyProvider)
		for _, model := range def.ExtraModels {
			add(model, legacyProvider)
		}
	}

	for _, model := range c.modelList {
		add(model.ID, "")
	}
	add(c.sidecarModel, "")
	add(c.guardModel, "")
	add(c.judgeModel, "")
	add(c.planReviewerModel, "")
	add(c.session.Config.WorkerModel, "")
	add(c.session.Config.CoordinatorModel, "")
	add(c.session.Config.SidecarModel, "")
	add(c.session.Config.GuardModel, "")
	add(c.session.Config.JudgeModel, "")
	add(c.session.Config.PlanReviewerModel, "")
	add(c.session.Config.Generation.Model, "")

	sort.Slice(inputs, func(i, j int) bool {
		if inputs[i].model != inputs[j].model {
			return inputs[i].model < inputs[j].model
		}
		return inputs[i].legacyProvider < inputs[j].legacyProvider
	})
	return inputs
}

func newExecutionPolicyState(c *Coordinator) (*executionPolicyState, error) {
	if c == nil || c.session == nil || c.providerManager == nil {
		return nil, fmt.Errorf("execution policy snapshot requires an initialized coordinator")
	}
	defaultBackend := execution.OllamaBackendName
	if configured := execution.CanonicalTargetBackendName(c.session.Config.DefaultLLMBackend); configured != "" {
		defaultBackend = configured
	}
	snapshot := &ExecutionPolicySnapshot{
		Version:           executionPolicySnapshotVersion,
		TeamMaxConcurrent: c.maxConcurrent,
		DefaultLLMBackend: defaultBackend,
	}
	state := &executionPolicyState{
		snapshot:             snapshot,
		backendByName:        make(map[string]ExecutionBackendPolicySnapshot),
		modelRouteByModel:    make(map[string]ExecutionModelRouteSnapshot),
		environmentByBackend: make(map[string][]string),
		codexWorldByBackend:  make(map[string]executionPolicyCodexWorldState),
	}

	for _, ref := range c.providerManager.EffectiveProviderRefs() {
		backend := execution.CanonicalTargetBackendName(ref.Name)
		if backend == "" {
			continue
		}
		policy, err := c.providerManager.ResolveProviderExecutionPolicy(backend + "/execution-policy")
		if err != nil {
			return nil, fmt.Errorf("resolve execution policy for backend %q: %w", backend, err)
		}
		identity, err := c.providerManager.ResolveProviderExecutionIdentity(backend + "/execution-policy")
		if err != nil {
			return nil, fmt.Errorf("resolve execution identity for backend %q: %w", backend, err)
		}
		if identity.ProviderKey != policy.ProviderKey {
			return nil, fmt.Errorf("execution identity provider mismatch for backend %q: identity=%q policy=%q", backend, identity.ProviderKey, policy.ProviderKey)
		}
		state.backendByName[backend] = ExecutionBackendPolicySnapshot{
			Backend: backend, Kind: string(execution.BackendKindLLM), ProviderKey: policy.ProviderKey, MaxConcurrent: policy.MaxConcurrent, IdentityHash: identity.ConfigurationHash,
		}
	}

	for backend, cfg := range configuredCodexProviderConfigs(c) {
		backend = execution.CanonicalTargetBackendName(backend)
		environment, variables := captureExecutionPolicyEnvironment(cfg.InheritEnv)
		agentNetworkPolicies, networkDisabledByAgent, err := c.executionPolicyCodexAgentNetworkPolicies(backend)
		if err != nil {
			return nil, err
		}
		state.environmentByBackend[backend] = environment
		state.codexWorldByBackend[backend] = executionPolicyCodexWorldState{
			projectRoot:                c.projectDir,
			coordinatorNetworkDisabled: c.noNet,
			networkDisabledByAgent:     networkDisabledByAgent,
		}
		state.backendByName[backend] = ExecutionBackendPolicySnapshot{
			Backend: backend, Kind: string(execution.BackendKindAgent), MaxConcurrent: cfg.MaxConcurrent, IdentityHash: executionPolicyCodexIdentityHash(backend, cfg),
		}
		snapshot.ExecutionWorlds = append(snapshot.ExecutionWorlds, ExecutionWorldPolicySnapshot{
			Backend:                    backend,
			ExecutionWorld:             cfg.ExecutionWorld,
			ProjectRootHash:            executionPolicyValueHash("codex_project_root", c.projectDir),
			CoordinatorNetworkDisabled: c.noNet,
			AgentNetworkPolicies:       agentNetworkPolicies,
			InheritEnv:                 variables,
		})
	}

	for _, input := range c.executionPolicyModelInputs() {
		target, err := c.resolveCanonicalTaskTarget(input.model, input.legacyProvider)
		if err != nil {
			return nil, fmt.Errorf("resolve execution policy model route for %q: %w", input.model, err)
		}
		backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
		if err != nil {
			return nil, fmt.Errorf("resolve execution policy backend for %q: %w", input.model, err)
		}
		route := ExecutionModelRouteSnapshot{
			Model:          input.model,
			Backend:        execution.CanonicalTargetBackendName(target.Backend),
			LegacyProvider: execution.CanonicalTargetBackendName(input.legacyProvider),
		}
		if backend.Kind() == execution.BackendKindLLM {
			providerModel := target.Backend + "/" + target.Model
			policy, resolveErr := c.providerManager.ResolveProviderExecutionPolicy(providerModel)
			if resolveErr != nil {
				return nil, fmt.Errorf("resolve execution policy provider for %q: %w", input.model, resolveErr)
			}
			route.ProviderKey = policy.ProviderKey
		}
		if input.legacyProvider == "" {
			state.modelRouteByModel[input.model] = route
		}
		snapshot.ModelRoutes = append(snapshot.ModelRoutes, route)
	}

	for _, backend := range state.backendByName {
		snapshot.Backends = append(snapshot.Backends, backend)
	}
	sort.Slice(snapshot.Backends, func(i, j int) bool { return snapshot.Backends[i].Backend < snapshot.Backends[j].Backend })
	sort.Slice(snapshot.ModelRoutes, func(i, j int) bool {
		if snapshot.ModelRoutes[i].Model != snapshot.ModelRoutes[j].Model {
			return snapshot.ModelRoutes[i].Model < snapshot.ModelRoutes[j].Model
		}
		return snapshot.ModelRoutes[i].LegacyProvider < snapshot.ModelRoutes[j].LegacyProvider
	})
	sort.Slice(snapshot.ExecutionWorlds, func(i, j int) bool { return snapshot.ExecutionWorlds[i].Backend < snapshot.ExecutionWorlds[j].Backend })
	for i := range snapshot.ExecutionWorlds {
		sort.Slice(snapshot.ExecutionWorlds[i].AgentNetworkPolicies, func(j, k int) bool {
			return snapshot.ExecutionWorlds[i].AgentNetworkPolicies[j].Agent < snapshot.ExecutionWorlds[i].AgentNetworkPolicies[k].Agent
		})
		sort.Slice(snapshot.ExecutionWorlds[i].InheritEnv, func(j, k int) bool {
			return snapshot.ExecutionWorlds[i].InheritEnv[j].Name < snapshot.ExecutionWorlds[i].InheritEnv[k].Name
		})
	}
	configurationHash, err := executionPolicyConfigurationHash(snapshot)
	if err != nil {
		return nil, err
	}
	snapshot.ConfigurationHash = configurationHash
	return state, nil
}

// executionPolicyCodexAgentNetworkPolicies resolves the task-network setting
// for every configured agent whose current model route uses backend. The
// result is keyed by the agent definition's stable name because that is the
// identity carried into AttemptRequest.
func (c *Coordinator) executionPolicyCodexAgentNetworkPolicies(backend string) ([]ExecutionAgentNetworkPolicySnapshot, map[string]bool, error) {
	if c == nil || c.session == nil {
		return nil, nil, fmt.Errorf("execution policy Codex network policies require an initialized coordinator")
	}
	backend = execution.CanonicalTargetBackendName(backend)
	byAgent := make(map[string]bool)
	for configuredName, def := range c.session.Agents {
		if def == nil {
			continue
		}
		agentName := strings.TrimSpace(def.Name)
		if agentName == "" {
			agentName = configuredName
		}
		legacyProvider := strings.TrimSpace(def.SubagentProvider)
		if legacyProvider == "" {
			legacyProvider = c.session.Config.SubagentProviderDefault
		}
		models := make([]string, 0, 1+len(def.ExtraModels)+len(c.modelList))
		models = append(models, c.resolveAgentModel(def, ""))
		models = append(models, def.ExtraModels...)
		for _, model := range c.modelList {
			models = append(models, model.ID)
		}
		for _, model := range models {
			target, err := c.resolveCanonicalTaskTarget(model, legacyProvider)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve Codex network policy for agent %q model %q: %w", configuredName, model, err)
			}
			if execution.CanonicalTargetBackendName(target.Backend) != backend {
				continue
			}
			networkDisabled := c.noNet || def.NoNet
			if prior, exists := byAgent[agentName]; exists && prior != networkDisabled {
				return nil, nil, fmt.Errorf("conflicting Codex network policy for agent %q", agentName)
			}
			byAgent[agentName] = networkDisabled
		}
	}
	policies := make([]ExecutionAgentNetworkPolicySnapshot, 0, len(byAgent))
	for agentName, networkDisabled := range byAgent {
		policies = append(policies, ExecutionAgentNetworkPolicySnapshot{Agent: agentName, NetworkDisabled: networkDisabled})
	}
	return policies, byAgent, nil
}

func executionPolicyCodexIdentityHash(backend string, cfg agent.SubagentProviderConfig) string {
	identity := struct {
		Backend            string   `json:"backend"`
		Type               string   `json:"type"`
		Command            []string `json:"command"`
		Protocol           string   `json:"protocol"`
		StartupTimeout     string   `json:"startup_timeout"`
		InterruptGrace     string   `json:"interrupt_grace"`
		ShutdownGrace      string   `json:"shutdown_grace"`
		MaxEventBytes      int64    `json:"max_event_bytes"`
		MaxTranscriptBytes int64    `json:"max_transcript_bytes"`
		ExecutionWorld     string   `json:"execution_world"`
		InheritEnv         []string `json:"inherit_env"`
		MaxConcurrent      int      `json:"max_concurrent"`
	}{
		Backend:            backend,
		Type:               cfg.Type,
		Command:            slices.Clone(cfg.Command),
		Protocol:           cfg.Protocol,
		StartupTimeout:     cfg.StartupTimeout,
		InterruptGrace:     cfg.InterruptGrace,
		ShutdownGrace:      cfg.ShutdownGrace,
		MaxEventBytes:      cfg.MaxEventBytes,
		MaxTranscriptBytes: cfg.MaxTranscriptBytes,
		ExecutionWorld:     cfg.ExecutionWorld,
		InheritEnv:         slices.Clone(cfg.InheritEnv),
		MaxConcurrent:      cfg.MaxConcurrent,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		// The identity contains only scalar values and string slices, so this is
		// unreachable. Keep the fallback deterministic if that ever changes.
		encoded = []byte(backend)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func captureExecutionPolicyEnvironment(allowlist []string) ([]string, []ExecutionEnvironmentVariableSnapshot) {
	seen := make(map[string]struct{}, len(allowlist))
	environment := make([]string, 0, len(allowlist))
	variables := make([]ExecutionEnvironmentVariableSnapshot, 0, len(allowlist))
	for _, rawName := range allowlist {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		value, present := os.LookupEnv(name)
		variable := ExecutionEnvironmentVariableSnapshot{Name: name, Present: present}
		if present {
			variable.ValueHash = executionPolicyValueHash(name, value)
			environment = append(environment, name+"="+value)
		}
		variables = append(variables, variable)
	}
	return environment, variables
}

func executionPolicyValueHash(name, value string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + value))
	return hex.EncodeToString(sum[:])
}

func executionPolicyConfigurationHash(snapshot *ExecutionPolicySnapshot) (string, error) {
	if snapshot == nil {
		return "", fmt.Errorf("execution policy snapshot is nil")
	}
	copySnapshot := cloneExecutionPolicySnapshot(snapshot)
	copySnapshot.ConfigurationHash = ""
	encoded, err := json.Marshal(copySnapshot)
	if err != nil {
		return "", fmt.Errorf("marshal execution policy snapshot: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func cloneExecutionPolicySnapshot(snapshot *ExecutionPolicySnapshot) *ExecutionPolicySnapshot {
	if snapshot == nil {
		return nil
	}
	clone := *snapshot
	clone.Backends = slices.Clone(snapshot.Backends)
	clone.ModelRoutes = slices.Clone(snapshot.ModelRoutes)
	clone.ExecutionWorlds = make([]ExecutionWorldPolicySnapshot, len(snapshot.ExecutionWorlds))
	for i := range snapshot.ExecutionWorlds {
		clone.ExecutionWorlds[i] = snapshot.ExecutionWorlds[i]
		clone.ExecutionWorlds[i].AgentNetworkPolicies = slices.Clone(snapshot.ExecutionWorlds[i].AgentNetworkPolicies)
		clone.ExecutionWorlds[i].InheritEnv = slices.Clone(snapshot.ExecutionWorlds[i].InheritEnv)
	}
	return &clone
}

func validateExecutionPolicySnapshot(snapshot *ExecutionPolicySnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("execution policy snapshot is missing")
	}
	if snapshot.Version != executionPolicySnapshotVersion {
		return fmt.Errorf("execution policy snapshot version %d is unsupported", snapshot.Version)
	}
	if len(snapshot.Backends) == 0 {
		return fmt.Errorf("execution policy snapshot has no backends")
	}
	for _, backend := range snapshot.Backends {
		if strings.TrimSpace(backend.Backend) == "" || strings.TrimSpace(backend.IdentityHash) == "" {
			return fmt.Errorf("execution policy snapshot backend identity is incomplete")
		}
	}
	for _, world := range snapshot.ExecutionWorlds {
		if strings.TrimSpace(world.Backend) == "" || strings.TrimSpace(world.ProjectRootHash) == "" {
			return fmt.Errorf("execution policy snapshot execution world is incomplete")
		}
		seenAgents := make(map[string]struct{}, len(world.AgentNetworkPolicies))
		for _, policy := range world.AgentNetworkPolicies {
			if strings.TrimSpace(policy.Agent) == "" {
				return fmt.Errorf("execution policy snapshot agent network policy is incomplete")
			}
			if _, duplicate := seenAgents[policy.Agent]; duplicate {
				return fmt.Errorf("execution policy snapshot repeats network policy for agent %q", policy.Agent)
			}
			seenAgents[policy.Agent] = struct{}{}
		}
	}
	if strings.TrimSpace(snapshot.ConfigurationHash) == "" {
		return fmt.Errorf("execution policy snapshot configuration hash is missing")
	}
	hash, err := executionPolicyConfigurationHash(snapshot)
	if err != nil {
		return err
	}
	if hash != snapshot.ConfigurationHash {
		return fmt.Errorf("execution policy snapshot configuration hash does not match its contents")
	}
	return nil
}

func (s *executionPolicyState) teamMaxConcurrent() int {
	if s == nil || s.snapshot == nil {
		return 0
	}
	return s.snapshot.TeamMaxConcurrent
}

func (c *Coordinator) teamConcurrencyLimit() int {
	if c != nil && c.executionPolicy != nil {
		return c.executionPolicy.teamMaxConcurrent()
	}
	if c == nil {
		return 0
	}
	// Compatibility for minimal unit-test coordinators that deliberately do not
	// construct a production execution boundary.
	return c.maxConcurrent
}

// executionPolicyDefaultLLMBackend is the sole default-routing source once a
// production coordinator has constructed its policy state. It prevents a
// later mutation of TeamConfig from silently sending a bare model selector to
// a different backend mid-run.
func (c *Coordinator) executionPolicyDefaultLLMBackend() string {
	if c != nil && c.executionPolicy != nil && c.executionPolicy.snapshot != nil && c.executionPolicy.snapshot.DefaultLLMBackend != "" {
		return c.executionPolicy.snapshot.DefaultLLMBackend
	}
	if c != nil && c.session != nil {
		if backend := execution.CanonicalTargetBackendName(c.session.Config.DefaultLLMBackend); backend != "" {
			return backend
		}
	}
	return execution.OllamaBackendName
}

func (s *executionPolicyState) providerPolicyForModel(model string) (ExecutionBackendPolicySnapshot, bool) {
	if s == nil || s.snapshot == nil {
		return ExecutionBackendPolicySnapshot{}, false
	}
	if route, ok := s.modelRouteByModel[model]; ok {
		policy, ok := s.backendByName[route.Backend]
		return policy, ok && policy.Kind == string(execution.BackendKindLLM)
	}
	selector, err := execution.ParseExecutionSelector(model)
	if err != nil {
		return ExecutionBackendPolicySnapshot{}, false
	}
	backend := execution.CanonicalTargetBackendName(selector.Backend)
	if backend == "" {
		backend = s.snapshot.DefaultLLMBackend
	}
	policy, ok := s.backendByName[backend]
	if !ok || policy.Kind != string(execution.BackendKindLLM) {
		// ProviderManager's compatibility behavior routes an unknown prefix to
		// the local provider. Preserve that routing without consulting live
		// ProviderManager state after the snapshot is frozen.
		policy, ok = s.backendByName[execution.OllamaBackendName]
	}
	return policy, ok && policy.Kind == string(execution.BackendKindLLM)
}

func (s *executionPolicyState) backendPolicy(target execution.ExecutionTarget) (ExecutionBackendPolicySnapshot, bool) {
	if s == nil {
		return ExecutionBackendPolicySnapshot{}, false
	}
	policy, ok := s.backendByName[execution.CanonicalTargetBackendName(target.Backend)]
	return policy, ok
}

func (s *executionPolicyState) environmentForBackend(backend string) []string {
	if s == nil {
		return nil
	}
	return slices.Clone(s.environmentByBackend[execution.CanonicalTargetBackendName(backend)])
}

func (s *executionPolicyState) codexExecutionWorld(backend string, agentDef *agent.AgentDef) (string, bool, error) {
	if s == nil {
		return "", false, fmt.Errorf("execution policy snapshot is unavailable")
	}
	world, ok := s.codexWorldByBackend[execution.CanonicalTargetBackendName(backend)]
	if !ok || strings.TrimSpace(world.projectRoot) == "" {
		return "", false, fmt.Errorf("execution policy snapshot has no Codex execution world for backend %q", backend)
	}
	if agentDef == nil {
		return world.projectRoot, world.coordinatorNetworkDisabled, nil
	}
	agentName := strings.TrimSpace(agentDef.Name)
	if agentName == "" {
		return "", false, fmt.Errorf("execution policy snapshot cannot resolve an unnamed Codex agent")
	}
	networkDisabled, ok := world.networkDisabledByAgent[agentName]
	if !ok {
		return "", false, fmt.Errorf("execution policy snapshot has no network policy for Codex agent %q on backend %q", agentName, backend)
	}
	return world.projectRoot, networkDisabled, nil
}

// ExecutionPolicySnapshot returns a defensive copy of the immutable policy
// admitted for this coordinator. It deliberately excludes raw child
// environment values.
func (c *Coordinator) ExecutionPolicySnapshot() *ExecutionPolicySnapshot {
	if c == nil || c.executionPolicy == nil {
		return nil
	}
	return cloneExecutionPolicySnapshot(c.executionPolicy.snapshot)
}

func (c *Coordinator) executionEnvironmentForBackend(backend string, fallbackAllowlist []string) []string {
	if c != nil && c.executionPolicy != nil {
		return c.executionPolicy.environmentForBackend(backend)
	}
	// Compatibility for deliberately minimal unit-test coordinators. Production
	// coordinators always have executionPolicy before a provider can start.
	return buildAllowlistedEnvironment(fallbackAllowlist)
}

// codexExecutionWorld returns only values frozen in executionPolicy for a
// production coordinator. It intentionally fails closed when a task presents
// an agent that was not part of the admitted Codex route. The live fallback is
// retained solely for minimal, hand-built unit-test coordinators that have no
// execution policy state.
func (c *Coordinator) codexExecutionWorld(backend string, agentDef *agent.AgentDef) (string, bool, error) {
	if c == nil {
		return "", false, fmt.Errorf("codex execution world requires a coordinator")
	}
	if c.executionPolicy != nil {
		return c.executionPolicy.codexExecutionWorld(backend, agentDef)
	}
	networkDisabled := c.noNet
	if agentDef != nil && agentDef.NoNet {
		networkDisabled = true
	}
	return c.projectDir, networkDisabled, nil
}

func (c *Coordinator) ensureExecutionPolicySnapshot() error {
	if c == nil || c.executionPolicy == nil {
		return nil
	}
	current, err := newExecutionPolicyState(c)
	if err != nil {
		return fmt.Errorf("resolve execution policy snapshot: %w", err)
	}
	if current.snapshot.ConfigurationHash != c.executionPolicy.snapshot.ConfigurationHash {
		return fmt.Errorf("execution policy changed after coordinator startup")
	}

	var persisted *ExecutionPolicySnapshot
	c.viewSessionData(func(sd *SessionData) {
		persisted = cloneExecutionPolicySnapshot(sd.ExecutionPolicySnapshot)
	})
	journalSnapshot, err := c.canonicalExecutionPolicySnapshot()
	if err != nil {
		return err
	}
	if journalSnapshot != nil {
		if persisted == nil || persisted.ConfigurationHash != journalSnapshot.ConfigurationHash {
			if err := c.mutateSessionData(func(sd *SessionData) error {
				sd.ExecutionPolicySnapshot = cloneExecutionPolicySnapshot(journalSnapshot)
				return nil
			}); err != nil {
				return fmt.Errorf("repair execution policy snapshot projection: %w", err)
			}
			if err := c.persistSession("persist event-authoritative execution policy snapshot"); err != nil {
				return err
			}
		}
		if journalSnapshot.ConfigurationHash != current.snapshot.ConfigurationHash {
			return fmt.Errorf("execution policy snapshot drift detected (event_store=%s current=%s)", journalSnapshot.ConfigurationHash, current.snapshot.ConfigurationHash)
		}
		return nil
	}
	if persisted != nil {
		if err := validateExecutionPolicySnapshot(persisted); err != nil {
			return err
		}
		return fmt.Errorf("execution policy snapshot is missing from the event journal")
	}

	if len(c.getInterruptedTasks()) > 0 {
		return fmt.Errorf("legacy session has interrupted tasks but no execution policy snapshot; start a new session before resuming")
	}
	if c.eventStore == nil {
		return fmt.Errorf("execution policy snapshot requires an available event journal")
	}
	payload, err := json.Marshal(c.executionPolicy.snapshot)
	if err != nil {
		return fmt.Errorf("marshal execution policy snapshot: %w", err)
	}
	key := "execution-policy-snapshot:" + c.executionPolicy.snapshot.ConfigurationHash
	if _, err := c.EventJournal().Append(context.Background(), RunEvent{
		Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", IdempotencyKey: key, Payload: payload,
	}); err != nil {
		return fmt.Errorf("append execution policy snapshot: %w", err)
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.ExecutionPolicySnapshot = cloneExecutionPolicySnapshot(c.executionPolicy.snapshot)
		return nil
	}); err != nil {
		return fmt.Errorf("update execution policy snapshot projection: %w", err)
	}
	if err := c.persistSession("persist execution policy snapshot"); err != nil {
		return err
	}
	return nil
}

// canonicalExecutionPolicySnapshot reads the active branch's event-first
// policy record. session.json is a projection only and must never supply an
// execution policy that the event journal cannot corroborate.
func (c *Coordinator) canonicalExecutionPolicySnapshot() (*ExecutionPolicySnapshot, error) {
	if c == nil || c.eventStore == nil {
		return nil, nil
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return nil, fmt.Errorf("read execution policy event journal: %w", err)
	}
	var tree *SessionTree
	if c.session != nil {
		tree, err = LoadSessionTree(c.session.Workspace)
		if err != nil {
			return nil, fmt.Errorf("load execution policy session tree: %w", err)
		}
	}
	branch := "main"
	if tree != nil && tree.ActiveBranch != "" {
		branch = tree.ActiveBranch
	}
	return executionPolicySnapshotFromEvents(FilterEventsForBranch(events, tree, branch))
}
