package team

// Terminal sessions are durable task resources for processes which outlive a
// single model turn (interactive wizards, deploys, and streamed commands).
// They deliberately do not inherit the model request context: cancelling a
// model request must never make a still-running child look safe to retry.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/anomalyco/hufu/internal/tools"
)

const terminalSessionsFile = "terminal_sessions.json"

// TerminalSessionState records the durable lifecycle state of a session.
// Unknown is used after a process restart: hufu must not claim a child is gone
// merely because its in-memory process handle was lost.
type TerminalSessionState string

const (
	TerminalSessionRunning TerminalSessionState = "running"
	TerminalSessionExited  TerminalSessionState = "exited"
	TerminalSessionClosed  TerminalSessionState = "closed"
	TerminalSessionUnknown TerminalSessionState = "unknown"
)

// ProcessIdentity captures verifiable OS process attributes across restarts
type ProcessIdentity struct {
	PID       int    `json:"pid,omitempty"`
	PGID      int    `json:"pgid,omitempty"`
	StartTime int64  `json:"start_time,omitempty"`
	StartStr  string `json:"start_str,omitempty"`
	ExecPath  string `json:"exec_path,omitempty"`
}

// TerminalSession is a stateful child-process resource owned by one task.
type TerminalSession struct {
	ID              string               `json:"id"`
	RunID           string               `json:"run_id"`
	OwnerTaskID     string               `json:"owner_task_id"`
	Agent           string               `json:"agent"`
	Command         []string             `json:"command"`
	WorkingDir      string               `json:"working_dir,omitempty"`
	StartedAt       time.Time            `json:"started_at"`
	LastReadAt      time.Time            `json:"last_read_at,omitempty"`
	Running         bool                 `json:"running"`
	State           TerminalSessionState `json:"state"`
	ExitCode        *int                 `json:"exit_code,omitempty"`
	OutputRefs      []ArtifactRef        `json:"output_refs,omitempty"`
	PID             int                  `json:"pid,omitempty"`
	ProcessIdentity *ProcessIdentity     `json:"process_identity,omitempty"`
}

// TerminalStartRequest describes a child process. ChildTimeout applies only to
// the child process; it is intentionally independent from an agent/model timeout.
type TerminalStartRequest struct {
	RunID        string
	OwnerTaskID  string
	Agent        string
	Command      []string
	WorkingDir   string
	ChildTimeout time.Duration
	NetworkBlock bool
}

// TerminalInput carries the caller task identity required for ownership checks.
type TerminalInput struct {
	OwnerTaskID string
	Data        []byte
}

// TerminalReadResult contains output appended since the previous read by this
// manager instance. The complete durable log is referenced by Session.OutputRefs.
type TerminalReadResult struct {
	Session TerminalSession
	Output  []byte
	EOF     bool
}

// TerminalManager is the public contract used by coordinator services and tools.
type TerminalManager interface {
	Start(context.Context, TerminalStartRequest) (*TerminalSession, error)
	Write(context.Context, string, TerminalInput) error
	Read(context.Context, string) (TerminalReadResult, error)
	Close(context.Context, string) error
	List(context.Context, string) ([]TerminalSession, error)
	Reconcile(context.Context, string) (TerminalSession, error)
}

// TerminalEventSink receives durable lifecycle facts. Coordinator wires this to
// the append-only EventStore once a run has started.
type TerminalEventSink func(eventType, taskID string, payload map[string]interface{})

type terminalTaskIDKey struct{}

// WithTerminalTaskID authorizes terminal operations for a task. Coordinator
// task contexts already carry todoIDKey; this helper is for API consumers/tests.
func WithTerminalTaskID(ctx context.Context, taskID string) context.Context {
	return context.WithValue(ctx, terminalTaskIDKey{}, taskID)
}

func terminalTaskID(ctx context.Context) string {
	if ctx != nil {
		if taskID, ok := ctx.Value(terminalTaskIDKey{}).(string); ok && taskID != "" {
			return taskID
		}
		if taskID, ok := ctx.Value(todoIDKey{}).(string); ok {
			return taskID
		}
	}
	return ""
}

type managedTerminalSession struct {
	session    TerminalSession
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	outputPath string
	outputFile *os.File
	done       chan struct{}
	readOffset int64
}

// TerminalSessionManager owns live child handles and persists lifecycle state.
type TerminalSessionManager struct {
	mu        sync.RWMutex
	workspace string
	path      string
	sessions  map[string]*managedTerminalSession
	eventSink TerminalEventSink
}

// NewTerminalSessionManager restores durable records. Previously running
// sessions become unknown because a new process cannot safely own their child.
func NewTerminalSessionManager(workspace string, sink TerminalEventSink) (*TerminalSessionManager, error) {
	if workspace == "" {
		return nil, errors.New("terminal session manager: empty workspace")
	}
	m := &TerminalSessionManager{
		workspace: workspace,
		path:      filepath.Join(workspace, logsDir, terminalSessionsFile),
		sessions:  make(map[string]*managedTerminalSession),
		eventSink: sink,
	}
	if err := m.restore(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *TerminalSessionManager) restore() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read terminal sessions: %w", err)
	}
	var sessions []TerminalSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		return fmt.Errorf("decode terminal sessions: %w", err)
	}
	changed := false
	for i := range sessions {
		s := sessions[i]
		if s.State == TerminalSessionRunning || s.Running {
			prevState := s.State
			s.State = TerminalSessionUnknown
			s.Running = false
			changed = true
			m.emit("terminal_session_unknown", s, map[string]interface{}{
				"reason":         "restored_after_restart",
				"previous_state": prevState,
			})
		}
		m.sessions[s.ID] = &managedTerminalSession{session: s, outputPath: m.outputPath(s.ID)}
	}
	if changed {
		return m.persistLocked()
	}
	return nil
}

func (m *TerminalSessionManager) Start(ctx context.Context, req TerminalStartRequest) (*TerminalSession, error) {
	if req.OwnerTaskID == "" {
		return nil, errors.New("start terminal session: owner task ID is required")
	}
	if caller := terminalTaskID(ctx); caller != "" && caller != req.OwnerTaskID {
		return nil, fmt.Errorf("start terminal session: task %q cannot create a session for task %q", caller, req.OwnerTaskID)
	}
	if len(req.Command) == 0 || req.Command[0] == "" {
		return nil, errors.New("start terminal session: command is required")
	}

	id, err := newTerminalSessionID()
	if err != nil {
		return nil, err
	}
	outputPath := m.outputPath(id)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return nil, fmt.Errorf("create terminal output directory: %w", err)
	}
	outputFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create terminal output artifact: %w", err)
	}

	cmd := exec.Command(req.Command[0], req.Command[1:]...)
	cmd.Dir = req.WorkingDir
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if req.NetworkBlock {
		if err := tools.SetNetNamespace(cmd); err != nil {
			_ = outputFile.Close()
			return nil, fmt.Errorf("set network namespace for terminal command: %w", err)
		}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = outputFile.Close()
		return nil, fmt.Errorf("open terminal stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = outputFile.Close()
		return nil, fmt.Errorf("start terminal command: %w", err)
	}

	relOutput, _ := filepath.Rel(m.workspace, outputPath)
	now := time.Now().UTC()
	identity, _ := getProcessIdentity(cmd.Process.Pid)
	managed := &managedTerminalSession{session: TerminalSession{
		ID: id, RunID: req.RunID, OwnerTaskID: req.OwnerTaskID, Agent: req.Agent,
		Command: append([]string(nil), req.Command...), WorkingDir: req.WorkingDir,
		StartedAt: now, Running: true, State: TerminalSessionRunning, PID: cmd.Process.Pid,
		ProcessIdentity: identity,
		OutputRefs:      []ArtifactRef{{Path: relOutput, Type: "terminal_output", Description: "complete terminal session output"}},
	}, cmd: cmd, stdin: stdin, outputPath: outputPath, outputFile: outputFile, done: make(chan struct{})}

	m.mu.Lock()
	m.sessions[id] = managed
	err = m.persistLocked()
	if err != nil {
		delete(m.sessions, id)
	}
	copy := deepCopyTerminalSession(managed.session)
	m.mu.Unlock()
	if err != nil {
		killProcessGroup(cmd)
		_ = cmd.Wait()
		_ = outputFile.Close()
		_ = os.Remove(outputPath)
		return nil, err
	}
	m.emit("terminal_session_started", copy, map[string]interface{}{"command": req.Command, "pid": copy.PID})

	go m.waitForExit(managed, req.ChildTimeout)
	return &copy, nil
}

func (m *TerminalSessionManager) waitForExit(managed *managedTerminalSession, timeout time.Duration) {
	defer close(managed.done)
	var timer <-chan time.Time
	var stopTimer func()
	if timeout > 0 {
		t := time.NewTimer(timeout)
		timer = t.C
		stopTimer = func() {
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
	}
	done := make(chan error, 1)
	go func() { done <- managed.cmd.Wait() }()
	var err error
	if timer == nil {
		err = <-done
	} else {
		select {
		case err = <-done:
		case <-timer:
			killProcessGroup(managed.cmd)
			err = <-done
		}
		stopTimer()
	}

	m.mu.Lock()
	if current := m.sessions[managed.session.ID]; current == managed {
		var ioErr error
		if managed.outputFile != nil {
			if sErr := managed.outputFile.Sync(); sErr != nil && ioErr == nil {
				ioErr = sErr
			}
			if cErr := managed.outputFile.Close(); cErr != nil && ioErr == nil {
				ioErr = cErr
			}
			managed.outputFile = nil
		}
		code := exitCode(err)
		managed.session.ExitCode = &code
		managed.session.Running = false
		if managed.session.State != TerminalSessionClosed {
			managed.session.State = TerminalSessionExited
		}
		pErr := m.persistLocked()
		payload := map[string]interface{}{"exit_code": code}
		if ioErr != nil {
			payload["io_error"] = ioErr.Error()
		}
		if pErr != nil {
			payload["persist_error"] = pErr.Error()
		}
		m.emit("terminal_session_exited", managed.session, payload)
	}
	m.mu.Unlock()
}

func (m *TerminalSessionManager) Write(ctx context.Context, id string, input TerminalInput) error {
	m.mu.Lock()
	managed, err := m.ownerSessionLocked(ctx, id, input.OwnerTaskID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !managed.session.Running || managed.stdin == nil {
		m.mu.Unlock()
		return fmt.Errorf("terminal session %q is not running", id)
	}
	stdin := managed.stdin
	sessionCopy := deepCopyTerminalSession(managed.session)
	m.mu.Unlock()

	if len(input.Data) == 0 {
		return nil
	}
	if _, err := stdin.Write(input.Data); err != nil {
		return fmt.Errorf("write terminal session %q: %w", id, err)
	}
	m.emit("terminal_session_written", sessionCopy, map[string]interface{}{"bytes": len(input.Data)})
	return nil
}

func (m *TerminalSessionManager) Read(ctx context.Context, id string) (TerminalReadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, err := m.ownerSessionLocked(ctx, id, "")
	if err != nil {
		return TerminalReadResult{}, err
	}
	data, err := os.ReadFile(managed.outputPath)
	if err != nil && !os.IsNotExist(err) {
		return TerminalReadResult{}, fmt.Errorf("read terminal output: %w", err)
	}
	const maxReadChunk = 64 * 1024
	if managed.readOffset > int64(len(data)) {
		managed.readOffset = 0
	}
	unread := data[managed.readOffset:]
	if len(unread) > maxReadChunk {
		unread = unread[:maxReadChunk]
	}
	output := append([]byte(nil), unread...)
	managed.readOffset += int64(len(output))
	managed.session.LastReadAt = time.Now().UTC()
	if err := m.persistLocked(); err != nil {
		return TerminalReadResult{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit("terminal_session_read", copy, map[string]interface{}{"bytes": len(output)})
	eof := !copy.Running && copy.State != TerminalSessionUnknown
	return TerminalReadResult{Session: copy, Output: output, EOF: eof}, nil
}

func (m *TerminalSessionManager) Close(ctx context.Context, id string) error {
	m.mu.Lock()
	managed, err := m.ownerSessionLocked(ctx, id, "")
	if err != nil {
		m.mu.Unlock()
		return err
	}

	if managed.cmd != nil && managed.cmd.Process != nil {
		if managed.session.Running {
			if managed.stdin != nil {
				_ = managed.stdin.Close()
			}
			killProcessGroup(managed.cmd)
		}
		managed.session.Running = false
		managed.session.State = TerminalSessionClosed
		err = m.persistLocked()
		copy := deepCopyTerminalSession(managed.session)
		m.mu.Unlock()
		if err != nil {
			return err
		}
		if managed.done != nil {
			<-managed.done
		}
		m.emit("terminal_session_closed", copy, nil)
		return nil
	}

	// For restored sessions without a live cmd handle (e.g. unknown state)
	pid := managed.session.PID
	if pid > 0 && isPIDAlive(pid) {
		valid, _ := verifyProcessIdentity(managed.session.ProcessIdentity)
		if !valid {
			m.mu.Unlock()
			return fmt.Errorf("cannot close restored terminal session %q: process %d identity mismatch (PID may have been reused); state remains unknown and requires manual intervention", id, pid)
		}
		_ = killPIDGroup(pid)
		time.Sleep(50 * time.Millisecond)
		if isPIDAlive(pid) {
			m.mu.Unlock()
			return fmt.Errorf("cannot close restored terminal session %q: process %d is still running and could not be terminated", id, pid)
		}
	}

	managed.session.Running = false
	managed.session.State = TerminalSessionClosed
	err = m.persistLocked()
	copy := deepCopyTerminalSession(managed.session)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.emit("terminal_session_closed", copy, map[string]interface{}{
		"reconciled": true,
		"evidence":   "process_terminated_or_dead",
	})
	return nil
}

func (m *TerminalSessionManager) List(_ context.Context, runID string) ([]TerminalSession, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]TerminalSession, 0, len(m.sessions))
	for _, managed := range m.sessions {
		if runID == "" || managed.session.RunID == runID {
			result = append(result, deepCopyTerminalSession(managed.session))
		}
	}
	return result, nil
}

func (m *TerminalSessionManager) Reconcile(_ context.Context, id string) (TerminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[id]
	if !ok {
		return TerminalSession{}, fmt.Errorf("terminal session %q not found", id)
	}
	prevState := managed.session.State
	reconciled := false
	reason := "state_unchanged"
	if managed.session.State == TerminalSessionUnknown || !managed.session.Running {
		pid := managed.session.PID
		if pid > 0 && !isPIDAlive(pid) {
			managed.session.State = TerminalSessionExited
			managed.session.Running = false
			reconciled = true
			reason = "pid_not_running"
		} else if pid > 0 && isPIDAlive(pid) {
			valid, _ := verifyProcessIdentity(managed.session.ProcessIdentity)
			if !valid {
				reason = "pid_identity_mismatch"
			} else {
				reason = "pid_still_running"
			}
		}
	}
	if err := m.persistLocked(); err != nil {
		return TerminalSession{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit("terminal_session_reconciled", copy, map[string]interface{}{
		"reconciled":     reconciled,
		"reason":         reason,
		"previous_state": prevState,
		"state":          copy.State,
	})
	return copy, nil
}

// RequireTaskClosed rejects completion/retry while an owner still has a running
// or unknown child. Unknown is deliberately fail-closed after a restart.
func (m *TerminalSessionManager) RequireTaskClosed(taskID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, managed := range m.sessions {
		s := managed.session
		if s.OwnerTaskID == taskID && (s.State == TerminalSessionRunning || s.State == TerminalSessionUnknown || s.Running) {
			return fmt.Errorf("task %q has unclosed terminal session %q (%s); close or reconcile it before retrying or completing", taskID, s.ID, s.State)
		}
	}
	return nil
}

// RequireNoLeaks is the final acceptance gate for terminal resources.
func (m *TerminalSessionManager) RequireNoLeaks(runID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var leaked []string
	for _, managed := range m.sessions {
		s := managed.session
		if (runID == "" || s.RunID == runID) && (s.State == TerminalSessionRunning || s.State == TerminalSessionUnknown || s.Running) {
			leaked = append(leaked, fmt.Sprintf("%s (task %s, %s)", s.ID, s.OwnerTaskID, s.State))
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("leaked terminal sessions: %v", leaked)
	}
	return nil
}

func (m *TerminalSessionManager) ownerSessionLocked(ctx context.Context, id, suppliedTaskID string) (*managedTerminalSession, error) {
	managed, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("terminal session %q not found", id)
	}
	owner := terminalTaskID(ctx)
	if owner == "" {
		owner = suppliedTaskID
	}
	if owner == "" || owner != managed.session.OwnerTaskID {
		return nil, fmt.Errorf("terminal session %q belongs to task %q", id, managed.session.OwnerTaskID)
	}
	return managed, nil
}

func (m *TerminalSessionManager) outputPath(id string) string {
	return filepath.Join(m.workspace, logsDir, "terminal", id+".log")
}

func (m *TerminalSessionManager) persistLocked() error {
	sessions := make([]TerminalSession, 0, len(m.sessions))
	for _, managed := range m.sessions {
		sessions = append(sessions, deepCopyTerminalSession(managed.session))
	}
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return fmt.Errorf("encode terminal sessions: %w", err)
	}
	if err := AtomicWriteFile(m.path, data, 0o644); err != nil {
		return fmt.Errorf("persist terminal sessions: %w", err)
	}
	return nil
}

func (m *TerminalSessionManager) emit(eventType string, session TerminalSession, payload map[string]interface{}) {
	if m.eventSink == nil {
		return
	}
	envelope := map[string]interface{}{
		"session_id":    session.ID,
		"run_id":        session.RunID,
		"owner_task_id": session.OwnerTaskID,
		"agent":         session.Agent,
		"state":         session.State,
		"working_dir":   session.WorkingDir,
		"output_refs":   session.OutputRefs,
	}
	for k, v := range payload {
		envelope[k] = v
	}
	m.eventSink(eventType, session.OwnerTaskID, envelope)
}

func newTerminalSessionID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate terminal session ID: %w", err)
	}
	return "term-" + hex.EncodeToString(b), nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

func killPIDGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err != nil {
		err = syscall.Kill(pid, syscall.SIGKILL)
	}
	return err
}

func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	return false
}

func deepCopyTerminalSession(s TerminalSession) TerminalSession {
	cp := s
	if len(s.Command) > 0 {
		cp.Command = append([]string(nil), s.Command...)
	}
	if len(s.OutputRefs) > 0 {
		cp.OutputRefs = append([]ArtifactRef(nil), s.OutputRefs...)
	}
	if s.ProcessIdentity != nil {
		identityCopy := *s.ProcessIdentity
		cp.ProcessIdentity = &identityCopy
	}
	return cp
}
