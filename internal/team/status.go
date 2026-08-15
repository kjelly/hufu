package team

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ErrTasksUnresolved marks a completed coordinator response that still has
// failed or blocked tasks. Callers must not report this as a successful run.
var ErrTasksUnresolved = errors.New("tasks unresolved")

type StatusEvent struct {
	Type        string // "start", "step", "tool_call", "tool_result", "done", "error", "text", "todos_updated", "skill_used", "loop_warning", "timing", "judge", "skeptic", "memory_learning", "budget_exceeded", "task_timeout"
	TeamName    string
	Agent       string
	Message     string
	ToolName    string
	ToolArgs    string
	ToolResult  string
	Step        int
	Todos       []*TodoItem
	SkillName   string
	Model       string
	Duration    time.Duration
	ModelTime   time.Duration
	ToolTime    time.Duration
	TodoID      string // ID of the TodoItem this event belongs to (set for worker-task events)
	Output      string // Final output text (set in done events for task-level events)
	SSHSessions int
	Data        map[string]any
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
	ContractID       string  `json:"contract_id,omitempty"`
	ContractHash     string  `json:"contract_hash,omitempty"`
	ContractRevision int     `json:"contract_revision,omitempty"`
	Agent            string
	Desc             string
	Status           TaskStatus
	Detail           string
	Output           string // Full task output
	Model            string
	Skills           []string
	InjectedSkills   []string
	LoadedSkills     []string
	StartedAt        time.Time
	EndedAt          time.Time
	ModelTime        time.Duration
	ToolTime         time.Duration
	Source           string
	ParentID         string
	DependsOn        []string          // IDs of tasks that must complete before this one starts
	Verify           string            // Command to run to verify the task
	VerifyMode       string            // success, expected_failure, or observation
	VerifySpec       *VerificationSpec `json:"verify_spec,omitempty"`
	VerifyResult     *VerificationResult
	// RuntimeError preserves a structured runtime/provider failure so phase
	// aggregation does not degrade it into an unclassified worker error.
	RuntimeError        *ExecutionError            `json:"runtime_error,omitempty"`
	ExecutionReceipt    *ExecutionReceipt          `json:"execution_receipt,omitempty"`
	ExecutionReceipts   []ExecutionReceipt         `json:"execution_receipts,omitempty"`
	FailureEvent        *FailureEventPayload       `json:"failure_event,omitempty"`
	MaxRetries          int                        // Maximum number of retries for this task
	Retries             int                        // Current number of retries
	OnFailure           string                     // ID of the task to jump back to if this task fails (creates a loop)
	SideEffect          SideEffectClass            `json:"side_effect,omitempty"`
	Recovery            RecoveryPolicy             `json:"recovery,omitempty"`
	ReconcileTool       string                     `json:"reconcile_tool,omitempty"`
	RecoveryState       string                     `json:"recovery_state,omitempty"`
	TypedResult         *TaskResult                `json:"typed_result,omitempty"`
	Resolution          *TaskResolution            `json:"resolution,omitempty"`
	Kind                TaskKind                   `json:"kind,omitempty"`
	Advances            []string                   `json:"advances,omitempty"`
	ExpectedStateChange string                     `json:"expected_state_change,omitempty"`
	Progress            TaskProgress               `json:"progress,omitempty"`
	ProgressCriteria    []string                   `json:"progress_criteria,omitempty"`
	FailureFingerprints []FailureFingerprint       `json:"failure_fingerprints,omitempty"`
	RecoveryHypothesis  *RecoveryHypothesis        `json:"recovery_hypothesis,omitempty"`
	DiagnosticHints     []string                   `json:"diagnostic_hints,omitempty"`
	LastOperation       string                     `json:"last_operation,omitempty"`
	Execution           ExecutionContract          `json:"execution,omitempty"`
	MemoryManifests     []MemoryInjectionManifest  `json:"memory_manifests,omitempty"`
	ContextManifests    []ContextInjectionManifest `json:"context_manifests,omitempty"`
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
	PlanTaskID          string
	Phase               Phase
	Action              *Action
	ContractID          string
	ContractHash        string
	ContractRevision    int
	Agent               string
	Desc                string
	Model               string
	Source              string
	ParentID            string
	Verify              string
	VerifyMode          string
	VerifySpec          *VerificationSpec
	MaxRetries          int
	OnFailure           string
	DependsOn           []string
	SideEffect          SideEffectClass
	Recovery            RecoveryPolicy
	ReconcileTool       string
	Kind                TaskKind
	Advances            []string
	ExpectedStateChange string
	RecoveryHypothesis  *RecoveryHypothesis
	Execution           ExecutionContract
}

// todoItemFromSpec builds a pending TodoItem from a spec and an explicit ID.
// It is shared by AddBatch and the event-first CommitTaskCreation boundary so
// both paths produce byte-identical projection state.
func todoItemFromSpec(item TodoSpec, id string) *TodoItem {
	return &TodoItem{
		ID:                  id,
		PlanTaskID:          item.PlanTaskID,
		Phase:               item.Phase,
		Action:              cloneActionPtr(item.Action),
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               item.Agent,
		Desc:                item.Desc,
		Model:               item.Model,
		Status:              TaskPending,
		Source:              item.Source,
		ParentID:            item.ParentID,
		Verify:              item.Verify,
		VerifyMode:          item.VerifyMode,
		VerifySpec:          item.VerifySpec,
		MaxRetries:          item.MaxRetries,
		OnFailure:           item.OnFailure,
		DependsOn:           append([]string(nil), item.DependsOn...),
		SideEffect:          item.SideEffect,
		Recovery:            item.Recovery,
		ReconcileTool:       item.ReconcileTool,
		Kind:                item.Kind,
		Advances:            append([]string(nil), item.Advances...),
		ExpectedStateChange: item.ExpectedStateChange,
		Progress:            ProgressUnknown,
		RecoveryHypothesis:  item.RecoveryHypothesis,
		Execution:           item.Execution,
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
			copyResult := *result
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
			ti.TypedResult = &copyResult
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
		copyResult := *item.VerifyResult
		copyResult.Spec = cloneVerificationSpecPtr(item.VerifyResult.Spec)
		verifyResult = &copyResult
	}
	var runtimeErr *ExecutionError
	if item.RuntimeError != nil {
		copyErr := *item.RuntimeError
		copyErr.Evidence = append([]string(nil), item.RuntimeError.Evidence...)
		runtimeErr = &copyErr
	}
	var typedResult *TaskResult
	if item.TypedResult != nil {
		copyTR := *item.TypedResult
		typedResult = &copyTR
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
		ContractID:          item.ContractID,
		ContractHash:        item.ContractHash,
		ContractRevision:    item.ContractRevision,
		Agent:               item.Agent,
		Desc:                item.Desc,
		Status:              item.Status,
		Detail:              item.Detail,
		Output:              item.Output,
		Model:               item.Model,
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
		VerifyResult:        verifyResult,
		RuntimeError:        runtimeErr,
		ExecutionReceipt:    execReceipt,
		ExecutionReceipts:   execReceipts,
		FailureEvent:        failureEvent,
		MaxRetries:          item.MaxRetries,
		Retries:             item.Retries,
		OnFailure:           item.OnFailure,
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
		Execution:           item.Execution,
		MemoryManifests:     memoryManifests,
		ContextManifests:    contextManifests,
	}
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
	copyR.MemoryManifest = cloneMemoryInjectionManifest(receipt.MemoryManifest)
	copyR.ContextManifest = cloneContextInjectionManifest(receipt.ContextManifest)
	if receipt.RepairProvenance != nil {
		copyRP := *receipt.RepairProvenance
		if receipt.RepairProvenance.SubmittedResult != nil {
			copyResult := *receipt.RepairProvenance.SubmittedResult
			copyRP.SubmittedResult = &copyResult
		}
		copyR.RepairProvenance = &copyRP
	}
	if receipt.VerifyResult != nil {
		copyVR := *receipt.VerifyResult
		copyVR.Spec = cloneVerificationSpecPtr(receipt.VerifyResult.Spec)
		copyR.VerifyResult = &copyVR
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
			var dependsOn []string
			if len(item.DependsOn) > 0 {
				dependsOn = make([]string, len(item.DependsOn))
				copy(dependsOn, item.DependsOn)
			}
			result = append(result, &TodoItem{
				ID:                  item.ID,
				Agent:               item.Agent,
				Desc:                item.Desc,
				Status:              item.Status,
				Detail:              item.Detail,
				Model:               item.Model,
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
				MaxRetries:          item.MaxRetries,
				Retries:             item.Retries,
				OnFailure:           item.OnFailure,
				SideEffect:          item.SideEffect,
				Recovery:            item.Recovery,
				ReconcileTool:       item.ReconcileTool,
				RecoveryState:       item.RecoveryState,
				Kind:                item.Kind,
				Advances:            append([]string(nil), item.Advances...),
				ExpectedStateChange: item.ExpectedStateChange,
				Progress:            item.Progress,
				ProgressCriteria:    append([]string(nil), item.ProgressCriteria...),
				FailureFingerprints: append([]FailureFingerprint(nil), item.FailureFingerprints...),
				RecoveryHypothesis:  cloneRecoveryHypothesis(item.RecoveryHypothesis),
				LastOperation:       item.LastOperation,
				Execution:           item.Execution,
			})
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
