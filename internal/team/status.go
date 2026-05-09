package team

import (
	"fmt"
	"sync"
	"time"
)

type StatusEvent struct {
	Type       string // "start", "step", "tool_call", "tool_result", "done", "error", "text", "todos_updated", "skill_used", "loop_warning", "timing"
	TeamName   string
	Agent      string
	Message    string
	ToolName   string
	ToolArgs   string
	ToolResult string
	Step       int
	Todos      []*TodoItem
	SkillName  string
	Model      string
	Duration   time.Duration
	ModelTime  time.Duration
	ToolTime   time.Duration
	TodoID     string // ID of the TodoItem this event belongs to (set for worker-task events)
	Output     string // Final output text (set in done events for task-level events)
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
	Model          string
	Skills         []string
	InjectedSkills []string
	StartedAt      time.Time
	EndedAt        time.Time
	ModelTime      time.Duration
	ToolTime       time.Duration
	Source         string
	ParentID       string
}

type TodoList struct {
	mu    sync.Mutex
	items []*TodoItem
	next  int
}

func (tl *TodoList) AddBatch(items []struct {
	Agent    string
	Desc     string
	Model    string
	Source   string
	ParentID string
}) []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var added []*TodoItem
	for _, item := range items {
		tl.next++
		ti := &TodoItem{
			ID:       fmt.Sprintf("%d", tl.next),
			Agent:    item.Agent,
			Desc:     item.Desc,
			Model:    item.Model,
			Status:   TaskPending,
			Source:   item.Source,
			ParentID: item.ParentID,
		}
		tl.items = append(tl.items, ti)
		added = append(added, ti)
	}
	return added
}

func (tl *TodoList) UpdateStatus(id string, status TaskStatus, detail string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for _, ti := range tl.items {
		if ti.ID == id {
			ti.Status = status
			if detail != "" {
				ti.Detail = detail
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
			return
		}
	}
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
		result[i] = &TodoItem{
			ID:        item.ID,
			Agent:     item.Agent,
			Desc:      item.Desc,
			Status:    item.Status,
			Detail:    item.Detail,
			Model:     item.Model,
			Skills:    skills,
			StartedAt: item.StartedAt,
			EndedAt:   item.EndedAt,
			ModelTime: item.ModelTime,
			ToolTime:  item.ToolTime,
			Source:    item.Source,
			ParentID:  item.ParentID,
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
	// Debug log when ID is not found
	fmt.Printf("[DEBUG] SetInjectedSkills: todo item %q not found\n", id)
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
			result = append(result, &TodoItem{
				ID:        item.ID,
				Agent:     item.Agent,
				Desc:      item.Desc,
				Status:    item.Status,
				Detail:    item.Detail,
				Model:     item.Model,
				Skills:    skills,
				StartedAt: item.StartedAt,
				EndedAt:   item.EndedAt,
				ModelTime: item.ModelTime,
				ToolTime:  item.ToolTime,
				Source:    item.Source,
				ParentID:  item.ParentID,
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
