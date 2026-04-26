package team

import (
	"fmt"
	"sync"
)

type StatusEvent struct {
	Type       string // "start", "step", "tool_call", "tool_result", "done", "error", "text", "todos_updated"
	TeamName   string
	Agent      string
	Message    string
	ToolName   string
	ToolArgs   string
	ToolResult string
	Step       int
	Todos      []*TodoItem
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

type StatusReporter func(event StatusEvent)

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskError      TaskStatus = "error"
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
	ID     string
	Agent  string
	Desc   string
	Status TaskStatus
	Detail string
}

type TodoList struct {
	mu    sync.Mutex
	items []*TodoItem
	next  int
}

func (tl *TodoList) AddBatch(items []struct {
	Agent string
	Desc  string
}) []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	var added []*TodoItem
	for _, item := range items {
		tl.next++
		ti := &TodoItem{
			ID:     fmt.Sprintf("%d", tl.next),
			Agent:  item.Agent,
			Desc:   item.Desc,
			Status: TaskPending,
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
			return
		}
	}
}

func (tl *TodoList) Items() []*TodoItem {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	result := make([]*TodoItem, len(tl.items))
	copy(result, tl.items)
	return result
}

func (tl *TodoList) Clear() {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.items = nil
	tl.next = 0
}