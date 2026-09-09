package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// ErrTasksUnresolved marks a completed coordinator response that still has
// failed or blocked tasks. Callers must not report this as a successful run.
var ErrTasksUnresolved = errors.New("tasks unresolved")

type StatusEvent struct {
	Type       string // "start", "step", "tool_call", "tool_result", "codex_activity", "done", "error", "text", "todos_updated", "skill_used", "loop_warning", "timing", "judge", "skeptic", "memory_learning", "budget_exceeded", "task_timeout", "model_profile_resolved"
	TeamName   string
	Agent      string
	Message    string
	ToolName   string
	ToolArgs   string
	ToolResult string
	Step       int
	Todos      []*TodoItem
	Decisions  []DecisionIndexEntry
	SkillName  string
	Model      string
	Duration   time.Duration
	ModelTime  time.Duration
	ToolTime   time.Duration
	TodoID     string // ID of the TodoItem this event belongs to (set for worker-task events)
	Output     string // Final output text (set in done events for task-level events)
	// Execution identity is populated from the durable TodoItem for task-level
	// events. These are metadata-only canonical values, never credentials.
	ExecutionTarget string `json:"execution_target,omitempty"`
	Backend         string `json:"backend,omitempty"`
	BackendKind     string `json:"backend_kind,omitempty"`
	SSHSessions     int
	Data            map[string]any
	// ContextWindowTelemetry is typed and content-free. It is separate from
	// Data so admission reporting cannot accidentally carry model content.
	ContextWindowTelemetry *ContextWindowTelemetryEvent
	// ModelProfile is a secret-free effective profile projection.
	ModelProfile *modelprofile.TelemetryProjection
}

func (e StatusEvent) withData(data map[string]any) StatusEvent {
	e.Data = data
	return e
}

func (e StatusEvent) withOutput(output string) StatusEvent {
	e.Output = output
	return e
}

func (e StatusEvent) withAgent(agent string) StatusEvent {
	e.Agent = agent
	return e
}

func (e StatusEvent) withMessage(msg string) StatusEvent {
	e.Message = msg
	return e
}

func (e StatusEvent) withStep(step int) StatusEvent {
	e.Step = step
	return e
}

func (e StatusEvent) withTool(name, args string) StatusEvent {
	e.ToolName = name
	e.ToolArgs = args
	return e
}

func (e StatusEvent) withToolResult(name, result string) StatusEvent {
	e.ToolName = name
	e.ToolResult = result
	return e
}

func (e StatusEvent) withTodos(todos []*TodoItem) StatusEvent {
	e.Todos = todos
	return e
}

func (e StatusEvent) withSkillName(name string) StatusEvent {
	e.SkillName = name
	return e
}

func (e StatusEvent) withModel(model string) StatusEvent {
	e.Model = model
	return e
}

func (e StatusEvent) withTiming(duration, modelTime, toolTime time.Duration) StatusEvent {
	e.Duration = duration
	e.ModelTime = modelTime
	e.ToolTime = toolTime
	return e
}

func (e StatusEvent) withTodoID(id string) StatusEvent {
	e.TodoID = id
	return e
}

type StatusReporter func(event StatusEvent)

type TaskStatus string

type TaskKind string

const (
	TaskKindOutcome    TaskKind = "outcome"
	TaskKindRepair     TaskKind = "repair"
	TaskKindDiagnostic TaskKind = "diagnostic"
)

type TaskProgress string

const (
	ProgressUnknown   TaskProgress = "unknown"
	ProgressAdvanced  TaskProgress = "advanced"
	ProgressNoChange  TaskProgress = "no_change"
	ProgressRegressed TaskProgress = "regressed"
)

const (
	TaskPending            TaskStatus = "pending"
	TaskInProgress         TaskStatus = "in_progress"
	TaskVerifying          TaskStatus = "verifying"
	TaskDone               TaskStatus = "done"
	TaskError              TaskStatus = "error"
	TaskBlocked            TaskStatus = "blocked"
	TaskSkipped            TaskStatus = "skipped"
	TaskPlanned            TaskStatus = "planned"
	TaskPaused             TaskStatus = "paused"
	TaskProtocolIncomplete TaskStatus = "protocol_incomplete"
)

// VerificationResult is the durable evidence produced by a task's objective
// verification command. Output is intentionally bounded by the verifier.
type VerificationResult struct {
	Command        string            `json:"command,omitempty"`
	WorkDir        string            `json:"work_dir,omitempty"`
	ExitCode       int               `json:"exit_code"`
	Stdout         string            `json:"stdout,omitempty"`
	Stderr         string            `json:"stderr,omitempty"`
	Duration       time.Duration     `json:"duration,omitempty"`
	TimedOut       bool              `json:"timed_out,omitempty"`
	WeakWarning    bool              `json:"weak_warning,omitempty"`
	WeakReason     string            `json:"weak_reason,omitempty"`
	Overturned     bool              `json:"overturned,omitempty"`
	OverturnReason string            `json:"overturn_reason,omitempty"`
	Fingerprint    string            `json:"fingerprint,omitempty"`
	EvaluatedAt    time.Time         `json:"evaluated_at,omitempty"`
	Spec           *VerificationSpec `json:"spec,omitempty"`
	// rawStdout is intentionally not serialized. Command verification keeps
	// persisted evidence bounded, while json_assert needs the complete output
	// from the same command invocation to parse a single JSON document.
	rawStdout string
}

// CanTransition reports whether a normal lifecycle update may move a task
// between the supplied states. DAG retries use ResetForRetry explicitly.
func CanTransition(from, to TaskStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case TaskPending:
		return to == TaskPlanned || to == TaskInProgress || to == TaskDone || to == TaskSkipped || to == TaskBlocked || to == TaskError || to == TaskProtocolIncomplete
	case TaskPlanned:
		return to == TaskInProgress || to == TaskSkipped || to == TaskBlocked || to == TaskError || to == TaskProtocolIncomplete
	case TaskInProgress:
		return to == TaskPlanned || to == TaskPaused || to == TaskVerifying || to == TaskDone || to == TaskBlocked || to == TaskError || to == TaskSkipped || to == TaskProtocolIncomplete
	case TaskProtocolIncomplete:
		return to == TaskVerifying || to == TaskDone || to == TaskBlocked || to == TaskError || to == TaskInProgress
	case TaskVerifying:
		return to == TaskDone || to == TaskBlocked || to == TaskError
	case TaskPaused:
		return to == TaskInProgress || to == TaskSkipped || to == TaskBlocked || to == TaskError
	case TaskError:
		// A terminal execution error may be refined into blocked when
		// reconciliation proves that replay is unsafe (for example a
		// protocol-only failure after an external side effect).
		return to == TaskInProgress || to == TaskBlocked
	case TaskBlocked:
		return to == TaskInProgress
	case TaskDone, TaskSkipped:
		return false
	default:
		return false
	}
}

const (
	TaskSourceCoordinator = "coordinator"
	TaskSourceAgent       = "agent"
	TaskSourceSubagent    = "subagent"
)

type TaskTracker struct {
	todo *TodoList
}

func NewTaskTracker() *TaskTracker {
	return &TaskTracker{
		todo: &TodoList{},
	}
}

func (t *TaskTracker) TodoList() *TodoList {
	return t.todo
}

type TodoItem struct {
	ID               string
	Phase            Phase   `json:"phase,omitempty"`
	Action           *Action `json:"action,omitempty"`
	PlanTaskID       string  `json:"plan_task_id,omitempty"`
	PlanFirst        bool    `json:"plan_first,omitzero"`
	PlanID           string  `json:"plan_id,omitempty"`
	ContractID       string  `json:"contract_id,omitempty"`
	ContractHash     string  `json:"contract_hash,omitempty"`
	ContractRevision int     `json:"contract_revision,omitempty"`
	Agent            string
	Desc             string
	Goal             string `json:"goal,omitempty"`
	Constraints      string `json:"constraints,omitempty"`
	Status           TaskStatus
	Detail           string
	Output           string // Full task output
	// CheckpointPause marks a deliberate scheduler pause. It is durable
	// lifecycle metadata so crash recovery does not mistake an operator-facing
	// checkpoint pause for an interrupted worker that may be replayed.
	CheckpointPause bool `json:"checkpoint_pause,omitempty"`
	// OccurrenceRevision changes whenever the durable lifecycle projection is
	// advanced. DispatchID identifies the currently admitted worker lease and
	// is intentionally different for every dispatch, including resume.
	OccurrenceRevision int    `json:"occurrence_revision,omitempty"`
	DispatchID         string `json:"dispatch_id,omitempty"`
	// Model is retained in memory as a compatibility shadow for callers that
	// still build TaskDefs from a TodoItem. Canonical occurrences persist their
	// typed ExecutionTarget instead; MarshalJSON suppresses this field whenever
	// that target is present.
	Model string `json:"model,omitempty"`
	// ModelTopology is the immutable ordered model topology for this durable
	// task occurrence. The first model is the primary; remaining models are
	// explicit initial fanout leaves.
	ModelTopology []string `json:"model_topology,omitempty"`
	// ExecutionTarget and ExecutionTopology are the canonical, immutable
	// execution identity admitted for this occurrence. Legacy model/provider
	// fields are dual-written only during the migration period.
	ExecutionTarget   execution.ExecutionTarget   `json:"execution_target,omitzero"`
	ExecutionTopology []execution.ExecutionTarget `json:"execution_topology,omitempty"`
	Sidecar           bool                        `json:"sidecar,omitempty"`
	Summarize         bool                        `json:"summarize,omitempty"`
	OutputMode        string                      `json:"output_mode,omitempty"`
	ContextFiles      []string                    `json:"context_files,omitempty"`
	Requires          []string                    `json:"requires,omitempty"`
	Skills            []string
	InjectedSkills    []string
	LoadedSkills      []string
	StartedAt         time.Time
	EndedAt           time.Time
	ModelTime         time.Duration
	ToolTime          time.Duration
	Source            string
	ParentID          string
	DependsOn         []string                 // IDs of tasks that must complete before this one starts
	Verify            string                   // Command to run to verify the task
	VerifyMode        string                   // success, expected_failure, or observation
	VerifySpec        *VerificationSpec        `json:"verify_spec,omitempty"`
	WorksetBinding    *WorksetBinding          `json:"workset_binding,omitempty"`
	WorksetReceipt    *WorksetExpansionReceipt `json:"workset_receipt,omitempty"`
	VerifyResult      *VerificationResult
	// RuntimeError preserves a structured runtime/provider failure so phase
	// aggregation does not degrade it into an unclassified worker error.
	RuntimeError      *ExecutionError      `json:"runtime_error,omitempty"`
	ExecutionReceipt  *ExecutionReceipt    `json:"execution_receipt,omitempty"`
	ExecutionReceipts []ExecutionReceipt   `json:"execution_receipts,omitempty"`
	FailureEvent      *FailureEventPayload `json:"failure_event,omitempty"`
	// RemediationContext carries the canonical failure/result of the task
	// whose on_failure back-edge most recently reset this task (spec.md
	// §9.3). It is set by dagScheduler right before an authorized semantic
	// reset and read (non-destructively) each time this occurrence is
	// dispatched; the next genuine reset always replaces it with fresh
	// evidence rather than stacking.
	RemediationContext *RemediationContext `json:"remediation_context,omitempty"`
	MaxRetries         int                 // Maximum number of retries for this task
	Retries            int                 // Current number of retries
	OnFailure          string              // ID of the task to jump back to if this task fails (creates a loop)
	// OnFailureClasses mirrors TaskDef.OnFailureClasses. It must be carried
	// through the durable TodoItem (not just the coordinator's in-memory
	// TaskDef) because task_occurrence_projection.go's
	// compareTaskDefWithTodoOccurrence reconstructs a TaskDef from this
	// TodoItem via taskDefFromTodoItem and rejects the whole dispatch if it
	// differs from the originally-supplied TaskDef.
	OnFailureClasses    []TaskFailureClass   `json:"on_failure_classes,omitempty"`
	Escalate            bool                 `json:"escalate,omitempty"`
	AdversarialVerify   int                  `json:"adversarial_verify,omitempty"`
	SideEffect          SideEffectClass      `json:"side_effect,omitempty"`
	Recovery            RecoveryPolicy       `json:"recovery,omitempty"`
	ReconcileTool       string               `json:"reconcile_tool,omitempty"`
	RecoveryState       string               `json:"recovery_state,omitempty"`
	TypedResult         *TaskResult          `json:"typed_result,omitempty"`
	Resolution          *TaskResolution      `json:"resolution,omitempty"`
	Kind                TaskKind             `json:"kind,omitempty"`
	Advances            []string             `json:"advances,omitempty"`
	ExpectedStateChange string               `json:"expected_state_change,omitempty"`
	Progress            TaskProgress         `json:"progress,omitempty"`
	ProgressCriteria    []string             `json:"progress_criteria,omitempty"`
	FailureFingerprints []FailureFingerprint `json:"failure_fingerprints,omitempty"`
	RecoveryHypothesis  *RecoveryHypothesis  `json:"recovery_hypothesis,omitempty"`
	DiagnosticHints     []string             `json:"diagnostic_hints,omitempty"`
	LastOperation       string               `json:"last_operation,omitempty"`
	Execution           ExecutionContract    `json:"execution,omitempty"`
	Optional            bool                 `json:"optional,omitempty"`
	ResourceClaims      []string             `json:"resource_claims,omitempty"`
	Resources           []ResourceClaim      `json:"resources,omitempty"`
	// Decision admission is immutable task-contract state. It is persisted on
	// the todo before execution so crash recovery cannot silently downgrade a
	// configured decision task to the off-profile path.
	DecisionProfile     string                     `json:"decision_profile,omitempty"`
	DecisionOptions     []DecisionOption           `json:"decision_options,omitempty"`
	DecisionAssumptions []DecisionAssumption       `json:"decision_assumptions,omitempty"`
	DecisionFacts       map[string]any             `json:"decision_facts,omitempty"`
	DecisionArtifacts   []ArtifactRef              `json:"decision_artifacts,omitempty"`
	DecisionBaseRates   []BaseRateEvidence         `json:"decision_base_rates,omitempty"`
	DecisionProvenance  []EvidenceProvenance       `json:"decision_provenance,omitempty"`
	MemoryManifests     []MemoryInjectionManifest  `json:"memory_manifests,omitempty"`
	ContextManifests    []ContextInjectionManifest `json:"context_manifests,omitempty"`
	// SubagentProvider is immutable after task admission
	// (docs/hufu-external-coding-agent-runtime-spec.md §7.2).
	SubagentProvider string `json:"subagent_provider,omitempty"`
	// ProviderBinding carries the durable provider/session identity for this
	// occurrence. Its SessionID/TurnID MAY transition from empty to populated
	// as execution progresses; Provider itself does not change.
	ProviderBinding *ProviderBinding `json:"provider_binding,omitempty"`
	BackendBinding  *BackendBinding  `json:"backend_binding,omitempty"`
}

// MarshalJSON keeps historical target-less checkpoints/events readable while
// ensuring newly admitted typed occurrences do not continue the retired
// model/provider identity split. Unmarshal remains the ordinary struct
// decoder, so old checkpoints and event payloads remain decode-compatible.
func (item TodoItem) MarshalJSON() ([]byte, error) {
	type todoItemWire TodoItem
	wire := todoItemWire(item)
	if !item.ExecutionTarget.IsZero() {
		wire.Model = ""
		wire.ModelTopology = nil
		wire.SubagentProvider = ""
		wire.ProviderBinding = nil
	}
	return json.Marshal(wire)
}

func (item *TodoItem) UnmarshalJSON(data []byte) error {
	type todoItemWire TodoItem
	var wire todoItemWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*item = TodoItem(wire)
	materializeLegacyIdentityShadow(item)
	return nil
}

type TodoList struct {
	mu       sync.Mutex
	items    []*TodoItem
	next     int
	runID    string
	onChange func()
}

func (tl *TodoList) SetRunID(runID string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.runID = runID
}

func (tl *TodoList) RunID() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.runID
}

// TodoSpec describes a todo item to be created via AddBatch.
type TodoSpec struct {
	PlanTaskID        string
	PlanFirst         bool
	PlanID            string
	Phase             Phase
	Action            *Action
	ContractID        string
	ContractHash      string
	ContractRevision  int
	Agent             string
	Desc              string
	Goal              string
	Constraints       string
	Model             string
	ModelTopology     []string
	ExecutionTarget   execution.ExecutionTarget
	ExecutionTopology []execution.ExecutionTarget
	Sidecar           bool
	Summarize         bool
	OutputMode        string
	ContextFiles      []string
	Requires          []string
	Source            string
	ParentID          string
	Verify            string
	VerifyMode        string
	VerifySpec        *VerificationSpec
	WorksetBinding    *WorksetBinding
	WorksetReceipt    *WorksetExpansionReceipt
	MaxRetries        int
	OnFailure         string
	// OnFailureClasses mirrors TaskDef.OnFailureClasses (coordinator.go):
	// which TaskFailureClass values authorize this task's on_failure
	// back-edge. It must survive the durable TodoSpec/TodoItem round trip
	// (task_occurrence_projection.go's compareTaskDefWithTodoOccurrence
	// rebuilds the scheduler's TaskDef from the durable TodoItem via
	// taskDefFromTodoItem and rejects any mismatch) exactly like SideEffect/
	// Recovery already do, or a real hufu-coding dispatch fails admission
	// before a dagScheduler is ever constructed.
	OnFailureClasses    []TaskFailureClass
	Escalate            bool
	AdversarialVerify   int
	DependsOn           []string
	SideEffect          SideEffectClass
	Recovery            RecoveryPolicy
	ReconcileTool       string
	Kind                TaskKind
	Advances            []string
	ExpectedStateChange string
	RecoveryHypothesis  *RecoveryHypothesis
	Execution           ExecutionContract
	Optional            bool
	ResourceClaims      []string
	Resources           []ResourceClaim
	DecisionProfile     string
	DecisionOptions     []DecisionOption
	DecisionAssumptions []DecisionAssumption
	DecisionFacts       map[string]any
	DecisionArtifacts   []ArtifactRef
	DecisionBaseRates   []BaseRateEvidence
	DecisionProvenance  []EvidenceProvenance
	// SubagentProvider and ProviderBinding follow TaskDef's field of the same
	// name (docs/hufu-external-coding-agent-runtime-spec.md §7.2). Callers
	// normally only set SubagentProvider; todoItemFromSpec derives a minimal
	// ProviderBinding from it when ProviderBinding is left nil.
	SubagentProvider string
	ProviderBinding  *ProviderBinding
	BackendBinding   *BackendBinding
}

// todoItemFromSpec builds a pending TodoItem from a spec and an explicit ID.
// It is shared by AddBatch and the event-first CommitTaskCreation boundary so
// both paths produce byte-identical projection state.
func todoItemFromSpec(item TodoSpec, id string) *TodoItem {
	target := item.ExecutionTarget
	if target.IsZero() {
		target = targetFromLegacyIdentity(item.Model, item.SubagentProvider)
	}
	legacyIdentity := target.IsZero()
	providerBinding := item.ProviderBinding
	if legacyIdentity && providerBinding == nil && strings.TrimSpace(item.SubagentProvider) != "" {
		providerBinding = &ProviderBinding{Provider: item.SubagentProvider}
	}
	topology := item.ExecutionTopology
	if len(topology) == 0 {
		topology = topologyFromLegacyIdentity(item.ModelTopology, target, item.SubagentProvider)
	}
	backendBinding := item.BackendBinding
	if backendBinding == nil {
		backendBinding = backendBindingFromProviderBinding(providerBinding, target)
	}
	compatibilityProvider := strings.TrimSpace(item.SubagentProvider)
	if !legacyIdentity {
		// Keep a read-only in-memory shadow for old runtime adapters and tests;
		// TodoItem.MarshalJSON and canonical task events never persist it.
		compatibilityProvider = target.Backend
		if target.Backend == "local" {
			compatibilityProvider = localSubagentProviderName
		}
		providerBinding = nil
	}
	return &TodoItem{
		ID:                  id,
		PlanTaskID:          item.PlanTaskID,
		PlanFirst:           item.PlanFirst,
		PlanID:              item.PlanID,
		Phase:               item.Phase,
		Action:              cloneActionPtr(item.Action),
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               item.Agent,
		Desc:                item.Desc,
		Goal:                item.Goal,
		Constraints:         item.Constraints,
		Model:               item.Model,
		ModelTopology:       cloneModelTopology(item.ModelTopology),
		ExecutionTarget:     target,
		ExecutionTopology:   cloneExecutionTopology(topology),
		Sidecar:             item.Sidecar,
		Summarize:           item.Summarize,
		OutputMode:          item.OutputMode,
		ContextFiles:        append([]string(nil), item.ContextFiles...),
		Requires:            append([]string(nil), item.Requires...),
		Status:              TaskPending,
		Source:              item.Source,
		ParentID:            item.ParentID,
		Verify:              item.Verify,
		VerifyMode:          item.VerifyMode,
		VerifySpec:          item.VerifySpec,
		WorksetBinding:      cloneWorksetBinding(item.WorksetBinding),
		WorksetReceipt:      cloneWorksetReceipt(item.WorksetReceipt),
		MaxRetries:          item.MaxRetries,
		OnFailure:           item.OnFailure,
		OnFailureClasses:    append([]TaskFailureClass(nil), item.OnFailureClasses...),
		Escalate:            item.Escalate,
		AdversarialVerify:   item.AdversarialVerify,
		DependsOn:           append([]string(nil), item.DependsOn...),
		SideEffect:          item.SideEffect,
		Recovery:            item.Recovery,
		ReconcileTool:       item.ReconcileTool,
		Kind:                item.Kind,
		Advances:            append([]string(nil), item.Advances...),
		ExpectedStateChange: item.ExpectedStateChange,
		Progress:            ProgressUnknown,
		RecoveryHypothesis:  item.RecoveryHypothesis,
		Execution:           cloneExecutionContract(item.Execution),
		Optional:            item.Optional,
		ResourceClaims:      append([]string(nil), item.ResourceClaims...),
		Resources:           append([]ResourceClaim(nil), item.Resources...),
		DecisionProfile:     item.DecisionProfile,
		DecisionOptions:     append([]DecisionOption(nil), item.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(item.DecisionAssumptions),
		DecisionFacts:       cloneDecisionFacts(item.DecisionFacts),
		DecisionArtifacts:   append([]ArtifactRef(nil), item.DecisionArtifacts...),
		DecisionBaseRates:   cloneBaseRateEvidence(item.DecisionBaseRates),
		DecisionProvenance:  cloneEvidenceProvenance(item.DecisionProvenance),
		SubagentProvider:    compatibilityProvider,
		ProviderBinding:     cloneProviderBinding(providerBinding),
		BackendBinding:      cloneBackendBinding(backendBinding),
	}
}

func (tl *TodoList) AddBatch(items []TodoSpec) []*TodoItem {
	tl.mu.Lock()
	var added []*TodoItem
	for _, item := range items {
		tl.next++
		ti := todoItemFromSpec(item, fmt.Sprintf("%d", tl.next))
		tl.items = append(tl.items, ti)
		added = append(added, ti)
	}
	onChange := tl.onChange
	tl.mu.Unlock()

	if onChange != nil {
		onChange()
	}
	return added
}

// ReserveIDs advances the list's ID counter by count and returns the reserved
// IDs in order. It is the durable half of the event-first creation boundary:
// callers append the task_created events for these IDs before adding the items
// via AddReserved. A failed append leaves the counter advanced but no item in
// the projection, which is safe because IDs are opaque sequential strings.
func (tl *TodoList) ReserveIDs(count int) []string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	ids := make([]string, count)
	for i := range ids {
		tl.next++
		ids[i] = fmt.Sprintf("%d", tl.next)
	}
	return ids
}

// AddReserved appends already-constructed items (with pre-reserved IDs) and
// fires the change callback. It is the projection half of CommitTaskCreation.
func (tl *TodoList) AddReserved(items []*TodoItem) {
	if len(items) == 0 {
		return
	}
	tl.mu.Lock()
	tl.items = append(tl.items, items...)
	onChange := tl.onChange
	tl.mu.Unlock()

	if onChange != nil {
		onChange()
	}
}

func (tl *TodoList) DeleteIDs(ids ...string) {
	if len(ids) == 0 {
		return
	}

	tl.mu.Lock()
	remove := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			remove[id] = struct{}{}
		}
	}
	if len(remove) == 0 {
		tl.mu.Unlock()
		return
	}

	fresh := tl.items[:0]
	changed := false
	for _, item := range tl.items {
		if _, ok := remove[item.ID]; ok {
			changed = true
			continue
		}
		fresh = append(fresh, item)
	}
	tl.items = fresh
	onChange := tl.onChange
	tl.mu.Unlock()

	if changed && onChange != nil {
		onChange()
	}
}

func (tl *TodoList) UpdateStatus(id string, status TaskStatus, detail string) {
	_ = tl.TryUpdateStatusAndOutput(id, status, detail, "")
}

// AppendFailureFingerprint mutates the canonical task record (rather than a
// snapshot returned by Items) and checkpoints the change.
func (tl *TodoList) AppendFailureFingerprint(id string, fingerprint FailureFingerprint) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID != id {
			continue
		}
		duplicate := false
		for i := range ti.FailureFingerprints {
			existing := &ti.FailureFingerprints[i]
			if fingerprint.Digest != "" && existing.Digest == fingerprint.Digest {
				if existing.Occurrences < 1 {
					existing.Occurrences = 1
				}
				increment := fingerprint.Occurrences
				if increment < 1 {
					increment = 1
				}
				existing.Occurrences += increment
				duplicate = true
				break
			}
		}
		if !duplicate {
			if fingerprint.Occurrences < 1 {
				fingerprint.Occurrences = 1
			}
			ti.FailureFingerprints = append(ti.FailureFingerprints, fingerprint)
		}
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// AppendDiagnosticHint keeps a bounded reflection candidate on the canonical
// task record so the next diagnostic packet can include it.
func (tl *TodoList) AppendDiagnosticHint(id, hint string) error {
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item != nil && item.ID == id {
			item.DiagnosticHints = append(item.DiagnosticHints, hint)
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("todo item %q not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// SetLastOperation records task-local operation identity without checkpointing
// every tool call. The next lifecycle mutation persists the value.
func (tl *TodoList) SetLastOperation(id, operation string) {
	if strings.TrimSpace(operation) == "" {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.LastOperation = operation
			return
		}
	}
}

// SetPlanLifecycle applies the durable plan lifecycle projection after its
// event has been appended. Plan metadata is part of task identity, so callers
// must use the coordinator's event-first transition boundary when changing it.
func (tl *TodoList) SetPlanLifecycle(id string, planFirst bool, planID string) error {
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item == nil || item.ID != id {
			continue
		}
		item.PlanFirst = planFirst
		item.PlanID = planID
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func (tl *TodoList) UpdateStatusAndOutput(id string, status TaskStatus, detail string, output string) {
	_ = tl.TryUpdateStatusAndOutput(id, status, detail, output)
}

// TryUpdateStatusAndOutput applies a lifecycle transition and returns an error
// for unknown tasks or illegal transitions. Callers that make correctness
// decisions should use this method instead of the compatibility wrappers.
func (tl *TodoList) TryUpdateStatusAndOutput(id string, status TaskStatus, detail string, output string) error {
	tl.mu.Lock()
	updated := false
	var transitionErr error
	for _, ti := range tl.items {
		if ti.ID == id {
			if !CanTransition(ti.Status, status) {
				transitionErr = fmt.Errorf("invalid task status transition %s -> %s for task %s", ti.Status, status, id)
				break
			}
			ti.Status = status
			if detail != "" {
				ti.Detail = detail
			}
			if output != "" {
				ti.Output = output
			}
			switch status {
			case TaskInProgress:
				if ti.StartedAt.IsZero() {
					ti.StartedAt = time.Now()
				}
			case TaskDone, TaskError, TaskBlocked, TaskSkipped:
				if ti.EndedAt.IsZero() {
					ti.EndedAt = time.Now()
				}
			}
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()

	if updated && onChange != nil {
		onChange()
	}
	if transitionErr != nil {
		return transitionErr
	}
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	return nil
}

// TryUpdateStatusAndFailure applies a terminal transition and its structured
// failure evidence as one projection update. Keeping the evidence in the
// same onChange callback prevents checkpointing a terminal task once before
// its failure payload and again after it, which would create duplicate
// terminal events.
func (tl *TodoList) TryUpdateStatusAndFailure(id string, status TaskStatus, detail, output string, event *FailureEventPayload) error {
	tl.mu.Lock()
	updated := false
	var transitionErr error
	for _, ti := range tl.items {
		if ti.ID != id {
			continue
		}
		if !CanTransition(ti.Status, status) {
			transitionErr = fmt.Errorf("invalid task status transition %s -> %s for task %s", ti.Status, status, id)
			break
		}
		ti.Status = status
		if detail != "" {
			ti.Detail = detail
		}
		if output != "" {
			ti.Output = output
		}
		if event != nil {
			ti.FailureEvent = cloneFailureEventPayload(event)
			if summary := strings.TrimSpace(event.Summary); summary != "" {
				ti.Detail = summary
			}
		}
		switch status {
		case TaskInProgress:
			if ti.StartedAt.IsZero() {
				ti.StartedAt = time.Now()
			}
		case TaskDone, TaskError, TaskBlocked, TaskSkipped:
			if ti.EndedAt.IsZero() {
				ti.EndedAt = time.Now()
			}
		}
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if updated && onChange != nil {
		onChange()
	}
	if transitionErr != nil {
		return transitionErr
	}
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	return nil
}

func (tl *TodoList) SetRecoveryState(id string, state string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.RecoveryState = state
			return
		}
	}
}

func (tl *TodoList) SetVerificationResult(id string, result *VerificationResult) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			if result == nil {
				ti.VerifyResult = nil
				ti.RuntimeError = nil
				updated = true
				break
			}
			copyResult := *result
			ti.VerifyResult = &copyResult
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// SetProviderBinding updates one Todo's ProviderBinding in place. It never
// changes SubagentProvider (immutable after admission, §7.2); it is used
// only to attach/refresh the mutable runtime portion (session/turn identity)
// once an external provider establishes it mid-attempt.
func (tl *TodoList) SetProviderBinding(id string, binding *ProviderBinding) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.ProviderBinding = cloneProviderBinding(binding)
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// SetBackendBinding refreshes the canonical mutable backend session evidence.
func (tl *TodoList) SetBackendBinding(id string, binding *BackendBinding) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.BackendBinding = cloneBackendBinding(binding)
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func (tl *TodoList) SetRuntimeError(id string, runtimeErr *ExecutionError) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			if runtimeErr == nil {
				ti.RuntimeError = nil
			} else {
				copyErr := *runtimeErr
				ti.RuntimeError = &copyErr
			}
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func (tl *TodoList) SetTypedResult(id string, result *TaskResult) error {
	tl.mu.Lock()
	updated := false
	sec, _ := GetSystemSecret()
	for _, ti := range tl.items {
		if ti.ID == id {
			if result == nil {
				ti.TypedResult = nil
				updated = true
				break
			}
			copyResult := cloneTaskResult(result)
			if len(copyResult.Evidence) > 0 {
				cleanEv := make([]EvidenceRef, len(copyResult.Evidence))
				for i, ev := range copyResult.Evidence {
					if ev.TaskID == "" {
						ev.TaskID = id
					}
					// Only keep SystemHMAC if signature is valid system HMAC for this task & run!
					if sec != "" && VerifyEvidenceSignature(ev, sec, id, tl.runID) {
						cleanEv[i] = ev
					} else {
						ev.SystemHMAC = ""
						cleanEv[i] = ev
					}
				}
				copyResult.Evidence = cleanEv
			}
			ti.TypedResult = copyResult
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// SetFailureEvent stores self-contained failure evidence separately from the
// human-readable Detail string so terminal failure events can be replayed
// without parsing an error message.
func (tl *TodoList) SetFailureEvent(id string, event *FailureEventPayload) error {
	return tl.SetFailureEventAndOutput(id, event, "")
}

// SetFailureEventAndOutput atomically attaches failure evidence and an
// optional bounded output projection before a terminal transition is
// checkpointed. This prevents a same-status follow-up update from being
// deduplicated before the worker evidence reaches the event store.
func (tl *TodoList) SetFailureEventAndOutput(id string, event *FailureEventPayload, output string) error {
	tl.mu.Lock()
	updated := false
	notify := false
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.FailureEvent = cloneFailureEventPayload(event)
			// Detail must move with the evidence. Attaching a failure event
			// fires onChange, which projects the workspace status immediately —
			// before the caller's own status update lands. A run killed in that
			// window left behind "status: working" with "detail: Task completed
			// successfully" sitting directly above a failure_event with
			// class=execution, and nothing in the file said which half was
			// current. Setting both here makes the pair atomic.
			if event != nil {
				if summary := strings.TrimSpace(event.Summary); summary != "" {
					ti.Detail = summary
				}
			}
			if output != "" {
				ti.Output = output
			}
			updated = true
			notify = ti.Status == TaskError || ti.Status == TaskBlocked || ti.Status == TaskSkipped || ti.Status == TaskProtocolIncomplete
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if notify && onChange != nil {
		onChange()
	}
	return nil
}

// ResetForRetry returns a task to TaskPending so it can run again as part of
// an on_failure DAG loop. Unlike UpdateStatus, it deliberately bypasses the
// terminal-state protection (Done/Error are normally final) because a retry
// re-executes tasks that already completed. Timing fields are cleared so the
// re-run records fresh timestamps, and Retries is incremented.
func (tl *TodoList) ResetForRetry(id string, detail string) {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.Status = TaskPending
			ti.Detail = detail
			ti.Output = ""
			ti.VerifyResult = nil
			ti.RuntimeError = nil
			ti.RecoveryState = RecoveryStateNotStarted
			ti.LastOperation = ""
			ti.Progress = ProgressUnknown
			ti.ProgressCriteria = nil
			ti.StartedAt = time.Time{}
			ti.EndedAt = time.Time{}
			ti.ModelTime = 0
			ti.ToolTime = 0
			ti.Retries++
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()

	if updated && onChange != nil {
		onChange()
	}
}

func (tl *TodoList) Restore(items []*TodoItem) {
	tl.mu.Lock()
	tl.items = items
	maxId := 0
	for _, item := range items {
		var idVal int
		if _, err := fmt.Sscanf(item.ID, "%d", &idVal); err == nil {
			if idVal > maxId {
				maxId = idVal
			}
		}
	}
	tl.next = maxId
	onChange := tl.onChange
	tl.mu.Unlock()
	if onChange != nil {
		onChange()
	}
}

// cloneTodoItem returns a deep copy of item (slice and pointer fields detached)
// so callers can mutate the result without aliasing the source. Nil-safe.
func cloneTodoItem(item *TodoItem) *TodoItem {
	if item == nil {
		return nil
	}
	var skills []string
	if len(item.Skills) > 0 {
		skills = make([]string, len(item.Skills))
		copy(skills, item.Skills)
	}
	var injectedSkills []string
	if len(item.InjectedSkills) > 0 {
		injectedSkills = make([]string, len(item.InjectedSkills))
		copy(injectedSkills, item.InjectedSkills)
	}
	var loadedSkills []string
	if len(item.LoadedSkills) > 0 {
		loadedSkills = make([]string, len(item.LoadedSkills))
		copy(loadedSkills, item.LoadedSkills)
	}
	var diagnosticHints []string
	if len(item.DiagnosticHints) > 0 {
		diagnosticHints = append([]string(nil), item.DiagnosticHints...)
	}
	var dependsOn []string
	if len(item.DependsOn) > 0 {
		dependsOn = make([]string, len(item.DependsOn))
		copy(dependsOn, item.DependsOn)
	}
	var verifySpec *VerificationSpec
	if item.VerifySpec != nil {
		copySpec := cloneVerificationSpec(*item.VerifySpec)
		verifySpec = &copySpec
	}
	var verifyResult *VerificationResult
	if item.VerifyResult != nil {
		verifyResult = cloneVerificationResult(item.VerifyResult)
	}
	var runtimeErr *ExecutionError
	if item.RuntimeError != nil {
		copyErr := *item.RuntimeError
		copyErr.Evidence = append([]string(nil), item.RuntimeError.Evidence...)
		runtimeErr = &copyErr
	}
	var typedResult *TaskResult
	if item.TypedResult != nil {
		typedResult = cloneTaskResult(item.TypedResult)
	}
	failureEvent := cloneFailureEventPayload(item.FailureEvent)
	var resolution *TaskResolution
	if item.Resolution != nil {
		copyRes := *item.Resolution
		resolution = &copyRes
	}
	var execReceipt *ExecutionReceipt
	if item.ExecutionReceipt != nil {
		copyER := cloneExecutionReceipt(item.ExecutionReceipt)
		execReceipt = &copyER
	}
	var execReceipts []ExecutionReceipt
	if len(item.ExecutionReceipts) > 0 {
		execReceipts = make([]ExecutionReceipt, len(item.ExecutionReceipts))
		for i, r := range item.ExecutionReceipts {
			// Deep-copy via cloneExecutionReceipt so the snapshot's
			// RepairProvenance and VerifyResult pointers are independent of
			// the canonical item. A shallow copyR := r would share the
			// VerifyResult pointer, letting a caller mutate the snapshot's
			// verify result outside the todo lock and silently change
			// canonical evidence (race + corruption).
			execReceipts[i] = cloneExecutionReceipt(&r)
		}
	}
	memoryManifests := make([]MemoryInjectionManifest, len(item.MemoryManifests))
	for i := range item.MemoryManifests {
		memoryManifests[i] = *cloneMemoryInjectionManifest(&item.MemoryManifests[i])
	}
	contextManifests := make([]ContextInjectionManifest, len(item.ContextManifests))
	for i := range item.ContextManifests {
		contextManifests[i] = *cloneContextInjectionManifest(&item.ContextManifests[i])
	}
	return &TodoItem{
		ID:                  item.ID,
		Phase:               item.Phase,
		Action:              cloneActionPtr(item.Action),
		PlanTaskID:          item.PlanTaskID,
		PlanFirst:           item.PlanFirst,
		PlanID:              item.PlanID,
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               item.Agent,
		Goal:                item.Goal,
		Constraints:         item.Constraints,
		Desc:                item.Desc,
		Status:              item.Status,
		Detail:              item.Detail,
		Output:              item.Output,
		CheckpointPause:     item.CheckpointPause,
		OccurrenceRevision:  item.OccurrenceRevision,
		DispatchID:          item.DispatchID,
		Model:               item.Model,
		ModelTopology:       cloneModelTopology(item.ModelTopology),
		ExecutionTarget:     item.ExecutionTarget,
		ExecutionTopology:   cloneExecutionTopology(item.ExecutionTopology),
		Sidecar:             item.Sidecar,
		Summarize:           item.Summarize,
		OutputMode:          item.OutputMode,
		ContextFiles:        append([]string(nil), item.ContextFiles...),
		Requires:            append([]string(nil), item.Requires...),
		Skills:              skills,
		InjectedSkills:      injectedSkills,
		LoadedSkills:        loadedSkills,
		StartedAt:           item.StartedAt,
		EndedAt:             item.EndedAt,
		ModelTime:           item.ModelTime,
		ToolTime:            item.ToolTime,
		Source:              item.Source,
		ParentID:            item.ParentID,
		DependsOn:           dependsOn,
		Verify:              item.Verify,
		VerifyMode:          item.VerifyMode,
		VerifySpec:          verifySpec,
		WorksetBinding:      cloneWorksetBinding(item.WorksetBinding),
		WorksetReceipt:      cloneWorksetReceipt(item.WorksetReceipt),
		VerifyResult:        verifyResult,
		RuntimeError:        runtimeErr,
		ExecutionReceipt:    execReceipt,
		ExecutionReceipts:   execReceipts,
		FailureEvent:        failureEvent,
		RemediationContext:  cloneRemediationContext(item.RemediationContext),
		MaxRetries:          item.MaxRetries,
		Retries:             item.Retries,
		OnFailure:           item.OnFailure,
		OnFailureClasses:    append([]TaskFailureClass(nil), item.OnFailureClasses...),
		Escalate:            item.Escalate,
		AdversarialVerify:   item.AdversarialVerify,
		SideEffect:          item.SideEffect,
		Recovery:            item.Recovery,
		ReconcileTool:       item.ReconcileTool,
		RecoveryState:       item.RecoveryState,
		TypedResult:         typedResult,
		Resolution:          resolution,
		Kind:                item.Kind,
		Advances:            append([]string(nil), item.Advances...),
		ExpectedStateChange: item.ExpectedStateChange,
		Progress:            item.Progress,
		ProgressCriteria:    append([]string(nil), item.ProgressCriteria...),
		FailureFingerprints: append([]FailureFingerprint(nil), item.FailureFingerprints...),
		RecoveryHypothesis:  cloneRecoveryHypothesis(item.RecoveryHypothesis),
		DiagnosticHints:     diagnosticHints,
		LastOperation:       item.LastOperation,
		Execution:           cloneExecutionContract(item.Execution),
		Optional:            item.Optional,
		ResourceClaims:      append([]string(nil), item.ResourceClaims...),
		Resources:           append([]ResourceClaim(nil), item.Resources...),
		DecisionProfile:     item.DecisionProfile,
		DecisionOptions:     append([]DecisionOption(nil), item.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(item.DecisionAssumptions),
		DecisionFacts:       cloneDecisionFacts(item.DecisionFacts),
		DecisionArtifacts:   append([]ArtifactRef(nil), item.DecisionArtifacts...),
		DecisionBaseRates:   cloneBaseRateEvidence(item.DecisionBaseRates),
		DecisionProvenance:  cloneEvidenceProvenance(item.DecisionProvenance),
		MemoryManifests:     memoryManifests,
		ContextManifests:    contextManifests,
		SubagentProvider:    item.SubagentProvider,
		ProviderBinding:     cloneProviderBinding(item.ProviderBinding),
		BackendBinding:      cloneBackendBinding(item.BackendBinding),
	}
}

// TryApplyProjectedItem installs one task projection atomically and fires the
// checkpoint callback only after the projection is installed. The event-first
// coordinator boundaries use this for reset projections whose lifecycle state
// is intentionally outside CanTransition (retry and same-occurrence resume).
func (tl *TodoList) TryApplyProjectedItem(projected *TodoItem) error {
	if projected == nil || projected.ID == "" {
		return fmt.Errorf("projected task is invalid")
	}
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item != nil && item.ID == projected.ID {
			// The projection is copied into the live item rather than replacing
			// the slot. Callers across the coordinator and its tests hold
			// *TodoItem aliases obtained at creation; swapping the pointer would
			// silently detach every one of them from the lifecycle.
			*item = *cloneTodoItem(projected)
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", projected.ID)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// TrySetTypedResultForDispatch installs a result only when the Todo still owns
// the exact dispatch lease that produced it. The callback is intentionally
// invoked after the Todo mutex is released.
func (tl *TodoList) TrySetTypedResultForDispatch(id string, revision int, dispatchID string, result *TaskResult) error {
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item == nil || item.ID != id {
			continue
		}
		if item.OccurrenceRevision != revision || item.DispatchID != dispatchID {
			tl.mu.Unlock()
			return fmt.Errorf("task %s dispatch lease is stale", id)
		}
		item.TypedResult = cloneTaskResult(result)
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// TrySetDispatchLease installs a fresh worker lease. It is used by dispatch
// activation after the occurrence event and admission are durable. The typed
// result is cleared in the same critical section: a new lease owns no result
// yet, and leaving the prior attempt's result projected would let it be read
// back as if it belonged to the dispatch that replaced it.
func (tl *TodoList) TrySetDispatchLease(id string, revision int, dispatchID string) error {
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item != nil && item.ID == id {
			item.OccurrenceRevision = revision
			item.DispatchID = dispatchID
			item.TypedResult = nil
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// restoreTodoOccurrenceContract restores only the immutable execution
// contract from src. Lifecycle state (including plan approval, status,
// retries, receipts, verification, and output) remains owned by dst.
func restoreTodoOccurrenceContract(dst, src *TodoItem) {
	if dst == nil || src == nil {
		return
	}
	dst.Phase = src.Phase
	dst.Action = cloneActionPtr(src.Action)
	dst.PlanTaskID = src.PlanTaskID
	dst.ContractID = src.ContractID
	dst.ContractHash = src.ContractHash
	dst.ContractRevision = src.ContractRevision
	dst.Agent = src.Agent
	dst.Desc = src.Desc
	dst.Goal = src.Goal
	dst.Constraints = src.Constraints
	dst.Model = src.Model
	dst.ModelTopology = cloneModelTopology(src.ModelTopology)
	dst.ExecutionTarget = src.ExecutionTarget
	dst.ExecutionTopology = cloneExecutionTopology(src.ExecutionTopology)
	dst.Sidecar = src.Sidecar
	dst.Summarize = src.Summarize
	dst.OutputMode = src.OutputMode
	dst.ContextFiles = append([]string(nil), src.ContextFiles...)
	dst.Requires = append([]string(nil), src.Requires...)
	dst.Source = src.Source
	dst.ParentID = src.ParentID
	dst.DependsOn = append([]string(nil), src.DependsOn...)
	dst.Verify = src.Verify
	dst.VerifyMode = src.VerifyMode
	dst.VerifySpec = cloneVerificationSpecPtr(src.VerifySpec)
	dst.WorksetBinding = cloneWorksetBinding(src.WorksetBinding)
	dst.WorksetReceipt = cloneWorksetReceipt(src.WorksetReceipt)
	dst.MaxRetries = src.MaxRetries
	dst.OnFailure = src.OnFailure
	dst.Escalate = src.Escalate
	dst.AdversarialVerify = src.AdversarialVerify
	dst.SideEffect = src.SideEffect
	dst.Recovery = src.Recovery
	dst.ReconcileTool = src.ReconcileTool
	dst.Kind = src.Kind
	dst.Advances = append([]string(nil), src.Advances...)
	dst.ExpectedStateChange = src.ExpectedStateChange
	dst.Execution = cloneExecutionContract(src.Execution)
	dst.Optional = src.Optional
	dst.ResourceClaims = append([]string(nil), src.ResourceClaims...)
	dst.Resources = append([]ResourceClaim(nil), src.Resources...)
	dst.RecoveryHypothesis = cloneRecoveryHypothesis(src.RecoveryHypothesis)
	dst.DecisionProfile = src.DecisionProfile
	dst.DecisionOptions = append([]DecisionOption(nil), src.DecisionOptions...)
	dst.DecisionAssumptions = cloneDecisionAssumptions(src.DecisionAssumptions)
	dst.DecisionFacts = cloneDecisionFacts(src.DecisionFacts)
	dst.DecisionArtifacts = append([]ArtifactRef(nil), src.DecisionArtifacts...)
	dst.DecisionBaseRates = cloneBaseRateEvidence(src.DecisionBaseRates)
	dst.DecisionProvenance = cloneEvidenceProvenance(src.DecisionProvenance)
}

func (tl *TodoList) SetContextManifest(id string, manifest *ContextInjectionManifest) error {
	if manifest == nil {
		return errors.New("context manifest is nil")
	}
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item.ID != id {
			continue
		}
		copyManifest := cloneContextInjectionManifest(manifest)
		replaced := false
		for i := range item.ContextManifests {
			if sameContextManifestIdentity(item.ContextManifests[i], *manifest) {
				item.ContextManifests[i] = *copyManifest
				replaced = true
				break
			}
		}
		if !replaced {
			item.ContextManifests = append(item.ContextManifests, *copyManifest)
		}
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func (tl *TodoList) SetMemoryManifest(id string, manifest *MemoryInjectionManifest) error {
	if manifest == nil {
		return errors.New("memory manifest is nil")
	}
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item.ID != id {
			continue
		}
		copyManifest := cloneMemoryInjectionManifest(manifest)
		replaced := false
		for i := range item.MemoryManifests {
			if item.MemoryManifests[i].RunID == manifest.RunID && item.MemoryManifests[i].Attempt == manifest.Attempt {
				item.MemoryManifests[i] = *copyManifest
				replaced = true
				break
			}
		}
		if !replaced {
			item.MemoryManifests = append(item.MemoryManifests, *copyManifest)
		}
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// SetProgress persists both the orthogonal task progress and the exact
// criteria that advanced. Keeping this on the canonical TodoItem is required
// for session checkpoints; callers often operate on Items() clones.
func (tl *TodoList) SetProgress(id string, progress TaskProgress, criteria []string) error {
	tl.mu.Lock()
	updated := false
	for _, item := range tl.items {
		if item.ID != id {
			continue
		}
		item.Progress = progress
		item.ProgressCriteria = append([]string(nil), criteria...)
		updated = true
		break
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func cloneRecoveryHypothesis(src *RecoveryHypothesis) *RecoveryHypothesis {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Evidence = append([]EvidenceRef(nil), src.Evidence...)
	return &clone
}

func (tl *TodoList) SetExecutionReceipt(id string, receipt *ExecutionReceipt) error {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			if receipt == nil {
				ti.ExecutionReceipt = nil
			} else {
				copyR := cloneExecutionReceipt(receipt)
				ti.ExecutionReceipt = &copyR

				// A repair updates the durable record for the same execution
				// attempt. Keep one receipt per attempt rather than appending a
				// second receipt when provenance is completed after repair.
				replaced := false
				for i := range ti.ExecutionReceipts {
					existing := &ti.ExecutionReceipts[i]
					if existing.RunID == copyR.RunID && existing.TaskID == copyR.TaskID && existing.Attempt == copyR.Attempt && existing.ModelExecutionID == copyR.ModelExecutionID {
						ti.ExecutionReceipts[i] = copyR
						replaced = true
						break
					}
				}
				if !replaced {
					ti.ExecutionReceipts = append(ti.ExecutionReceipts, copyR)
				}
			}
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

// UpdateReceiptVerifyResult attaches a verification result to the
// ExecutionReceipt matching (runID, taskID, attempt), retaining the
// verification evidence per-attempt for forensics after the todo-wide
// VerifyResult slot is cleared (§5, §9 evidence retention). The receipt is
// updated in the durable ExecutionReceipts history slice and the single
// ExecutionReceipt field. A missing receipt for that (runID, attempt) is a
// no-op. RunID is required: after crash-resume a todo can carry receipts from
// a prior run with the same attempt number, so matching on attempt alone
// would misattribute evidence to the wrong run.
func (tl *TodoList) UpdateReceiptVerifyResult(runID, taskID string, attempt int, vr *VerificationResult) {
	if tl == nil || taskID == "" || attempt < 1 || vr == nil {
		return
	}
	tl.mu.Lock()
	for _, ti := range tl.items {
		if ti.ID != taskID {
			continue
		}
		copyVR := *vr
		copyVR.Spec = cloneVerificationSpecPtr(vr.Spec)
		// Update the durable per-attempt history. Match on (RunID, Attempt)
		// so a crash-resumed run with a fresh executionRunID does not overwrite
		// a prior run's receipt that happens to share the attempt number.
		for i := range ti.ExecutionReceipts {
			r := &ti.ExecutionReceipts[i]
			if r.Attempt == attempt && r.RunID == runID {
				r.VerifyResult = &copyVR
				break
			}
		}
		// Update the single receipt field if it matches (RunID, attempt).
		if ti.ExecutionReceipt != nil && ti.ExecutionReceipt.Attempt == attempt && ti.ExecutionReceipt.RunID == runID {
			ti.ExecutionReceipt.VerifyResult = &copyVR
		}
		break
	}
	tl.mu.Unlock()
}

func cloneExecutionReceipt(receipt *ExecutionReceipt) ExecutionReceipt {
	copyR := *receipt
	if receipt.ExitCode != nil {
		exitCode := *receipt.ExitCode
		copyR.ExitCode = &exitCode
	}
	copyR.ArtifactScope = cloneArtifactAccessScope(receipt.ArtifactScope)
	copyR.MemoryManifest = cloneMemoryInjectionManifest(receipt.MemoryManifest)
	copyR.ContextManifest = cloneContextInjectionManifest(receipt.ContextManifest)
	copyR.ToolDispositions = append([]ToolExecutionDisposition(nil), receipt.ToolDispositions...)
	if receipt.StepBudget != nil {
		stepBudget := *receipt.StepBudget
		copyR.StepBudget = &stepBudget
	}
	if receipt.SubmittedResult != nil {
		copyR.SubmittedResult = cloneTaskResult(receipt.SubmittedResult)
	}
	if receipt.RepairProvenance != nil {
		copyRP := *receipt.RepairProvenance
		if receipt.RepairProvenance.SubmittedResult != nil {
			copyRP.SubmittedResult = cloneTaskResult(receipt.RepairProvenance.SubmittedResult)
		}
		if receipt.RepairProvenance.History != nil {
			copyRP.History = make([]RepairAttemptProvenance, len(receipt.RepairProvenance.History))
			for i, attempt := range receipt.RepairProvenance.History {
				copyRP.History[i] = attempt
				copyRP.History[i].SubmittedResult = cloneTaskResult(attempt.SubmittedResult)
			}
		}
		copyR.RepairProvenance = &copyRP
	}
	if receipt.VerifyResult != nil {
		copyR.VerifyResult = cloneVerificationResult(receipt.VerifyResult)
	}
	return copyR
}

func (tl *TodoList) SetTaskResolution(id string, resolution *TaskResolution) error {
	tl.mu.Lock()
	updated := false
	var valErr error
	for _, ti := range tl.items {
		if ti.ID == id {
			if resolution != nil {
				if err := ValidateResolution(resolution, id, tl.items, tl.runID); err != nil {
					valErr = err
					break
				}
				copyRes := *resolution
				ti.Resolution = &copyRes
			} else {
				ti.Resolution = nil
			}
			updated = true
			break
		}
	}
	onChange := tl.onChange
	tl.mu.Unlock()

	if valErr != nil {
		return valErr
	}
	if !updated {
		return fmt.Errorf("task %s not found", id)
	}
	if onChange != nil {
		onChange()
	}
	return nil
}

func (tl *TodoList) Items() []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	result := make([]*TodoItem, len(tl.items))
	for i, item := range tl.items {
		result[i] = cloneTodoItem(item)
	}
	return result
}

// Has reports whether id identifies a live todo item. Callers that annotate
// task metadata may receive synthetic or already-completed context IDs (for
// example the coordinator stream); checking first keeps those annotations
// from producing misleading "item not found" warnings.
func (tl *TodoList) Has(id string) bool {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, item := range tl.items {
		if item != nil && item.ID == id {
			return true
		}
	}
	return false
}

// ExecutionMetadata returns the privacy-safe task metadata used in durable
// execution telemetry. It intentionally excludes the task description, task
// output, and verification result.
func (tl *TodoList) ExecutionMetadata(id string) (source string, skills []string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, item := range tl.items {
		if item.ID != id {
			continue
		}
		seen := make(map[string]struct{})
		for _, values := range [][]string{item.Skills, item.InjectedSkills, item.LoadedSkills} {
			for _, skill := range values {
				if skill == "" {
					continue
				}
				if _, ok := seen[skill]; ok {
					continue
				}
				seen[skill] = struct{}{}
				skills = append(skills, skill)
			}
		}
		return item.Source, skills
	}
	return "", nil
}

func (tl *TodoList) SetSkills(id string, skills []string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.Skills = skills
			return
		}
	}
}

func (tl *TodoList) SetInjectedSkills(id string, skills []string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.InjectedSkills = skills
			return
		}
	}
	log.Printf("[WARN] SetInjectedSkills: todo item %q not found", id)
}

func (tl *TodoList) AddLoadedSkill(id string, skillName string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			for _, s := range ti.LoadedSkills {
				if s == skillName {
					return
				}
			}
			ti.LoadedSkills = append(ti.LoadedSkills, skillName)
			return
		}
	}
	log.Printf("[WARN] AddLoadedSkill: todo item %q not found", id)
}

func (tl *TodoList) Children(parentID string) []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var result []*TodoItem
	for _, item := range tl.items {
		if item.ParentID == parentID {
			// Children is another Todo projection boundary. Reuse the canonical
			// clone so new contract fields cannot be silently omitted here.
			result = append(result, cloneTodoItem(item))
		}
	}
	return result
}

func (tl *TodoList) Clear() {
	tl.mu.Lock()
	tl.items = nil
	tl.next = 0
	onChange := tl.onChange
	tl.mu.Unlock()
	if onChange != nil {
		onChange()
	}
}

func (tl *TodoList) UpdateTodoTiming(id string, modelTime, toolTime time.Duration) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.ModelTime = modelTime
			ti.ToolTime = toolTime
			return
		}
	}
}

func (tl *TodoList) CompletedCount() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	count := 0
	for _, ti := range tl.items {
		if ti.Status == TaskDone {
			count++
		}
	}
	return count
}

func (tl *TodoList) ErrorCount() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	count := 0
	for _, ti := range tl.items {
		if ti.Status == TaskError || ti.Status == TaskBlocked {
			count++
		}
	}
	return count
}
