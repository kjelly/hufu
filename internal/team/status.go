package team

import (
	"fmt"
	"log"
	"sync"
	"time"
)

type StatusEvent struct {
	Type        string // "start", "step", "tool_call", "tool_result", "done", "error", "text", "todos_updated", "skill_used", "loop_warning", "timing", "judge", "skeptic"
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
	TaskDone       TaskStatus = "done"
	TaskError      TaskStatus = "error"
	TaskSkipped    TaskStatus = "skipped"
	TaskPlanned    TaskStatus = "planned"
	TaskPaused     TaskStatus = "paused"
)

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
	MaxRetries     int      // Maximum number of retries for this task
	Retries        int      // Current number of retries
	OnFailure      string   // ID of the task to jump back to if this task fails (creates a loop)
}

type TodoList struct {
	mu       sync.Mutex
	items    []*TodoItem
	next     int
	onChange func()
}

// TodoSpec describes a todo item to be created via AddBatch.
type TodoSpec struct {
	Agent      string
	Desc       string
	Model      string
	Source     string
	ParentID   string
	Verify     string
	MaxRetries int
	OnFailure  string
}

func (tl *TodoList) AddBatch(items []TodoSpec) []*TodoItem {
	tl.mu.Lock()
	var added []*TodoItem
	for _, item := range items {
		tl.next++
		ti := &TodoItem{
			ID:         fmt.Sprintf("%d", tl.next),
			Agent:      item.Agent,
			Desc:       item.Desc,
			Model:      item.Model,
			Status:     TaskPending,
			Source:     item.Source,
			ParentID:   item.ParentID,
			Verify:     item.Verify,
			MaxRetries: item.MaxRetries,
			OnFailure:  item.OnFailure,
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

func (tl *TodoList) UpdateStatus(id string, status TaskStatus, detail string) {
	tl.UpdateStatusAndOutput(id, status, detail, "")
}

func (tl *TodoList) UpdateStatusAndOutput(id string, status TaskStatus, detail string, output string) {
	tl.mu.Lock()
	updated := false
	for _, ti := range tl.items {
		if ti.ID == id {
			// TaskDone and TaskSkipped are terminal states.
			// TaskError can transition to TaskInProgress for retries.
			if ti.Status == TaskDone || ti.Status == TaskSkipped {
				if status != ti.Status {
					tl.mu.Unlock()
					return
				}
			}
			// TaskError can only transition to TaskInProgress (for retries)
			if ti.Status == TaskError && status != TaskInProgress && status != TaskError {
				tl.mu.Unlock()
				return
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
			case TaskDone, TaskError, TaskSkipped:
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
			ti.StartedAt = time.Time{}
			ti.EndedAt = time.Time{}
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

func (tl *TodoList) Items() []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	result := make([]*TodoItem, len(tl.items))
	for i, item := range tl.items {
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
		result[i] = &TodoItem{
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
			MaxRetries:     item.MaxRetries,
			Retries:        item.Retries,
			OnFailure:      item.OnFailure,
		}
	}
	return result
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
				MaxRetries:     item.MaxRetries,
				Retries:        item.Retries,
				OnFailure:      item.OnFailure,
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
		if ti.Status == TaskError {
			count++
		}
	}
	return count
}
