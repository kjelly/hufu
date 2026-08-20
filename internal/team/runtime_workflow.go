package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
)

// runtimeWorkflow is the generic, runtime-owned P0 workflow state machine.
// It intentionally knows nothing about a particular project, provider, or
// agent-team template. Coordinator prose may request work, but it cannot move
// this state machine or select a worker from another phase.
type runtimeWorkflow struct {
	mu                   sync.RWMutex
	enabled              bool
	team                 string
	phases               []Phase
	state                Phase
	policies             Policies
	capabilities         Capabilities
	workspace            RuntimeWorkspace
	phaseAgents          map[Phase]map[string]bool
	phaseContracts       map[Phase]map[string]bool
	results              map[Phase]PhaseResult
	retryState           *RetryState
	retryPolicy          agent.RetryConfig
	registry             *ProviderRegistry
	verificationRequired bool
	repositoryRoot       string
	emitEvent            func(eventType string, phase Phase, details LifecycleEventPayload)
}

func newRuntimeWorkflow(session *TeamSession) (*runtimeWorkflow, error) {
	w := &runtimeWorkflow{state: PhaseInit, results: make(map[Phase]PhaseResult), retryState: NewRetryState()}
	if session == nil || len(session.Config.Workflow.Phases) == 0 {
		return w, nil
	}
	phases, err := normalizeWorkflowPhases(session.Config.Workflow.Phases)
	if err != nil {
		return nil, err
	}
	if err := validateWorkflowPhasePolicy(phases, session.Config.Policies.AllowPhaseSkip); err != nil {
		return nil, err
	}
	root := filepath.Join(session.Workspace, "runtime")
	if err := ensureRuntimeWorkspace(root); err != nil {
		return nil, err
	}
	w.enabled = true
	w.team = session.Config.Name
	w.repositoryRoot = session.Dir
	if strings.TrimSpace(w.repositoryRoot) == "" {
		w.repositoryRoot = session.Workspace
	}
	if strings.TrimSpace(w.repositoryRoot) == "" {
		w.repositoryRoot = "."
	}
	w.phases = phases
	w.workspace = RuntimeWorkspace{Root: root}
	w.policies = Policies{
		RequirePhaseSuccess: session.Config.Policies.RequirePhaseSuccess,
		AllowPhaseSkip:      session.Config.Policies.AllowPhaseSkip,
		MaxRetries:          session.Config.Policies.MaxRetries,
		FailFast:            session.Config.Policies.FailFast,
	}
	w.capabilities = Capabilities{Required: append([]string(nil), session.Config.Capabilities.Required...)}

	ec := ExecutionContext{
		Team: w.team, RepositoryRoot: w.repositoryRoot, CurrentPhase: PhaseInit,
		Workflow:         Workflow{Phases: w.phases},
		Capabilities:     w.capabilities,
		RuntimeWorkspace: w.workspace,
		Policies:         w.policies,
	}
	if err := ec.Validate(); err != nil {
		return nil, fmt.Errorf("invalid runtime context configuration: %w", err)
	}
	w.capabilities = Capabilities{Required: append([]string(nil), session.Config.Capabilities.Required...)}
	w.registry = session.ProviderRegistry
	w.retryPolicy = session.Config.Retry
	w.verificationRequired = session.Config.Verification.Required
	w.phaseAgents = make(map[Phase]map[string]bool, len(phases))
	w.phaseContracts = make(map[Phase]map[string]bool, len(phases))
	for _, task := range session.ContractTasks {
		if task.Phase == "" {
			continue
		}
		if w.phaseAgents[task.Phase] == nil {
			w.phaseAgents[task.Phase] = make(map[string]bool)
		}
		w.phaseAgents[task.Phase][strings.ToLower(strings.TrimSpace(task.Agent))] = true
		if w.phaseContracts[task.Phase] == nil {
			w.phaseContracts[task.Phase] = make(map[string]bool)
		}
		w.phaseContracts[task.Phase][runtimeContractID(task)] = true
	}
	return w, nil
}

// executeAction invokes a provider selected by a static action contract. The
// runtime owns this boundary: a coordinator cannot supply an action through
// its tool schema, and actions are permitted only during EXECUTE.
func (w *runtimeWorkflow) executeActionValue(ctx context.Context, action Action) (interface{}, error) {
	if !w.Enabled() {
		return "", fmt.Errorf("structured actions require an enabled runtime workflow")
	}
	w.mu.RLock()
	phase := w.state
	registry := w.registry
	w.mu.RUnlock()
	if phase != PhaseExecute {
		return "", fmt.Errorf("structured action %q is only allowed during execute phase", action.Type)
	}
	capability := normalizeCapability(action.Capability)
	if capability == "" {
		return "", fmt.Errorf("structured action %q is missing capability", action.Type)
	}
	if strings.TrimSpace(action.Type) == "" {
		return "", fmt.Errorf("structured action for capability %q is missing type", capability)
	}
	provider, ok := registry.Get(capability)
	if !ok {
		return "", ActionProviderError{Capability: capability, Cause: fmt.Errorf("provider is not registered")}
	}
	if err := provider.Validate(action); err != nil {
		return "", ActionValidationError{Capability: capability, Cause: err}
	}
	result, err := provider.Execute(ctx, action)
	if err != nil {
		return "", ActionProviderError{Capability: capability, Cause: err}
	}
	return result, nil
}

func (w *runtimeWorkflow) executeAction(ctx context.Context, action Action) (string, error) {
	result, err := w.executeActionValue(ctx, action)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "structured action completed", nil
	}
	if text, ok := result.(string); ok {
		return text, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", ActionProviderError{Capability: normalizeCapability(action.Capability), Cause: fmt.Errorf("encode provider result: %w", err)}
	}
	return string(encoded), nil
}

func (w *runtimeWorkflow) providerName(capability string) string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	registry := w.registry
	w.mu.RUnlock()
	if registry == nil {
		return ""
	}
	return registry.ProviderName(capability)
}

// permitActionRetry records a retryable provider failure under its stable
// signature and applies the configured per-signature limit. It deliberately
// handles only ActionProvider execution: generic worker retries retain their
// established DAG and recovery policies.
func (w *runtimeWorkflow) permitActionRetry(task TaskDef, err error) bool {
	if w == nil || !w.Enabled() || task.Action == nil || err == nil {
		return false
	}
	if w.policies.FailFast {
		return false
	}
	var validation ActionValidationError
	if errors.As(err, &validation) {
		return false
	}
	var providerErr ActionProviderError
	if !errors.As(err, &providerErr) && !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	limit := w.retryPolicy.Transient.MaxAttempts
	if limit <= 0 {
		limit = w.policies.MaxRetries
	}
	if limit <= 0 {
		limit = task.MaxRetries
	}
	if limit <= 0 {
		return false
	}
	category := CategoryProviderFailure
	if errors.Is(err, context.DeadlineExceeded) {
		category = CategoryTimeout
	}
	source := normalizeCapability(task.Action.Capability)
	if source == "" {
		source = task.Agent
	}
	failure := ExecutionError{
		Phase: w.state, Component: task.Action.Type, Source: source,
		Category: category, Message: err.Error(), Cause: err.Error(), Retryable: true,
	}
	return w.retryState.RecordFailure(failure, limit)
}

func (w *runtimeWorkflow) actionExecutionError(task TaskDef, err error) ExecutionError {
	phase := PhaseInit
	if w != nil {
		w.mu.RLock()
		phase = w.state
		w.mu.RUnlock()
	}
	category := CategoryProviderFailure
	retryable := true
	var validation ActionValidationError
	if errors.As(err, &validation) {
		category = CategoryValidationFailed
		retryable = false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		category = CategoryTimeout
	}
	source := ""
	component := task.Agent
	if task.Action != nil {
		source = normalizeCapability(task.Action.Capability)
		component = task.Action.Type
	}
	if source == "" {
		source = task.Agent
	}
	return ExecutionError{Phase: phase, Component: component, Source: source, Category: category,
		Message: err.Error(), Cause: err.Error(), Retryable: retryable}
}

// repairRetryLimit returns the configured repair limit, falling back to the
// established per-task DAG limit for teams that have not opted into retry
// policy configuration.
func (w *runtimeWorkflow) repairRetryLimit(taskMaxRetries int) int {
	if w == nil || !w.Enabled() {
		return taskMaxRetries
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.retryPolicy.Repair.MaxAttemptsPerFailureSignature > 0 {
		return w.retryPolicy.Repair.MaxAttemptsPerFailureSignature
	}
	if w.policies.MaxRetries > 0 {
		return w.policies.MaxRetries
	}
	return taskMaxRetries
}

// permitRepairRetry limits DAG repair loops by the durable generic failure
// signature. Validation failures remain permanent; an on-failure loop may
// repair a provider, tool, or environment failure only within this bound.
func (w *runtimeWorkflow) permitRepairRetry(task TaskDef, err error) bool {
	if !w.Enabled() || err == nil {
		return false
	}
	if w.policies.FailFast {
		return false
	}
	var validation ActionValidationError
	if errors.As(err, &validation) {
		return false
	}
	limit := w.repairRetryLimit(task.MaxRetries)
	if limit <= 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	category := CategoryToolFailure
	if errors.Is(err, context.DeadlineExceeded) {
		category = CategoryTimeout
	} else {
		var providerErr ActionProviderError
		if errors.As(err, &providerErr) {
			category = CategoryProviderFailure
		}
	}
	component := task.ID
	if component == "" {
		component = task.Agent
	}
	failure := ExecutionError{Phase: w.state, Component: component, Source: task.Agent,
		Category: category, Message: err.Error(), Cause: err.Error(), Retryable: true}
	return w.retryState.RecordFailure(failure, limit)
}

func normalizeWorkflowPhases(raw []string) ([]Phase, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	canonical := []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify}
	index := make(map[Phase]int, len(canonical))
	for i, p := range canonical {
		index[p] = i
	}
	result := make([]Phase, 0, len(raw))
	last := -1
	seen := make(map[Phase]bool)
	for _, value := range raw {
		p := Phase(strings.ToUpper(strings.TrimSpace(value)))
		i, ok := index[p]
		if !ok {
			return nil, fmt.Errorf("workflow phase %q is unsupported; use prepare, audit, execute, or verify", value)
		}
		if seen[p] || i <= last {
			return nil, fmt.Errorf("workflow phases must be unique and ordered prepare → audit → execute → verify")
		}
		seen[p] = true
		last = i
		result = append(result, p)
	}
	if result[0] != PhasePrepare || result[len(result)-1] != PhaseVerify {
		return nil, fmt.Errorf("workflow must start with prepare and end with verify")
	}
	return result, nil
}

func validateWorkflowPhasePolicy(phases []Phase, allowSkip bool) error {
	if allowSkip {
		return nil
	}
	required := []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify}
	if len(phases) != len(required) {
		return fmt.Errorf("workflow phase skipping is disabled; configure prepare, audit, execute, and verify")
	}
	for i, phase := range required {
		if phases[i] != phase {
			return fmt.Errorf("workflow phase skipping is disabled; configure prepare, audit, execute, and verify in order")
		}
	}
	return nil
}

func ensureRuntimeWorkspace(root string) error {
	if root == "" {
		return fmt.Errorf("runtime workspace root is empty")
	}
	for _, name := range []string{"", "artifacts", "receipts", "logs", "phases"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			return fmt.Errorf("create runtime workspace: %w", err)
		}
	}
	return nil
}

func (w *runtimeWorkflow) Enabled() bool { return w != nil && w.enabled }

func (w *runtimeWorkflow) State() Phase {
	if w == nil {
		return PhaseInit
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

func (w *runtimeWorkflow) setEventEmitter(fn func(string, Phase, LifecycleEventPayload)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emitEvent = fn
}

func (w *runtimeWorkflow) emit(eventType string, phase Phase, details LifecycleEventPayload) {
	if w.emitEvent != nil {
		w.emitEvent(eventType, phase, details)
	}
}

func (w *runtimeWorkflow) Start() error {
	if !w.Enabled() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == PhaseInit {
		w.state = w.phases[0]
		w.emit("phase_started", w.state, LifecycleEventPayload{
			Agent: "", Provider: "", FailureSignature: "", Artifacts: []ArtifactRef{},
		})
	}
	if w.state == PhaseFailed {
		return fmt.Errorf("workflow is failed; start a new session before dispatching further work")
	}
	return nil
}

func (w *runtimeWorkflow) activeWorkerNames() []string {
	if !w.Enabled() {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	set := w.phaseAgents[w.state]
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (w *runtimeWorkflow) validateTasks(tasks []TaskDef) error {
	if !w.Enabled() {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.state == PhaseInit {
		return fmt.Errorf("workflow has not started")
	}
	if w.state == PhaseDone || w.state == PhaseFailed {
		return fmt.Errorf("workflow is %s and cannot accept new tasks", strings.ToLower(string(w.state)))
	}
	allowed := w.phaseAgents[w.state]
	expectedContracts := w.phaseContracts[w.state]
	providedContracts := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task.Phase != w.state {
			return fmt.Errorf("workflow phase %s only accepts %s tasks; task for agent %q is bound to %s", w.state, w.state, task.Agent, task.Phase)
		}
		if !allowed[strings.ToLower(strings.TrimSpace(task.Agent))] {
			return fmt.Errorf("agent %q is not authorized for workflow phase %s", task.Agent, w.state)
		}
		if !expectedContracts[task.ContractID] {
			return fmt.Errorf("task for agent %q is not bound to a static contract for workflow phase %s", task.Agent, w.state)
		}
		if providedContracts[task.ContractID] {
			return fmt.Errorf("workflow contract %q was dispatched more than once in phase %s", task.ContractID, w.state)
		}
		providedContracts[task.ContractID] = true
	}
	for contractID := range expectedContracts {
		if !providedContracts[contractID] {
			return fmt.Errorf("workflow phase %s must dispatch static contract %q", w.state, contractID)
		}
	}
	return nil
}

func (w *runtimeWorkflow) observe(items []*TodoItem) error {
	if !w.Enabled() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == PhaseDone {
		return nil
	}
	if w.state == PhaseFailed {
		return fmt.Errorf("workflow failed: %s", w.results[w.state].Summary)
	}
	expectedContracts := w.phaseContracts[w.state]
	if len(expectedContracts) == 0 {
		return w.failLocked("workflow", "workflow", "CONFIGURATION", fmt.Sprintf("no static task contract is configured for phase %s", w.state), false, PhaseStatusFailure)
	}
	completed := make(map[string]bool, len(expectedContracts))
	var evidence []ArtifactRef
	for _, item := range items {
		if item.Phase != w.state || !expectedContracts[item.ContractID] {
			continue
		}
		switch item.Status {
		case TaskDone:
			if w.state == PhaseVerify && w.verificationRequired && !isVerifySuccess(item.VerifyResult) {
				return w.failLocked(item.Agent, item.Agent, CategoryValidationFailed, "verify phase task completed without successful objective verification evidence", false, PhaseStatusFailure)
			}
			completed[item.ContractID] = true
			if item.TypedResult != nil {
				evidence = append(evidence, item.TypedResult.Artifacts...)
			}
		case TaskError, TaskProtocolIncomplete:
			if item.RuntimeError != nil {
				return w.failExecutionErrorLocked(*item.RuntimeError, PhaseStatusFailure)
			}
			return w.failLocked(item.Agent, item.Agent, "TASK_FAILURE", item.Detail, false, PhaseStatusFailure)
		case TaskBlocked:
			return w.failLocked(item.Agent, item.Agent, "TASK_BLOCKED", item.Detail, false, PhaseStatusBlocked)
		}
	}
	for contractID := range expectedContracts {
		if !completed[contractID] {
			return nil
		}
	}
	w.results[w.state] = PhaseResult{Status: PhaseStatusSuccess, Summary: fmt.Sprintf("%s phase completed", strings.ToLower(string(w.state))), Evidence: evidence}
	w.emit("phase_succeeded", w.state, LifecycleEventPayload{
		Agent: "", Provider: "", FailureSignature: "", Artifacts: evidence,
	})
	next := nextWorkflowPhase(w.phases, w.state)
	if next == "" {
		return w.failLocked("workflow", "workflow", "CONFIGURATION", "verify must be the final configured workflow phase", false, PhaseStatusFailure)
	}
	if !w.policies.AllowPhaseSkip && !IsValidTransition(w.state, next) {
		return w.failLocked("workflow", "workflow", "STATE_TRANSITION", fmt.Sprintf("invalid transition %s → %s", w.state, next), false, PhaseStatusFailure)
	}
	w.state = next
	w.emit("phase_started", w.state, LifecycleEventPayload{
		Agent: "", Provider: "", FailureSignature: "", Artifacts: []ArtifactRef{},
	})
	return nil
}

func (w *runtimeWorkflow) failExecutionErrorLocked(executionErr ExecutionError, status PhaseStatus) error {
	if status == "" {
		status = PhaseStatusFailure
	}
	if executionErr.Phase == "" {
		executionErr.Phase = w.state
	}
	if executionErr.Message == "" {
		executionErr.Message = executionErr.Cause
	}
	w.results[w.state] = PhaseResult{Status: status, Summary: executionErr.Message, Errors: []ExecutionError{executionErr}}
	if IsValidTransition(w.state, PhaseFailed) {
		w.state = PhaseFailed
	}
	w.emit("phase_failed", executionErr.Phase, LifecycleEventPayload{
		Agent:            executionErr.Component,
		Provider:         executionErr.Source,
		FailureSignature: executionErr.Signature().String(),
		Artifacts:        []ArtifactRef{},
	})
	return fmt.Errorf("workflow %s failed: %s", strings.ToLower(string(executionErr.Phase)), executionErr.Message)
}

func nextWorkflowPhase(phases []Phase, current Phase) Phase {
	for i, phase := range phases {
		if phase != current {
			continue
		}
		if i+1 < len(phases) {
			return phases[i+1]
		}
		if current == PhaseVerify {
			return PhaseDone
		}
	}
	return ""
}

func (w *runtimeWorkflow) failLocked(component, source, category, message string, retryable bool, status PhaseStatus) error {
	if status == "" {
		status = PhaseStatusFailure
	}
	phase := w.state
	w.results[phase] = PhaseResult{Status: status, Summary: message, Errors: []ExecutionError{{Phase: phase, Component: component, Source: source, Category: category, Cause: message, Message: message, Retryable: retryable}}}
	if IsValidTransition(w.state, PhaseFailed) {
		w.state = PhaseFailed
	}
	errObj := ExecutionError{Phase: phase, Component: component, Source: source, Category: category, Cause: message, Message: message, Retryable: retryable}
	w.emit("phase_failed", phase, LifecycleEventPayload{
		Agent:            component,
		Provider:         source,
		FailureSignature: errObj.Signature().String(),
		Artifacts:        []ArtifactRef{},
	})
	return fmt.Errorf("workflow %s failed: %s", strings.ToLower(string(phase)), message)
}

func (w *runtimeWorkflow) fail(component, source, category, message string, retryable bool) error {
	if !w.Enabled() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == PhaseDone || w.state == PhaseFailed {
		return nil
	}
	return w.failLocked(component, source, category, message, retryable, PhaseStatusFailure)
}

func (w *runtimeWorkflow) requireFinished() error {
	if !w.Enabled() {
		return nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.state != PhaseDone {
		return fmt.Errorf("workflow verification gate is not satisfied: current phase is %s", w.state)
	}
	if w.results[PhaseVerify].Status != PhaseStatusSuccess {
		return fmt.Errorf("workflow verification gate is not satisfied: verify phase did not succeed")
	}
	return nil
}

func (w *runtimeWorkflow) executionContext() ExecutionContext {
	if !w.Enabled() {
		return ExecutionContext{}
	}
	w.mu.RLock()
	phase := w.state
	root := w.repositoryRoot
	w.mu.RUnlock()
	return w.executionContextAt(phase, root)
}

func (w *runtimeWorkflow) executionContextAt(phase Phase, repositoryRoot string) ExecutionContext {
	if !w.Enabled() {
		return ExecutionContext{}
	}
	return ExecutionContext{
		Team: w.team, CurrentPhase: phase, RepositoryRoot: repositoryRoot,
		Workflow:     Workflow{Phases: append([]Phase(nil), w.phases...)},
		Capabilities: w.capabilities, RuntimeWorkspace: w.workspace,
		ArtifactPaths: map[string]string{
			"root":      filepath.ToSlash(w.workspace.Root),
			"artifacts": filepath.ToSlash(filepath.Join(w.workspace.Root, "artifacts")),
			"logs":      filepath.ToSlash(filepath.Join(w.workspace.Root, "logs")),
			"phases":    filepath.ToSlash(filepath.Join(w.workspace.Root, "phases")),
			"receipts":  filepath.ToSlash(filepath.Join(w.workspace.Root, "receipts")),
		},
		Policies: w.policies,
	}
}

func (w *runtimeWorkflow) setRepositoryRoot(root string) {
	if w == nil || strings.TrimSpace(root) == "" {
		return
	}
	w.mu.Lock()
	w.repositoryRoot = root
	w.mu.Unlock()
}

// runtimeAllowedPaths adds the runtime-owned workspace to the per-worker path
// allowance without mutating agent configuration. Source/project paths remain
// governed by the existing team and agent policy; execution receipts and
// generated artifacts always have a controlled writable destination.
func (c *Coordinator) runtimeAllowedPaths(paths []string) []string {
	result := append([]string(nil), paths...)
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return result
	}
	root := c.phaseWorkflow.executionContext().RuntimeWorkspace.Root
	for _, path := range result {
		if filepath.Clean(path) == filepath.Clean(root) {
			return result
		}
	}
	return append(result, root)
}

// runtimeAllowedWritePaths explicitly isolates write operations to the runtime workspace
// when a workflow is active, rejecting modifications to the underlying source.
func (c *Coordinator) runtimeAllowedWritePaths() []string {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return nil
	}
	return []string{c.phaseWorkflow.executionContext().RuntimeWorkspace.Root}
}

func (w *runtimeWorkflow) snapshot() (Phase, map[Phase]PhaseResult, string, *RetryState) {
	if !w.Enabled() {
		return "", nil, "", nil
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	results := make(map[Phase]PhaseResult, len(w.results))
	for phase, result := range w.results {
		results[phase] = result
	}
	var retryStateCopy *RetryState
	if w.retryState != nil {
		retryStateCopy = NewRetryState()
		for k, v := range w.retryState.Attempts {
			retryStateCopy.Attempts[k] = v
		}
	}
	return w.state, results, w.workspace.Root, retryStateCopy
}

func (w *runtimeWorkflow) restore(state Phase, results map[Phase]PhaseResult, root string, retryState *RetryState) error {
	if !w.Enabled() || state == "" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if root != "" && filepath.Clean(root) != filepath.Clean(w.workspace.Root) {
		return fmt.Errorf("runtime workspace checkpoint does not match configured workspace")
	}
	valid := state == PhaseInit || state == PhaseDone || state == PhaseFailed
	for _, phase := range w.phases {
		valid = valid || phase == state
	}
	if !valid {
		return fmt.Errorf("invalid workflow state in checkpoint: %s", state)
	}
	if state == PhaseDone && results[PhaseVerify].Status != PhaseStatusSuccess {
		return fmt.Errorf("invalid workflow transition: DONE requires VERIFY SUCCESS")
	}
	if state != PhaseInit && state != PhaseFailed && state != PhaseDone {
		for _, phase := range w.phases {
			if phase == state {
				break
			}
			if results[phase].Status != PhaseStatusSuccess {
				return fmt.Errorf("invalid workflow transition: phase %s reached but preceding phase %s did not succeed", state, phase)
			}
		}
	}
	w.state = state
	w.results = make(map[Phase]PhaseResult, len(results))
	for phase, result := range results {
		w.results[phase] = result
	}
	if retryState != nil {
		w.retryState = NewRetryState()
		for k, v := range retryState.Attempts {
			w.retryState.Attempts[k] = v
		}
	}
	return nil
}

func validateRuntimeWorkflowTeam(session *TeamSession, registry *ProviderRegistry) error {
	if session == nil || len(session.Config.Workflow.Phases) == 0 {
		return nil
	}
	phases, err := normalizeWorkflowPhases(session.Config.Workflow.Phases)
	if err != nil {
		return err
	}
	if err := validateWorkflowPhasePolicy(phases, session.Config.Policies.AllowPhaseSkip); err != nil {
		return err
	}
	if err := validateWorkflowCapabilities(session.Config.Capabilities.Required, registry); err != nil {
		return err
	}

	if !session.Config.Delegation.BindTaskGoalContracts {
		return fmt.Errorf("workflow requires delegation.bind-task-goal-contracts: true so phases are runtime-owned")
	}
	if session.Config.Delegation.RequireExactInitialBatch || session.Config.Delegation.BindInitialTaskContracts {
		return fmt.Errorf("workflow cannot combine delegation.initial-batch with runtime-owned phase dispatch")
	}
	configured := make(map[Phase]bool, len(phases))
	for _, phase := range phases {
		configured[phase] = true
	}
	seen := make(map[Phase]bool, len(phases))
	verifyHasObjectiveCheck := false
	contractIDs := make(map[string]bool, len(session.ContractTasks))
	for i := range session.ContractTasks {
		task := &session.ContractTasks[i]
		phase := Phase(strings.ToUpper(strings.TrimSpace(string(task.Phase))))
		if phase == "" {
			return fmt.Errorf("workflow task %q must declare phase", task.ID)
		}
		if !configured[phase] {
			return fmt.Errorf("workflow task %q declares phase %q outside configured workflow", task.ID, task.Phase)
		}
		if strings.TrimSpace(task.WhenGoalContains) == "" {
			return fmt.Errorf("workflow task %q must declare when-goal-contains", task.ID)
		}
		if strings.TrimSpace(task.Agent) == "" {
			return fmt.Errorf("workflow task %q must declare agent", task.ID)
		}
		if session.Agents[strings.ToLower(strings.TrimSpace(task.Agent))] == nil {
			return fmt.Errorf("workflow task %q references unknown agent %q", task.ID, task.Agent)
		}
		if task.Action != nil {
			if phase != PhaseExecute {
				return fmt.Errorf("workflow task %q declares an action outside execute phase", task.ID)
			}
			capability := normalizeCapability(task.Action.Capability)
			if capability == "" || strings.TrimSpace(task.Action.Type) == "" {
				return fmt.Errorf("workflow task %q action requires capability and type", task.ID)
			}
			if !containsCapability(session.Config.Capabilities.Required, capability) {
				return fmt.Errorf("workflow task %q action capability %q is not required by the team", task.ID, capability)
			}
		}
		if phase == PhaseVerify && (strings.TrimSpace(task.Verify) != "" || task.VerifySpec != nil) {
			verifyHasObjectiveCheck = true
		}
		task.Phase = phase
		contractID := runtimeContractID(*task)
		if contractIDs[contractID] {
			return fmt.Errorf("workflow task contract ID %q is duplicated", contractID)
		}
		contractIDs[contractID] = true
		seen[phase] = true
	}
	for _, phase := range phases {
		if !seen[phase] {
			return fmt.Errorf("workflow phase %s has no static task contract", strings.ToLower(string(phase)))
		}
	}
	if session.Config.Verification.Required && !verifyHasObjectiveCheck {
		return fmt.Errorf("workflow verification is required but no verify-phase static contract declares verify or verify-spec")
	}
	return nil
}

func containsCapability(capabilities []string, capability string) bool {
	for _, configured := range capabilities {
		if normalizeCapability(configured) == capability {
			return true
		}
	}
	return false
}

func runtimeContractID(task TaskDef) string {
	if id := strings.TrimSpace(task.ID); id != "" {
		return id
	}
	return strings.ToLower(strings.TrimSpace(task.Agent)) + ":" + strings.ToLower(strings.TrimSpace(task.WhenGoalContains))
}

func validateWorkflowCapabilities(required []string, registry *ProviderRegistry) error {
	for _, cap := range required {
		if registry == nil || !registry.Has(cap) {
			return fmt.Errorf("workflow requires unsupported capability %q", cap)
		}
	}
	return nil
}
