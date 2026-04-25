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