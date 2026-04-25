package team

type StatusEvent struct {
	Type       string // "start", "step", "tool_call", "tool_result", "done", "error", "text"
	Agent     string
	Message   string
	ToolName  string
	ToolArgs  string
	ToolResult string
	Step      int
}

type StatusReporter func(event StatusEvent)

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskInProgress TaskStatus = "in_progress"
	TaskDone       TaskStatus = "done"
	TaskError      TaskStatus = "error"
)

type TaskInfo struct {
	Agent   string
	Task   string
	Status TaskStatus
	Detail string
}

type TaskTracker struct {
	tasks []*TaskInfo
}

func NewTaskTracker() *TaskTracker {
	return &TaskTracker{}
}

func (t *TaskTracker) Start(agent, task string) {
	for _, ti := range t.tasks {
		if ti.Agent == agent {
			ti.Status = TaskInProgress
			ti.Task = task
			return
		}
	}
	t.tasks = append(t.tasks, &TaskInfo{
		Agent:   agent,
		Task:    task,
		Status:  TaskInProgress,
	})
}

func (t *TaskTracker) Done(agent string) {
	for _, ti := range t.tasks {
		if ti.Agent == agent {
			ti.Status = TaskDone
			return
		}
	}
}

func (t *TaskTracker) Error(agent, detail string) {
	for _, ti := range t.tasks {
		if ti.Agent == agent {
			ti.Status = TaskError
			ti.Detail = detail
			return
		}
	}
}

func (t *TaskTracker) Tasks() []*TaskInfo {
	result := make([]*TaskInfo, len(t.tasks))
	copy(result, t.tasks)
	return result
}