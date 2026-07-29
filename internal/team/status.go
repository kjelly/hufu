package team

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// ErrTasksUnresolved marks a completed coordinator response that still has
// failed or blocked tasks. Callers must not report this as a successful run.
var ErrTasksUnresolved = errors.New("tasks unresolved")

type StatusEvent struct {
	Type        string // "start", "step", "tool_call", "tool_result", "done", "error", "text", "todos_updated", "skill_used", "loop_warning", "timing", "judge", "skeptic", "budget_exceeded", "task_timeout"
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

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskVerifying  TaskStatus = "verifying"
	TaskDone       TaskStatus = "done"
	TaskError      TaskStatus = "error"
	TaskBlocked    TaskStatus = "blocked"
	TaskSkipped    TaskStatus = "skipped"
	TaskPlanned    TaskStatus = "planned"
	TaskPaused     TaskStatus = "paused"
)

// VerificationResult is the durable evidence produced by a task's objective
// verification command. Output is intentionally bounded by the verifier.
type VerificationResult struct {
	Command  string
	WorkDir  string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
}

// CanTransition reports whether a normal lifecycle update may move a task
// between the supplied states. DAG retries use ResetForRetry explicitly.
func CanTransition(from, to TaskStatus) bool {
	if from == to {
		return true
	}
	switch from {
	case TaskPending:
		return to == TaskPlanned || to == TaskInProgress || to == TaskDone || to == TaskSkipped || to == TaskBlocked || to == TaskError
	case TaskPlanned:
		return to == TaskInProgress || to == TaskSkipped || to == TaskBlocked || to == TaskError
	case TaskInProgress:
		return to == TaskPlanned || to == TaskPaused || to == TaskVerifying || to == TaskDone || to == TaskBlocked || to == TaskError || to == TaskSkipped
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
	ID             string
	Agent          string
	Desc           string
	Status         TaskStatus
	Detail         string
	Output         string // Full task output
	Model          string
	Skills         []string
	InjectedSkills []string
	LoadedSkills   []string
	StartedAt      time.Time
	EndedAt        time.Time
	ModelTime      time.Duration
	ToolTime       time.Duration
	Source         string
	ParentID       string
	DependsOn      []string // IDs of tasks that must complete before this one starts
	Verify         string   // Command to run to verify the task
	VerifyMode     string   // success, expected_failure, or observation
	VerifyResult   *VerificationResult
	MaxRetries     int             // Maximum number of retries for this task
	Retries        int             // Current number of retries
	OnFailure      string          // ID of the task to jump back to if this task fails (creates a loop)
	SideEffect     SideEffectClass `json:"side_effect,omitempty"`
	Recovery       RecoveryPolicy  `json:"recovery,omitempty"`
	ReconcileTool  string          `json:"reconcile_tool,omitempty"`
	RecoveryState  string          `json:"recovery_state,omitempty"`
	TypedResult    *TaskResult     `json:"typed_result,omitempty"`
	Resolution     *TaskResolution `json:"resolution,omitempty"`
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
	Agent         string
	Desc          string
	Model         string
	Source        string
	ParentID      string
	Verify        string
	VerifyMode    string
	MaxRetries    int
	OnFailure     string
	SideEffect    SideEffectClass
	Recovery      RecoveryPolicy
	ReconcileTool string
}

func (tl *TodoList) AddBatch(items []TodoSpec) []*TodoItem {
	tl.mu.Lock()
	var added []*TodoItem
	for _, item := range items {
		tl.next++
		ti := &TodoItem{
			ID:            fmt.Sprintf("%d", tl.next),
			Agent:         item.Agent,
			Desc:          item.Desc,
			Model:         item.Model,
			Status:        TaskPending,
			Source:        item.Source,
			ParentID:      item.ParentID,
			Verify:        item.Verify,
			VerifyMode:    item.VerifyMode,
			MaxRetries:    item.MaxRetries,
			OnFailure:     item.OnFailure,
			SideEffect:    item.SideEffect,
			Recovery:      item.Recovery,
			ReconcileTool: item.ReconcileTool,
		}
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
	defer tl.mu.Unlock()
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
	var dependsOn []string
	if len(item.DependsOn) > 0 {
		dependsOn = make([]string, len(item.DependsOn))
		copy(dependsOn, item.DependsOn)
	}
	var verifyResult *VerificationResult
	if item.VerifyResult != nil {
		copyResult := *item.VerifyResult
		verifyResult = &copyResult
	}
	var typedResult *TaskResult
	if item.TypedResult != nil {
		copyTR := *item.TypedResult
		typedResult = &copyTR
	}
	var resolution *TaskResolution
	if item.Resolution != nil {
		copyRes := *item.Resolution
		resolution = &copyRes
	}
	return &TodoItem{
		ID:             item.ID,
		Agent:          item.Agent,
		Desc:           item.Desc,
		Status:         item.Status,
		Detail:         item.Detail,
		Output:         item.Output,
		Model:          item.Model,
		Skills:         skills,
		InjectedSkills: injectedSkills,
		LoadedSkills:   loadedSkills,
		StartedAt:      item.StartedAt,
		EndedAt:        item.EndedAt,
		ModelTime:      item.ModelTime,
		ToolTime:       item.ToolTime,
		Source:         item.Source,
		ParentID:       item.ParentID,
		DependsOn:      dependsOn,
		Verify:         item.Verify,
		VerifyMode:     item.VerifyMode,
		VerifyResult:   verifyResult,
		MaxRetries:     item.MaxRetries,
		Retries:        item.Retries,
		OnFailure:      item.OnFailure,
		SideEffect:     item.SideEffect,
		Recovery:       item.Recovery,
		ReconcileTool:  item.ReconcileTool,
		RecoveryState:  item.RecoveryState,
		TypedResult:    typedResult,
		Resolution:     resolution,
	}
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
				ID:             item.ID,
				Agent:          item.Agent,
				Desc:           item.Desc,
				Status:         item.Status,
				Detail:         item.Detail,
				Model:          item.Model,
				Skills:         skills,
				InjectedSkills: injectedSkills,
				LoadedSkills:   loadedSkills,
				StartedAt:      item.StartedAt,
				EndedAt:        item.EndedAt,
				ModelTime:      item.ModelTime,
				ToolTime:       item.ToolTime,
				Source:         item.Source,
				ParentID:       item.ParentID,
				DependsOn:      dependsOn,
				Verify:         item.Verify,
				VerifyMode:     item.VerifyMode,
				MaxRetries:     item.MaxRetries,
				Retries:        item.Retries,
				OnFailure:      item.OnFailure,
				SideEffect:     item.SideEffect,
				Recovery:       item.Recovery,
				ReconcileTool:  item.ReconcileTool,
				RecoveryState:  item.RecoveryState,
			})
		}
	}
	return result
}

func (tl *TodoList) Clear() {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.items = nil
	tl.next = 0
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
