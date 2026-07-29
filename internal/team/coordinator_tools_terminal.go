package team

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/tools"
)

type terminalTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal",
		Description: "Manage stateful terminal sessions for interactive wizards, deploys, long-running processes, or streamed output. Actions: start, write, read, resize, wait, close, list, reconcile. PTY sessions require the experimental PTY terminal feature.",
		Parameters: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action: start, write, read, resize, wait, close, list, reconcile",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Terminal session ID (required for write, read, close, reconcile)",
			},
			"command": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Command and arguments as array of strings (required for start)",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Optional working directory for start action",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Child timeout in seconds (start) or wait timeout in seconds (wait)",
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Wait target: exit or resource_released. artifact_verified is owned by ArtifactVerifier.",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Data to write to stdin (required for write action)",
			},
			"filter": map[string]any{
				"type":        "string",
				"description": "Optional list filter: unresolved (default), current, all",
			},
			"pty": map[string]any{
				"type":        "boolean",
				"description": "Start under a real PTY (experimental; requires --enable-pty-terminal)",
			},
			"rows": map[string]any{"type": "integer", "description": "PTY rows for start or resize"},
			"cols": map[string]any{"type": "integer", "description": "PTY columns for start or resize"},
		},
		Required: []string{"action"},
	}
}

// terminalArgs holds parsed arguments for the terminal tool.
type terminalArgs struct {
	Action     string   `json:"action"`
	ID         string   `json:"id"`
	Command    []string `json:"command"`
	WorkingDir string   `json:"working_dir"`
	Timeout    int      `json:"timeout"`
	Data       string   `json:"data"`
	Filter     string   `json:"filter"`
	PTY        bool     `json:"pty"`
	Rows       uint16   `json:"rows"`
	Cols       uint16   `json:"cols"`
	Target     string   `json:"target"`
}

func (t *terminalTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	args, err := t.parseArgs(call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	if err := t.checkPermission(ctx, args.Action); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	mgr := t.coordinator.TerminalManager()
	if mgr == nil {
		return fantasy.NewTextErrorResponse("terminal manager is not available"), nil
	}

	switch args.Action {
	case "start":
		return t.runStart(ctx, mgr, args)
	case "write":
		return t.runWrite(ctx, mgr, args)
	case "read":
		return t.runRead(ctx, mgr, args)
	case "resize":
		return t.runResize(ctx, mgr, args)
	case "wait":
		return t.runWait(ctx, mgr, args)
	case "close":
		return t.runClose(ctx, mgr, args)
	case "list":
		return t.runList(ctx, mgr, args)
	case "reconcile":
		return t.runReconcile(ctx, mgr, args)
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown action %q (valid: start, write, read, resize, wait, close, list, reconcile)", args.Action)), nil
	}
}

func (t *terminalTool) parseArgs(input string) (*terminalArgs, error) {
	var args terminalArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %v", err)
	}
	return &args, nil
}

func (t *terminalTool) checkPermission(ctx context.Context, action string) error {
	targetTool := "terminal"
	if action != "" {
		targetTool = "terminal_" + action
	}
	allowed, _, denyReason, err := tools.CheckToolPermissionDetail(ctx, targetTool)
	if err != nil {
		return fmt.Errorf("tool permission check failed: %v", err)
	}
	if !allowed {
		if mainAllowed, _, _, _ := tools.CheckToolPermissionDetail(ctx, "terminal"); !mainAllowed {
			return fmt.Errorf("tool '%s' is not permitted (%s)", targetTool, denyReason)
		}
	}
	return nil
}

func (t *terminalTool) getTaskID(ctx context.Context) string {
	if taskID := terminalTaskID(ctx); taskID != "" {
		return taskID
	}
	return t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })
}

func (t *terminalTool) runStart(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if len(args.Command) == 0 {
		return fantasy.NewTextErrorResponse("command is required for start action"), nil
	}
	taskID := t.getTaskID(ctx)
	if taskID == "" {
		return fantasy.NewTextErrorResponse("caller task identity context is required to start a terminal session"), nil
	}
	if args.PTY {
		if err := t.coordinator.SetPTYTerminalEnabled(true); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}

	workDir := args.WorkingDir
	if workDir == "" {
		workDir = t.coordinator.projectDir
	}
	if workDir != "" {
		if err := t.validateWorkDir(ctx, workDir); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}

	networkBlock := false
	if nb, ok := ctx.Value(tools.AgentNetworkBlockKey).(bool); ok && nb {
		networkBlock = true
	}

	var timeoutDur time.Duration
	if args.Timeout > 0 {
		timeoutDur = time.Duration(args.Timeout) * time.Second
	}

	agentName := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	if agentName == "" {
		agentName = "worker"
	}

	sess, err := mgr.Start(ctx, TerminalStartRequest{
		RunID:        t.coordinator.executionRunID,
		OwnerTaskID:  taskID,
		Agent:        agentName,
		Command:      args.Command,
		WorkingDir:   args.WorkingDir,
		ChildTimeout: timeoutDur,
		NetworkBlock: networkBlock,
		Mode: func() TerminalMode {
			if args.PTY {
				return TerminalModePTY
			}
			return TerminalModePipe
		}(),
		Rows: args.Rows,
		Cols: args.Cols,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if t.coordinator.session != nil {
		t.coordinator.report(t.coordinator.newEvent("terminal_started").withTodoID(taskID).withMessage(sess.ID))
	}
	out, _ := json.MarshalIndent(sess, "", "  ")
	return fantasy.NewTextResponse(string(out)), nil
}

func (t *terminalTool) validateWorkDir(ctx context.Context, workDir string) error {
	if allowedPaths, ok := ctx.Value(tools.AgentAllowedPathsKey).([]string); ok && len(allowedPaths) > 0 {
		if !tools.IsPathAllowed(workDir, allowedPaths) {
			return fmt.Errorf("working directory %q is outside allowed paths", workDir)
		}
	}
	if restrictedPath, ok := ctx.Value(tools.AgentRestrictedPathKey).(string); ok && restrictedPath != "" {
		if !tools.IsPathAllowed(workDir, []string{restrictedPath}) {
			return fmt.Errorf("working directory %q is restricted by path policy", workDir)
		}
	}
	return nil
}

func (t *terminalTool) runWrite(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for write action"), nil
	}
	err := mgr.Write(ctx, args.ID, TerminalInput{
		OwnerTaskID: t.getTaskID(ctx),
		Data:        []byte(args.Data),
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Wrote %d bytes to terminal session %s", len(args.Data), args.ID)), nil
}

func (t *terminalTool) runRead(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for read action"), nil
	}
	res, err := mgr.Read(ctx, args.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	respMap := map[string]any{
		"session": res.Session,
		"output":  string(res.Output),
		"eof":     res.EOF,
		"screen":  res.Screen,
	}
	out, _ := json.MarshalIndent(respMap, "", "  ")
	return fantasy.NewTextResponse(string(out)), nil
}

func (t *terminalTool) runResize(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for resize action"), nil
	}
	if err := mgr.Resize(ctx, args.ID, args.Rows, args.Cols); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Resized terminal session %s to %dx%d", args.ID, args.Rows, args.Cols)), nil
}

func (t *terminalTool) runWait(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for wait action"), nil
	}
	target := TerminalWaitTarget(args.Target)
	if target == "" {
		return fantasy.NewTextErrorResponse("target is required for wait action"), nil
	}
	if args.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(args.Timeout)*time.Second)
		defer cancel()
	}
	result, err := NewTerminalSessionWaiter(mgr).Wait(ctx, TerminalWaitRequest{
		SessionID: args.ID, Target: target, PollInterval: 25 * time.Millisecond,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return fantasy.NewTextResponse(string(out)), nil
}

func (t *terminalTool) runClose(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for close action"), nil
	}
	err := mgr.Close(ctx, args.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Closed terminal session %s", args.ID)), nil
}

func (t *terminalTool) runList(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	runID := t.coordinator.executionRunID
	filter := args.Filter
	if filter == "current" {
		sessions, err := mgr.List(ctx, runID)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		out, _ := json.MarshalIndent(sessions, "", "  ")
		return fantasy.NewTextResponse(string(out)), nil
	}
	allSessions, err := mgr.List(ctx, "")
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if filter == "all" {
		out, _ := json.MarshalIndent(allSessions, "", "  ")
		return fantasy.NewTextResponse(string(out)), nil
	}
	var res []TerminalSession
	for _, s := range allSessions {
		if s.RunID == runID || s.State == TerminalSessionRunning || s.State == TerminalSessionUnknown || s.Running {
			res = append(res, s)
		}
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	return fantasy.NewTextResponse(string(out)), nil
}

func (t *terminalTool) runReconcile(ctx context.Context, mgr TerminalManager, args *terminalArgs) (fantasy.ToolResponse, error) {
	if args.ID == "" {
		return fantasy.NewTextErrorResponse("id is required for reconcile action"), nil
	}
	sess, err := mgr.Reconcile(ctx, args.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	out, _ := json.MarshalIndent(sess, "", "  ")
	return fantasy.NewTextResponse(string(out)), nil
}

type terminalStartTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalStartTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_start",
		Description: "Start a stateful terminal process for long-running commands, wizards, or background services.",
		Parameters: map[string]any{
			"command": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Command and arguments as array of strings",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Optional working directory",
			},
			"timeout": map[string]any{
				"type":        "integer",
				"description": "Child timeout in seconds (optional)",
			},
			"pty":  map[string]any{"type": "boolean", "description": "Start under a real PTY (requires --enable-pty-terminal)"},
			"rows": map[string]any{"type": "integer", "description": "Initial PTY rows"},
			"cols": map[string]any{"type": "integer", "description": "Initial PTY columns"},
		},
		Required: []string{"command"},
	}
}

func (t *terminalStartTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Command    []string `json:"command"`
		WorkingDir string   `json:"working_dir"`
		Timeout    int      `json:"timeout"`
		PTY        bool     `json:"pty"`
		Rows       uint16   `json:"rows"`
		Cols       uint16   `json:"cols"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action":      "start",
		"command":     args.Command,
		"working_dir": args.WorkingDir,
		"timeout":     args.Timeout,
		"pty":         args.PTY,
		"rows":        args.Rows,
		"cols":        args.Cols,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

type terminalWriteTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalWriteTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_write",
		Description: "Write stdin input to a running stateful terminal session.",
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Terminal session ID",
			},
			"data": map[string]any{
				"type":        "string",
				"description": "Data string to write",
			},
		},
		Required: []string{"id", "data"},
	}
}

func (t *terminalWriteTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action": "write",
		"id":     args.ID,
		"data":   args.Data,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

type terminalReadTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalReadTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_read",
		Description: "Read output stream from a stateful terminal session.",
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Terminal session ID",
			},
		},
		Required: []string{"id"},
	}
}

func (t *terminalReadTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action": "read",
		"id":     args.ID,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

// terminalWaitTool exposes lifecycle-only waiting to agents. It deliberately
// cannot wait for artifact verification, which remains an ArtifactVerifier
// concern rather than a terminal/output concern.
type terminalWaitTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalWaitTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_wait",
		Description: "Wait for a terminal process to exit or its resources to be released.",
		Parameters: map[string]any{
			"id":      map[string]any{"type": "string", "description": "Terminal session ID"},
			"target":  map[string]any{"type": "string", "description": "exit or resource_released"},
			"timeout": map[string]any{"type": "integer", "description": "Optional wait timeout in seconds"},
		},
		Required: []string{"id", "target"},
	}
}

func (t *terminalWaitTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID      string `json:"id"`
		Target  string `json:"target"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action": "wait", "id": args.ID, "target": args.Target, "timeout": args.Timeout,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

type terminalCloseTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalCloseTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_close",
		Description: "Close/terminate a stateful terminal session.",
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Terminal session ID",
			},
		},
		Required: []string{"id"},
	}
}

func (t *terminalCloseTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action": "close",
		"id":     args.ID,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

type terminalListTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalListTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_list",
		Description: "List all active/recent terminal sessions for the current run.",
		Parameters:  map[string]any{},
	}
}

func (t *terminalListTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	payload, _ := json.Marshal(map[string]any{
		"action": "list",
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}

type terminalReconcileTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *terminalReconcileTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "terminal_reconcile",
		Description: "Check whether a terminal session's process is still alive and update its state.",
		Parameters: map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "Terminal session ID",
			},
		},
		Required: []string{"id"},
	}
}

func (t *terminalReconcileTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	payload, _ := json.Marshal(map[string]any{
		"action": "reconcile",
		"id":     args.ID,
	})
	return (&terminalTool{coordinator: t.coordinator}).Run(ctx, fantasy.ToolCall{Input: string(payload)})
}
