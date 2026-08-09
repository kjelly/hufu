package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// StructuredExecutionState is the durable state of a structured execution
// attempt. The states deliberately model validation separately from execution
// so a failed validator can be repaired without replaying a mutation.
type StructuredExecutionState string

const (
	StructuredExecutionDraft            StructuredExecutionState = "draft"
	StructuredExecutionValidating       StructuredExecutionState = "validating"
	StructuredExecutionRepairableFailed StructuredExecutionState = "repairable_failed"
	StructuredExecutionValidated        StructuredExecutionState = "validated"
	StructuredExecutionFrozen           StructuredExecutionState = "frozen"
	StructuredExecutionExecuting        StructuredExecutionState = "executing"
	StructuredExecutionVerified         StructuredExecutionState = "verified"
	StructuredExecutionFailed           StructuredExecutionState = "failed"
)

// ExecutionStepReceipt is Hufu-owned evidence for one actual structured step
// invocation. A producer cannot invent it: the runtime derives the ID and
// input digest before invoking the step runner.
type ExecutionStepReceipt struct {
	ID               string              `json:"id"`
	TaskID           string              `json:"task_id"`
	Attempt          int                 `json:"attempt"`
	RepairAttempt    int                 `json:"repair_attempt,omitempty"`
	StepID           string              `json:"step_id"`
	Tool             string              `json:"tool"`
	InputSHA256      string              `json:"input_sha256"`
	StartedAt        time.Time           `json:"started_at"`
	FinishedAt       time.Time           `json:"finished_at"`
	ExitCode         int                 `json:"exit_code"`
	Stdout           string              `json:"stdout,omitempty"`
	Stderr           string              `json:"stderr,omitempty"`
	StdoutRef        ArtifactRef         `json:"stdout_ref,omitempty"`
	StderrRef        ArtifactRef         `json:"stderr_ref,omitempty"`
	Consumed         []ArtifactRef       `json:"consumed,omitempty"`
	Produced         []ArtifactRef       `json:"produced,omitempty"`
	ConsumedDigests  map[string]string   `json:"consumed_digests,omitempty"`
	ProducedDigests  map[string]string   `json:"produced_digests,omitempty"`
	ProducedFacts    []StructuredFactRef `json:"produced_facts,omitempty"`
	PolicyVerdict    string              `json:"policy_verdict"`
	ValidatorVerdict string              `json:"validator_verdict,omitempty"`
	FailureClass     string              `json:"failure_class,omitempty"`
}

// ExecutionStepResult is returned by a provider-neutral step runner. Artifact
// keys must match declared output names. Artifact payloads are immutable
// references, so the engine can freeze their digests before mutation.
type ExecutionStepResult struct {
	ExitCode      int                    `json:"exit_code"`
	Stdout        string                 `json:"stdout,omitempty"`
	Stderr        string                 `json:"stderr,omitempty"`
	PolicyVerdict string                 `json:"policy_verdict,omitempty"`
	FailureClass  string                 `json:"failure_class,omitempty"`
	Artifacts     map[string]ArtifactRef `json:"artifacts,omitempty"`
	Facts         map[string]any         `json:"facts,omitempty"`
}

// StructuredStepRequest is supplied to a runner for one actual step. Repair
// attempts expose only declared produce steps, never validate/mutate tools.
type StructuredStepRequest struct {
	TaskID        string                    `json:"task_id"`
	Attempt       int                       `json:"attempt"`
	RepairAttempt int                       `json:"repair_attempt,omitempty"`
	Model         string                    `json:"model,omitempty"`
	Step          ExecutionStep             `json:"step"`
	ResolvedInput map[string]any            `json:"resolved_input"`
	Artifacts     map[string]ArtifactRef    `json:"artifacts"`
	Frozen        map[string]ArtifactRef    `json:"frozen"`
	Facts         map[string]StructuredFact `json:"facts"`
}

// StructuredStepRunner executes a single declared step. Implementations may
// call a provider, local tool, or test double; the execution engine does not
// interpret provider-specific inputs or outputs beyond artifact identity.
type StructuredStepRunner interface {
	RunStructuredStep(context.Context, StructuredStepRequest) (ExecutionStepResult, error)
}

// StructuredArtifactInspector is implemented by runners whose artifact
// references point at mutable storage such as workspace files. The engine
// re-inspects each consumed artifact immediately before mutation, closing the
// validation-to-execution TOCTOU gap. Content-addressed immutable runners need
// not implement it.
type StructuredArtifactInspector interface {
	InspectStructuredArtifact(context.Context, ArtifactRef) (ArtifactRef, error)
}

// StructuredStepRunnerFunc adapts a function to StructuredStepRunner.
type StructuredStepRunnerFunc func(context.Context, StructuredStepRequest) (ExecutionStepResult, error)

// RunStructuredStep implements StructuredStepRunner.
func (f StructuredStepRunnerFunc) RunStructuredStep(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
	return f(ctx, request)
}

// StructuredExecutionRequest identifies an execution contract attempt.
type StructuredExecutionRequest struct {
	TaskID          string
	Attempt         int
	Contract        ExecutionContract
	Registry        *ExecutionStepReceiptRegistry
	UpstreamOutputs map[string]map[string]StructuredOutputValue
	SelectModel     func(ExecutionStep, int) string
}

// StructuredExecutionResult contains the final lifecycle state, frozen
// artifacts, and every Hufu-owned step receipt, including failed validators.
type StructuredExecutionResult struct {
	State           StructuredExecutionState
	StateHistory    []StructuredExecutionState
	Receipts        []ExecutionStepReceipt
	Artifacts       map[string]ArtifactRef
	FrozenArtifacts map[string]ArtifactRef
	Facts           map[string]StructuredFact
}

// StructuredExecutionError identifies the causal failed step instead of
// collapsing a validator failure into a later result-submission failure.
type StructuredExecutionError struct {
	StepID    string
	ReceiptID string
	Class     string
	ExitCode  int
	Cause     error
}

// Error implements error.
func (e *StructuredExecutionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("structured execution %s step %q (receipt %s, exit %d): %v", e.Class, e.StepID, e.ReceiptID, e.ExitCode, e.Cause)
	}
	return fmt.Sprintf("structured execution %s step %q (receipt %s, exit %d)", e.Class, e.StepID, e.ReceiptID, e.ExitCode)
}

// Unwrap returns the original runner error when one was supplied.
func (e *StructuredExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ExecutionStepReceiptRegistry keeps authoritative receipts independent of
// worker summaries. It is safe to share between concurrently running tasks.
type ExecutionStepReceiptRegistry struct {
	mu       sync.RWMutex
	receipts map[string]ExecutionStepReceipt
}

// NewExecutionStepReceiptRegistry creates an empty authoritative receipt store.
func NewExecutionStepReceiptRegistry() *ExecutionStepReceiptRegistry {
	return &ExecutionStepReceiptRegistry{receipts: make(map[string]ExecutionStepReceipt)}
}

// Record stores one runtime-created receipt. Duplicate IDs with different
// evidence are rejected, preventing a later attempt from replacing history.
func (r *ExecutionStepReceiptRegistry) Record(receipt ExecutionStepReceipt) error {
	if r == nil {
		return fmt.Errorf("execution receipt registry is nil")
	}
	if strings.TrimSpace(receipt.ID) == "" {
		return fmt.Errorf("execution receipt id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.receipts[receipt.ID]; ok {
		if !reflect.DeepEqual(old, receipt) {
			return fmt.Errorf("execution receipt %q conflicts with existing evidence", receipt.ID)
		}
		return nil
	}
	r.receipts[receipt.ID] = cloneExecutionStepReceipt(receipt)
	return nil
}

// Get returns a detached receipt by ID.
func (r *ExecutionStepReceiptRegistry) Get(id string) (ExecutionStepReceipt, bool) {
	if r == nil {
		return ExecutionStepReceipt{}, false
	}
	r.mu.RLock()
	receipt, ok := r.receipts[id]
	r.mu.RUnlock()
	return cloneExecutionStepReceipt(receipt), ok
}

// ValidateClaims ensures every result claim cites an actual receipt belonging
// to exactly the same task and attempt. A claim for an unexecuted or other
// attempt's step therefore cannot become authoritative task evidence.
func (r *ExecutionStepReceiptRegistry) ValidateClaims(taskID string, attempt int, ids []string) error {
	if r == nil {
		return fmt.Errorf("execution receipt registry is nil")
	}
	if len(ids) == 0 {
		return fmt.Errorf("at least one execution receipt claim is required")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("execution receipt claim is empty")
		}
		if seen[id] {
			return fmt.Errorf("execution receipt claim %q is duplicated", id)
		}
		seen[id] = true
		receipt, ok := r.Get(id)
		if !ok {
			return fmt.Errorf("execution receipt claim %q does not exist", id)
		}
		if receipt.TaskID != taskID || receipt.Attempt != attempt {
			return fmt.Errorf("execution receipt claim %q belongs to task %q attempt %d, not task %q attempt %d", id, receipt.TaskID, receipt.Attempt, taskID, attempt)
		}
	}
	return nil
}

// ValidateCompleteClaims requires an exact claim set for one successful
// structured attempt. This prevents a worker from citing only the final happy
// receipt while omitting an earlier failed validator or repair invocation.
func (r *ExecutionStepReceiptRegistry) ValidateCompleteClaims(taskID string, attempt int, ids []string) error {
	if err := r.ValidateClaims(taskID, attempt, ids); err != nil {
		return err
	}
	expected := r.ReceiptIDs(taskID, attempt)
	claimed := make(map[string]bool, len(ids))
	for _, id := range ids {
		claimed[strings.TrimSpace(id)] = true
	}
	var missing []string
	for _, id := range expected {
		if !claimed[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("successful structured result omits execution receipt(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// ReceiptIDs returns stable receipt IDs for exactly one task attempt.
func (r *ExecutionStepReceiptRegistry) ReceiptIDs(taskID string, attempt int) []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	receipts := make([]ExecutionStepReceipt, 0)
	for _, receipt := range r.receipts {
		if receipt.TaskID == taskID && receipt.Attempt == attempt {
			receipts = append(receipts, receipt)
		}
	}
	r.mu.RUnlock()
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].StartedAt.Equal(receipts[j].StartedAt) {
			return receipts[i].ID < receipts[j].ID
		}
		return receipts[i].StartedAt.Before(receipts[j].StartedAt)
	})
	ids := make([]string, len(receipts))
	for i, receipt := range receipts {
		ids[i] = receipt.ID
	}
	return ids
}

// FirstFailure returns the first causal non-zero receipt for one task attempt.
// Later reporting calls (including a rejected submit_result) cannot replace it.
func (r *ExecutionStepReceiptRegistry) FirstFailure(taskID string, attempt int) (ExecutionStepReceipt, bool) {
	if r == nil {
		return ExecutionStepReceipt{}, false
	}
	r.mu.RLock()
	var receipts []ExecutionStepReceipt
	for _, receipt := range r.receipts {
		if receipt.TaskID != taskID || (attempt > 0 && receipt.Attempt != attempt) {
			continue
		}
		receipts = append(receipts, receipt)
	}
	r.mu.RUnlock()
	sortExecutionReceipts(receipts)
	for i, receipt := range receipts {
		if receipt.ExitCode == 0 {
			continue
		}
		if receipt.ValidatorVerdict == "fail" {
			recovered := false
			for _, later := range receipts[i+1:] {
				if later.StepID == receipt.StepID && later.ExitCode == 0 && later.ValidatorVerdict == "pass" {
					recovered = true
					break
				}
			}
			if recovered {
				continue
			}
		}
		return cloneExecutionStepReceipt(receipt), true
	}
	return ExecutionStepReceipt{}, false
}

func cloneExecutionStepReceipt(receipt ExecutionStepReceipt) ExecutionStepReceipt {
	copyReceipt := receipt
	copyReceipt.Consumed = append([]ArtifactRef(nil), receipt.Consumed...)
	copyReceipt.Produced = append([]ArtifactRef(nil), receipt.Produced...)
	copyReceipt.ProducedFacts = append([]StructuredFactRef(nil), receipt.ProducedFacts...)
	copyReceipt.ConsumedDigests = cloneStringMap(receipt.ConsumedDigests)
	copyReceipt.ProducedDigests = cloneStringMap(receipt.ProducedDigests)
	return copyReceipt
}

// RunStructuredExecution executes a structured contract using a generic step
// runner. A repairable validator failure closes the failed attempt, invokes
// only its declared upstream producers as a bounded repair, and then validates
// the new artifact again. No mutating step can execute until the validated
// artifact digest has been frozen.
func RunStructuredExecution(ctx context.Context, request StructuredExecutionRequest, runner StructuredStepRunner) (*StructuredExecutionResult, error) {
	if runner == nil {
		return nil, fmt.Errorf("structured execution runner is required")
	}
	if strings.TrimSpace(request.TaskID) == "" {
		return nil, fmt.Errorf("structured execution task id is required")
	}
	if request.Attempt <= 0 {
		return nil, fmt.Errorf("structured execution attempt must be positive")
	}
	if findings := validateStructuredExecutionSteps(request.Contract.Steps); hasStructuredExecutionErrors(findings) {
		return nil, fmt.Errorf("invalid structured execution contract: %s", formatStructuredExecutionFindings(findings))
	}
	ordered, err := orderedStructuredExecutionSteps(request.Contract.Steps)
	if err != nil {
		return nil, err
	}
	result := &StructuredExecutionResult{
		State:           StructuredExecutionDraft,
		StateHistory:    []StructuredExecutionState{StructuredExecutionDraft},
		Artifacts:       make(map[string]ArtifactRef),
		FrozenArtifacts: make(map[string]ArtifactRef),
		Facts:           make(map[string]StructuredFact),
	}
	execution := structuredExecutionRun{
		ctx: ctx, request: request, runner: runner, result: result,
		stepSucceeded: make(map[string]bool, len(ordered)),
		stepReceipts:  make(map[string]ExecutionStepReceipt, len(ordered)),
	}
	for _, step := range ordered {
		receipt, stepErr := execution.runStep(step, 0)
		if stepErr == nil {
			continue
		}
		if step.Effect != ExecutionEffectValidate || normalizedFailurePolicy(step.OnFailure) != StepFailureRepairable {
			execution.setState(StructuredExecutionFailed)
			failureClass := normalizedExecutionFailureClass(receipt.FailureClass)
			return result, structuredExecutionFailure(step, receipt, failureClass, stepErr)
		}
		if repairErr := execution.repairValidator(ordered, step); repairErr != nil {
			return result, repairErr
		}
	}
	return result, nil
}

type structuredExecutionRun struct {
	ctx           context.Context
	request       StructuredExecutionRequest
	runner        StructuredStepRunner
	result        *StructuredExecutionResult
	stepSucceeded map[string]bool
	stepReceipts  map[string]ExecutionStepReceipt
	receiptCount  int
}

func (e *structuredExecutionRun) setState(state StructuredExecutionState) {
	if e.result.State == state {
		return
	}
	e.result.State = state
	e.result.StateHistory = append(e.result.StateHistory, state)
}

func (e *structuredExecutionRun) runStep(step ExecutionStep, repairAttempt int) (ExecutionStepReceipt, error) {
	for _, dependency := range step.DependsOn {
		if !e.stepSucceeded[dependency] {
			err := fmt.Errorf("step %q cannot run before dependency %q succeeds", step.ID, dependency)
			return e.recordPreflightFailure(step, repairAttempt, err), err
		}
	}
	if err := e.validateMutationInputs(step); err != nil {
		return e.recordPreflightFailure(step, repairAttempt, err), err
	}
	resolvedInput, err := e.resolveStepInput(step)
	if err != nil {
		return e.recordPreflightFailure(step, repairAttempt, err), err
	}
	switch step.Effect {
	case ExecutionEffectValidate:
		e.setState(StructuredExecutionValidating)
	case ExecutionEffectMutate:
		e.setState(StructuredExecutionExecuting)
	}
	e.receiptCount++
	startedAt := time.Now().UTC()
	stepResult, runErr := e.runner.RunStructuredStep(e.ctx, StructuredStepRequest{
		TaskID: e.request.TaskID, Attempt: e.request.Attempt, RepairAttempt: repairAttempt,
		Model: e.selectedModel(step, repairAttempt), Step: step, ResolvedInput: resolvedInput,
		Artifacts: cloneArtifactMap(e.result.Artifacts), Frozen: cloneArtifactMap(e.result.FrozenArtifacts), Facts: cloneStructuredFacts(e.result.Facts),
	})
	if runErr != nil && stepResult.ExitCode == 0 {
		stepResult.ExitCode = 1
	}
	if runErr != nil && strings.TrimSpace(stepResult.Stderr) == "" {
		stepResult.Stderr = runErr.Error()
	}
	receipt := e.newReceipt(step, repairAttempt, startedAt, stepResult, runErr, resolvedInput)
	if runErr == nil && stepResult.ExitCode == 0 {
		runErr = e.recordDeclaredOutputs(step, stepResult, &receipt)
	}
	e.result.Receipts = append(e.result.Receipts, receipt)
	if e.request.Registry != nil {
		if registryErr := e.request.Registry.Record(receipt); registryErr != nil {
			return receipt, registryErr
		}
	}
	if runErr != nil || receipt.ExitCode != 0 {
		if runErr == nil {
			runErr = fmt.Errorf("step returned exit code %d", receipt.ExitCode)
		}
		return receipt, runErr
	}
	e.stepReceipts[step.ID] = receipt
	e.completeStep(step)
	return receipt, nil
}

func (e *structuredExecutionRun) selectedModel(step ExecutionStep, repairAttempt int) string {
	if e.request.SelectModel == nil {
		return ""
	}
	return e.request.SelectModel(step, repairAttempt)
}

func (e *structuredExecutionRun) recordPreflightFailure(step ExecutionStep, repairAttempt int, cause error) ExecutionStepReceipt {
	e.receiptCount++
	now := time.Now().UTC()
	receipt := ExecutionStepReceipt{
		ID:     structuredReceiptID(e.request.TaskID, e.request.Attempt, step.ID, repairAttempt, e.receiptCount),
		TaskID: e.request.TaskID, Attempt: e.request.Attempt, RepairAttempt: repairAttempt,
		StepID: step.ID, Tool: step.Tool, InputSHA256: structuredInputDigest(step.Input),
		StartedAt: now, FinishedAt: now, ExitCode: 1, Stderr: cause.Error(), PolicyVerdict: "denied",
		FailureClass: "policy",
	}
	receipt.StderrRef = executionOutputRef(receipt.ID, "stderr", receipt.Stderr)
	if step.Effect == ExecutionEffectValidate {
		receipt.ValidatorVerdict = "fail"
	}
	e.result.Receipts = append(e.result.Receipts, receipt)
	if e.request.Registry != nil {
		_ = e.request.Registry.Record(receipt)
	}
	return receipt
}

func (e *structuredExecutionRun) validateMutationInputs(step ExecutionStep) error {
	if step.Effect != ExecutionEffectMutate {
		return nil
	}
	for _, name := range step.Consumes {
		artifact, present := e.result.Artifacts[name]
		frozen, frozenPresent := e.result.FrozenArtifacts[name]
		if !present || !frozenPresent || artifact.SHA256 == "" || artifact.SHA256 != frozen.SHA256 {
			return fmt.Errorf("mutation step %q requires frozen artifact %q", step.ID, name)
		}
		if inspector, ok := e.runner.(StructuredArtifactInspector); ok {
			current, err := inspector.InspectStructuredArtifact(e.ctx, artifact)
			if err != nil {
				return fmt.Errorf("mutation step %q cannot inspect frozen artifact %q: %w", step.ID, name, err)
			}
			if current.SHA256 == "" || current.SHA256 != frozen.SHA256 {
				return fmt.Errorf("mutation step %q artifact %q digest changed after validation", step.ID, name)
			}
		}
	}
	return nil
}

func (e *structuredExecutionRun) newReceipt(step ExecutionStep, repairAttempt int, startedAt time.Time, stepResult ExecutionStepResult, runErr error, resolvedInput map[string]any) ExecutionStepReceipt {
	receipt := ExecutionStepReceipt{
		ID:     structuredReceiptID(e.request.TaskID, e.request.Attempt, step.ID, repairAttempt, e.receiptCount),
		TaskID: e.request.TaskID, Attempt: e.request.Attempt, RepairAttempt: repairAttempt,
		StepID: step.ID, Tool: step.Tool, InputSHA256: structuredInputDigest(resolvedInput),
		StartedAt: startedAt, FinishedAt: time.Now().UTC(), ExitCode: stepResult.ExitCode,
		Stdout: stepResult.Stdout, Stderr: stepResult.Stderr, Consumed: namedArtifacts(e.result.Artifacts, step.Consumes),
		ConsumedDigests: namedArtifactDigests(e.result.Artifacts, step.Consumes),
		PolicyVerdict:   normalizedPolicyVerdict(stepResult.PolicyVerdict),
	}
	receipt.StdoutRef = executionOutputRef(receipt.ID, "stdout", stepResult.Stdout)
	receipt.StderrRef = executionOutputRef(receipt.ID, "stderr", stepResult.Stderr)
	if step.Effect == ExecutionEffectValidate {
		receipt.ValidatorVerdict = "fail"
		if runErr == nil && stepResult.ExitCode == 0 {
			receipt.ValidatorVerdict = "pass"
		}
	}
	if receipt.ExitCode != 0 {
		receipt.FailureClass = normalizedExecutionFailureClass(stepResult.FailureClass)
		if step.Effect == ExecutionEffectValidate {
			receipt.FailureClass = "validation"
		} else if receipt.PolicyVerdict != "allowed" {
			receipt.FailureClass = "policy"
		}
	}
	return receipt
}

func (e *structuredExecutionRun) completeStep(step ExecutionStep) {
	if step.Effect == ExecutionEffectValidate {
		e.setState(StructuredExecutionValidated)
		for _, name := range step.Consumes {
			e.result.FrozenArtifacts[name] = e.result.Artifacts[name]
		}
		e.setState(StructuredExecutionFrozen)
	}
	if step.Effect == ExecutionEffectVerify {
		e.setState(StructuredExecutionVerified)
	}
	e.stepSucceeded[step.ID] = true
}

func (e *structuredExecutionRun) repairValidator(ordered []ExecutionStep, validator ExecutionStep) error {
	e.setState(StructuredExecutionRepairableFailed)
	producers := repairableProducerSteps(ordered, validator)
	for repairAttempt := 1; repairAttempt <= validator.MaxRepairs; repairAttempt++ {
		for _, producer := range producers {
			if _, err := e.runStep(producer, repairAttempt); err != nil {
				e.setState(StructuredExecutionFailed)
				receipt := e.result.Receipts[len(e.result.Receipts)-1]
				return structuredExecutionFailure(producer, receipt, normalizedExecutionFailureClass(receipt.FailureClass), err)
			}
		}
		receipt, err := e.runStep(validator, repairAttempt)
		if err == nil {
			return nil
		}
		if repairAttempt == validator.MaxRepairs {
			e.setState(StructuredExecutionFailed)
			return structuredExecutionFailure(validator, receipt, "validation", err)
		}
		e.setState(StructuredExecutionRepairableFailed)
	}
	return nil
}

func structuredExecutionFailure(step ExecutionStep, receipt ExecutionStepReceipt, class string, cause error) error {
	return &StructuredExecutionError{StepID: step.ID, ReceiptID: receipt.ID, Class: class, ExitCode: receipt.ExitCode, Cause: cause}
}

func repairableProducerSteps(ordered []ExecutionStep, validator ExecutionStep) []ExecutionStep {
	ancestors := structuredStepAncestors(ordered, validator.ID)
	var producers []ExecutionStep
	for _, step := range ordered {
		if ancestors[step.ID] && step.Effect == ExecutionEffectProduce {
			producers = append(producers, step)
		}
	}
	return producers
}

func cloneArtifactMap(values map[string]ArtifactRef) map[string]ArtifactRef {
	clone := make(map[string]ArtifactRef, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func namedArtifacts(artifacts map[string]ArtifactRef, names []string) []ArtifactRef {
	refs := make([]ArtifactRef, 0, len(names))
	for _, name := range names {
		if artifact, ok := artifacts[name]; ok {
			refs = append(refs, artifact)
		}
	}
	return refs
}

func structuredInputDigest(input map[string]any) string {
	data, err := json.Marshal(input)
	if err != nil {
		data = []byte("<unserializable>")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func structuredReceiptID(taskID string, attempt int, stepID string, repairAttempt, ordinal int) string {
	data := fmt.Sprintf("%s:%d:%s:%d:%d", taskID, attempt, stepID, repairAttempt, ordinal)
	sum := sha256.Sum256([]byte(data))
	return "receipt-" + hex.EncodeToString(sum[:8])
}

func validateStructuredExecutionSteps(steps []ExecutionStep) []ContractFinding {
	if len(steps) == 0 {
		return []ContractFinding{{Severity: FindingSeverityError, Code: "execution_steps_empty", Field: "execution.steps", Message: "structured execution requires at least one step"}}
	}
	findings, byID, outputProducer, outputKinds := validateStructuredStepDeclarations(steps)
	findings = append(findings, validateStructuredStepDependencies(steps, byID, outputProducer, outputKinds)...)
	if _, err := orderedStructuredExecutionSteps(steps); err != nil {
		findings = append(findings, structuredFinding("execution.steps", "execution_step_cycle", err.Error()))
		return findings
	}
	findings = append(findings, validateStructuredStepReferences(steps)...)
	return append(findings, validateStructuredMutationAncestry(steps)...)
}

func validateStructuredStepDeclarations(steps []ExecutionStep) ([]ContractFinding, map[string]ExecutionStep, map[string]string, map[string]ExecutionOutputKind) {
	var findings []ContractFinding
	byID := make(map[string]ExecutionStep, len(steps))
	outputProducer := make(map[string]string)
	outputKinds := make(map[string]ExecutionOutputKind)
	for i, step := range steps {
		path := fmt.Sprintf("execution.steps[%d]", i)
		id := strings.TrimSpace(step.ID)
		if id == "" {
			findings = append(findings, structuredFinding(path+".id", "execution_step_id", "step id is required"))
			continue
		}
		if _, exists := byID[id]; exists {
			findings = append(findings, structuredFinding(path+".id", "execution_step_duplicate_id", fmt.Sprintf("duplicate step id %q", id)))
			continue
		}
		byID[id] = step
		if strings.TrimSpace(step.Tool) == "" {
			findings = append(findings, structuredFinding(path+".tool", "execution_step_tool", "step tool is required"))
		}
		switch step.Effect {
		case ExecutionEffectRead, ExecutionEffectProduce, ExecutionEffectValidate, ExecutionEffectMutate, ExecutionEffectVerify:
		default:
			findings = append(findings, structuredFinding(path+".effect", "execution_step_effect", fmt.Sprintf("invalid step effect %q", step.Effect)))
		}
		if step.Effect == ExecutionEffectProduce && len(step.Outputs) == 0 {
			findings = append(findings, structuredFinding(path+".outputs", "execution_produce_outputs", "produce step must declare at least one output"))
		}
		if (step.Effect == ExecutionEffectValidate || step.Effect == ExecutionEffectMutate) && len(step.Consumes) == 0 {
			findings = append(findings, structuredFinding(path+".consumes", "execution_artifact_consumes", fmt.Sprintf("%s step must consume at least one artifact", step.Effect)))
		}
		policy := normalizedFailurePolicy(step.OnFailure)
		if policy != StepFailureTerminal && policy != StepFailureRepairable {
			findings = append(findings, structuredFinding(path+".on_failure", "execution_step_failure_policy", fmt.Sprintf("invalid failure policy %q", step.OnFailure)))
		}
		if policy == StepFailureRepairable && step.Effect != ExecutionEffectValidate {
			findings = append(findings, structuredFinding(path+".on_failure", "execution_repair_scope", "only validate steps may use repairable failure policy"))
		}
		if policy == StepFailureRepairable && step.MaxRepairs <= 0 {
			findings = append(findings, structuredFinding(path+".max_repairs", "execution_repair_limit", "repairable validation requires max_repairs greater than zero"))
		}
		if policy != StepFailureRepairable && step.MaxRepairs != 0 {
			findings = append(findings, structuredFinding(path+".max_repairs", "execution_repair_limit", "max_repairs is valid only for repairable validation"))
		}
		for _, output := range step.Outputs {
			name := strings.TrimSpace(output.Name)
			if name == "" {
				findings = append(findings, structuredFinding(path+".outputs", "execution_output_name", "step output name is required"))
				continue
			}
			if prior, exists := outputProducer[name]; exists {
				findings = append(findings, structuredFinding(path+".outputs", "execution_output_duplicate", fmt.Sprintf("output %q is already produced by step %q", name, prior)))
				continue
			}
			outputProducer[name] = id
			kind := normalizedExecutionOutputKind(output.Kind)
			switch kind {
			case ExecutionOutputArtifact, ExecutionOutputFact, ExecutionOutputReceipt:
				outputKinds[name] = kind
			default:
				findings = append(findings, structuredFinding(path+".outputs", "execution_output_kind", fmt.Sprintf("output %q has invalid kind %q", name, output.Kind)))
			}
			if output.Scope != "" && output.Scope != "task" && output.Scope != "secret" {
				findings = append(findings, structuredFinding(path+".outputs", "execution_output_scope", fmt.Sprintf("output %q has invalid scope %q", name, output.Scope)))
			}
			if output.Scope == "secret" && kind == ExecutionOutputFact {
				findings = append(findings, structuredFinding(path+".outputs", "execution_secret_fact", fmt.Sprintf("output %q cannot persist a secret fact value; use a secret-scoped artifact reference", name)))
			}
		}
	}
	return findings, byID, outputProducer, outputKinds
}

func validateStructuredStepDependencies(steps []ExecutionStep, byID map[string]ExecutionStep, outputProducer map[string]string, outputKinds map[string]ExecutionOutputKind) []ContractFinding {
	var findings []ContractFinding
	for i, step := range steps {
		path := fmt.Sprintf("execution.steps[%d]", i)
		seenDependencies := make(map[string]bool)
		for _, dependency := range step.DependsOn {
			if dependency == step.ID {
				findings = append(findings, structuredFinding(path+".depends_on", "execution_step_self_dependency", fmt.Sprintf("step %q cannot depend on itself", step.ID)))
			}
			if seenDependencies[dependency] {
				findings = append(findings, structuredFinding(path+".depends_on", "execution_step_duplicate_dependency", fmt.Sprintf("step %q repeats dependency %q", step.ID, dependency)))
			}
			seenDependencies[dependency] = true
			if _, ok := byID[dependency]; !ok {
				findings = append(findings, structuredFinding(path+".depends_on", "execution_step_missing_dependency", fmt.Sprintf("step %q depends on unknown step %q", step.ID, dependency)))
			}
		}
		for _, artifact := range step.Consumes {
			if _, ok := outputProducer[artifact]; !ok {
				findings = append(findings, structuredFinding(path+".consumes", "execution_step_missing_artifact", fmt.Sprintf("step %q consumes undeclared artifact %q", step.ID, artifact)))
			} else if outputKinds[artifact] != ExecutionOutputArtifact {
				findings = append(findings, structuredFinding(path+".consumes", "execution_step_non_artifact_consume", fmt.Sprintf("step %q consumes output %q as an artifact, but its kind is %q", step.ID, artifact, outputKinds[artifact])))
			}
		}
	}
	return findings
}

func validateStructuredMutationAncestry(steps []ExecutionStep) []ContractFinding {
	var findings []ContractFinding
	for _, step := range steps {
		if step.Effect != ExecutionEffectMutate {
			continue
		}
		ancestors := structuredStepAncestors(steps, step.ID)
		for _, artifact := range step.Consumes {
			validated := false
			for _, candidate := range steps {
				if ancestors[candidate.ID] && candidate.Effect == ExecutionEffectValidate && executionContainsString(candidate.Consumes, artifact) {
					validated = true
					break
				}
			}
			if !validated {
				findings = append(findings, structuredFinding("execution.steps."+step.ID+".consumes", "execution_mutation_unvalidated_artifact", fmt.Sprintf("mutation step %q must depend on successful validation of artifact %q", step.ID, artifact)))
			}
		}
	}
	return findings
}

func structuredFinding(field, code, message string) ContractFinding {
	return ContractFinding{Severity: FindingSeverityError, Code: code, Field: field, Message: message}
}

func hasStructuredExecutionErrors(findings []ContractFinding) bool {
	for _, finding := range findings {
		if finding.Severity == FindingSeverityError {
			return true
		}
	}
	return false
}

func formatStructuredExecutionFindings(findings []ContractFinding) string {
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, finding.Message)
	}
	return strings.Join(parts, "; ")
}

func orderedStructuredExecutionSteps(steps []ExecutionStep) ([]ExecutionStep, error) {
	byID := make(map[string]ExecutionStep, len(steps))
	remaining := make(map[string]int, len(steps))
	children := make(map[string][]string, len(steps))
	for _, step := range steps {
		if step.ID == "" {
			return nil, fmt.Errorf("structured execution step id is required")
		}
		if _, exists := byID[step.ID]; exists {
			return nil, fmt.Errorf("duplicate structured execution step id %q", step.ID)
		}
		byID[step.ID] = step
		remaining[step.ID] = len(step.DependsOn)
		for _, dependency := range step.DependsOn {
			if _, ok := byID[dependency]; !ok && !containsStepID(steps, dependency) {
				return nil, fmt.Errorf("step %q depends on unknown step %q", step.ID, dependency)
			}
			children[dependency] = append(children[dependency], step.ID)
		}
	}
	ready := make([]string, 0, len(steps))
	for _, step := range steps {
		if remaining[step.ID] == 0 {
			ready = append(ready, step.ID)
		}
	}
	var ordered []ExecutionStep
	for len(ready) > 0 {
		sort.Strings(ready)
		id := ready[0]
		ready = ready[1:]
		ordered = append(ordered, byID[id])
		for _, child := range children[id] {
			remaining[child]--
			if remaining[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(ordered) != len(steps) {
		return nil, fmt.Errorf("structured execution dependencies contain a cycle")
	}
	return ordered, nil
}

func containsStepID(steps []ExecutionStep, id string) bool {
	for _, step := range steps {
		if step.ID == id {
			return true
		}
	}
	return false
}

func structuredStepAncestors(steps []ExecutionStep, id string) map[string]bool {
	byID := make(map[string]ExecutionStep, len(steps))
	for _, step := range steps {
		byID[step.ID] = step
	}
	ancestors := make(map[string]bool)
	var visit func(string)
	visit = func(stepID string) {
		step, ok := byID[stepID]
		if !ok {
			return
		}
		for _, dependency := range step.DependsOn {
			if ancestors[dependency] {
				continue
			}
			ancestors[dependency] = true
			visit(dependency)
		}
	}
	visit(id)
	return ancestors
}

func normalizedFailurePolicy(policy StepFailurePolicy) StepFailurePolicy {
	if policy == "" {
		return StepFailureTerminal
	}
	return policy
}

func executionContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func structuredStepsContainEffect(steps []ExecutionStep, effect ExecutionEffect) bool {
	for _, step := range steps {
		if step.Effect == effect {
			return true
		}
	}
	return false
}
