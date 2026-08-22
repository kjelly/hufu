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
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/kjelly/hufu/internal/tools"
)

const terminalSessionsFile = "terminal_sessions.json"

const terminalScreenMaxBytes = 16 * 1024

var terminalANSISequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

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

// TerminalLifecycleEvent names facts emitted by the terminal manager. These
// are process facts only: consumers must not infer task completion from them.
type TerminalLifecycleEvent string

const (
	TerminalProcessStarted        TerminalLifecycleEvent = "process_started"
	TerminalProcessObserved       TerminalLifecycleEvent = "process_observed"
	TerminalProcessExited         TerminalLifecycleEvent = "process_exited"
	TerminalProcessReconciled     TerminalLifecycleEvent = "process_reconciled"
	TerminalResourceReleased      TerminalLifecycleEvent = "resource_released"
	TerminalCleanupRequestedEvent TerminalLifecycleEvent = "terminal_cleanup_requested"
	TerminalCleanupGracefulEvent  TerminalLifecycleEvent = "terminal_cleanup_graceful"
	TerminalCleanupForcedEvent    TerminalLifecycleEvent = "terminal_cleanup_forced"
	TerminalCleanupCompletedEvent TerminalLifecycleEvent = "terminal_cleanup_completed"
	TerminalCleanupManualEvent    TerminalLifecycleEvent = "terminal_cleanup_manual_intervention"
	TerminalCustodyTransferred    TerminalLifecycleEvent = "terminal_custody_transferred"
	TerminalLeaseRevoked          TerminalLifecycleEvent = "terminal_user_lease_revoked_for_cleanup"
	TerminalTaskTransferred       TerminalLifecycleEvent = "terminal_task_transferred"
)

type TerminalCustodian string

const (
	TerminalCustodianOwner       TerminalCustodian = "owner_task"
	TerminalCustodianCoordinator TerminalCustodian = "coordinator_cleanup"
	TerminalCustodianOperator    TerminalCustodian = "operator"
)

type TerminalCleanupState string

const (
	TerminalCleanupNone      TerminalCleanupState = "none"
	TerminalCleanupRequested TerminalCleanupState = "requested"
	TerminalCleanupGraceful  TerminalCleanupState = "graceful_termination"
	TerminalCleanupForced    TerminalCleanupState = "forced_termination"
	TerminalCleanupCompleted TerminalCleanupState = "completed"
	TerminalCleanupManual    TerminalCleanupState = "manual_intervention"
)

type TerminalCleanupReason string

const (
	TerminalCleanupTaskFailed     TerminalCleanupReason = "task_failed"
	TerminalCleanupTaskCancelled  TerminalCleanupReason = "task_cancelled"
	TerminalCleanupTaskIncomplete TerminalCleanupReason = "task_incomplete"
	TerminalCleanupRunCancelled   TerminalCleanupReason = "run_cancelled"
	TerminalCleanupRunShutdown    TerminalCleanupReason = "run_shutdown"
)

// TerminalWaitTarget is an explicit lifecycle condition a waiter may consume.
// It intentionally has no generic shell-condition variant.
type TerminalWaitTarget string

const (
	TerminalWaitExit             TerminalWaitTarget = "exit"
	TerminalWaitArtifactVerified TerminalWaitTarget = "artifact_verified"
	TerminalWaitResourceReleased TerminalWaitTarget = "resource_released"
)

// TerminalWaitRequest identifies both the resource and the fact being waited
// for. Artifact verification remains owned by an ArtifactVerifier, so the
// terminal manager rejects that target rather than treating output existence as
// proof of verification.
type TerminalWaitRequest struct {
	SessionID    string
	Target       TerminalWaitTarget
	PollInterval time.Duration
}

// TerminalWaitResult is the observed durable session state for a completed
// wait target.
type TerminalWaitResult struct {
	Session TerminalSession
	Target  TerminalWaitTarget
}

// TerminalMode determines whether a session uses ordinary pipes or a PTY.
type TerminalMode string

const (
	TerminalModePipe TerminalMode = "pipe"
	TerminalModePTY  TerminalMode = "pty"
)

// TerminalController owns terminal input for a running session.
type TerminalController string

const (
	TerminalControllerNone  TerminalController = "none"
	TerminalControllerAgent TerminalController = "agent"
	TerminalControllerUser  TerminalController = "user"
)

// TerminalLease identifies an exclusive human takeover.
type TerminalLease struct {
	ID string `json:"id"`
}

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
	ID          string `json:"id"`
	RunID       string `json:"run_id"`
	OwnerTaskID string `json:"owner_task_id"`
	// ControllerTaskID is the task currently authorized to use the session.
	// It normally equals OwnerTaskID. OwnerTaskID is immutable provenance; an
	// explicit operator handoff changes only this field.
	ControllerTaskID    string                `json:"controller_task_id,omitempty"`
	Agent               string                `json:"agent"`
	Command             []string              `json:"command"`
	WorkingDir          string                `json:"working_dir,omitempty"`
	StartedAt           time.Time             `json:"started_at"`
	LastReadAt          time.Time             `json:"last_read_at,omitempty"`
	ObservedAt          time.Time             `json:"observed_at,omitempty"`
	ExitedAt            time.Time             `json:"exited_at,omitempty"`
	ReconciledAt        time.Time             `json:"reconciled_at,omitempty"`
	ReleasedAt          time.Time             `json:"released_at,omitempty"`
	Running             bool                  `json:"running"`
	State               TerminalSessionState  `json:"state"`
	ExitCode            *int                  `json:"exit_code,omitempty"`
	OutputRefs          []ArtifactRef         `json:"output_refs,omitempty"`
	PID                 int                   `json:"pid,omitempty"`
	ProcessIdentity     *ProcessIdentity      `json:"process_identity,omitempty"`
	Mode                TerminalMode          `json:"mode,omitempty"`
	Controller          TerminalController    `json:"controller,omitempty"`
	LeaseID             string                `json:"lease_id,omitempty"`
	Rows                uint16                `json:"rows,omitempty"`
	Cols                uint16                `json:"cols,omitempty"`
	AttachedAt          time.Time             `json:"attached_at,omitempty"`
	Custodian           TerminalCustodian     `json:"custodian,omitempty"`
	CleanupState        TerminalCleanupState  `json:"cleanup_state,omitempty"`
	CleanupReason       TerminalCleanupReason `json:"cleanup_reason,omitempty"`
	CleanupRequestedAt  time.Time             `json:"cleanup_requested_at,omitempty"`
	CleanupCompletedAt  time.Time             `json:"cleanup_completed_at,omitempty"`
	CleanupError        string                `json:"cleanup_error,omitempty"`
	HandoffReason       string                `json:"handoff_reason,omitempty"`
	HandoffAuthorizedBy string                `json:"handoff_authorized_by,omitempty"`
	HandedOffAt         time.Time             `json:"handed_off_at,omitempty"`
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
	Mode         TerminalMode
	Rows         uint16
	Cols         uint16
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
	Screen  string
	EOF     bool
}

type TerminalCleanupRequest struct {
	OwnerTaskID string
	Reason      TerminalCleanupReason
	GracePeriod time.Duration
	ForceAfter  time.Duration
	// allowActiveRound is coordinator-only shutdown custody. It is never
	// supplied by a worker-facing terminal action; cleanupOne atomically
	// revokes owner custody before it signals the child.
	allowActiveRound bool
}

type TerminalCleanupResult struct {
	Session      TerminalSession
	Graceful     bool
	Forced       bool
	ManualAction bool
}

type TerminalCleanupManager interface {
	CleanupTaskTerminals(context.Context, TerminalCleanupRequest) ([]TerminalCleanupResult, error)
	CleanupRunTerminals(context.Context, string, TerminalCleanupReason) ([]TerminalCleanupResult, error)
}

// TerminalTransferRequest is an operator-authorized, explicit handoff. It is
// deliberately not included in TerminalManager, the interface exposed to the
// model-facing terminal tool.
type TerminalTransferRequest struct {
	SessionID             string
	RunID                 string
	SourceTaskID          string
	DestinationTaskID     string
	AcceptSessionID       string
	AcceptMode            TerminalMode
	Reason                string
	OperatorAuthorization string
}

// TerminalTransferManager is coordinator/operator-only authority for a
// cross-task handoff. It never changes TerminalSession.OwnerTaskID.
type TerminalTransferManager interface {
	TransferTerminal(context.Context, TerminalTransferRequest) (TerminalSession, error)
}

// TerminalManager is the public contract used by coordinator services and tools.
type TerminalManager interface {
	Start(context.Context, TerminalStartRequest) (*TerminalSession, error)
	Write(context.Context, string, TerminalInput) error
	Read(context.Context, string) (TerminalReadResult, error)
	Close(context.Context, string) error
	List(context.Context, string) ([]TerminalSession, error)
	Reconcile(context.Context, string) (TerminalSession, error)
	Resize(context.Context, string, uint16, uint16) error
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
	session TerminalSession
	// launchIdentity is the ephemeral custody proof established from the
	// process handle immediately after launch. It is deliberately separate
	// from session.ProcessIdentity: durable identity validation may fail after
	// the OS has already handed us a live process group.
	launchIdentity    *ProcessIdentity
	launchCustodySeen bool
	cmd               *exec.Cmd
	stdin             io.WriteCloser
	outputPath        string
	outputFile        *os.File
	done              chan struct{}
	processWaitDone   chan struct{}
	readOffset        int64
	ptyMaster         *os.File
	ptyCopyDone       chan struct{}
	ptyCopyErr        chan error
	ptyCopyErrRead    bool
	cleanupInProgress bool
	closeRequested    bool
	closeEventEmitted bool
	finalized         chan error
	groupSignal       func(int, syscall.Signal) error
	leaderObserve     func(int) (terminalLeaderObservation, error)
	groupContained    func(int, int) (bool, error)
}

type terminalLeaderObservation uint8

var errTerminalLeaderStillRunning = errors.New("terminal leader remained running after termination signal")

const (
	terminalLeaderRunning terminalLeaderObservation = iota + 1
	terminalLeaderExited
	terminalLeaderReaped
	terminalLeaderUnknown
)

func (o terminalLeaderObservation) String() string {
	switch o {
	case terminalLeaderRunning:
		return "running"
	case terminalLeaderExited:
		return "exited_waitable"
	case terminalLeaderReaped:
		return "reaped"
	default:
		return "unknown"
	}
}

// TerminalSessionManager owns live child handles and persists lifecycle state.
type TerminalSessionManager struct {
	mu               sync.RWMutex
	workspace        string
	path             string
	sessions         map[string]*managedTerminalSession
	eventSink        TerminalEventSink
	activeTaskRound  func(string) bool
	ptyCopyStartGate <-chan struct{}
	persistFile      func(string, []byte, os.FileMode) error
	// processIdentity is a package-private seam for testing the post-launch
	// durable identity validation failure without changing process state.
	processIdentity func(int) (*ProcessIdentity, error)
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

// LoadTerminalSessions reads durable terminal metadata for diagnostics without
// acquiring process ownership or changing lifecycle state. In particular,
// operator-facing commands must not turn a running session into unknown merely
// by listing it. Restart reconciliation remains the responsibility of a new
// TerminalSessionManager.
func LoadTerminalSessions(workspace string) ([]TerminalSession, error) {
	if workspace == "" {
		return nil, errors.New("load terminal sessions: empty workspace")
	}
	path := filepath.Join(workspace, logsDir, terminalSessionsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read terminal sessions: %w", err)
	}
	var sessions []TerminalSession
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, fmt.Errorf("decode terminal sessions: %w", err)
	}
	for i := range sessions {
		normalizeTerminalSessionDefaults(&sessions[i])
	}
	return sessions, nil
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
		if s.Mode == "" || s.Controller == "" || s.Custodian == "" || s.CleanupState == "" || s.ControllerTaskID == "" {
			changed = true
		}
		normalizeTerminalSessionDefaults(&s)
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

func normalizeTerminalSessionDefaults(session *TerminalSession) {
	if session.Mode == "" {
		session.Mode = TerminalModePipe
	}
	if session.Controller == "" {
		session.Controller = TerminalControllerNone
	}
	if session.Custodian == "" {
		session.Custodian = TerminalCustodianOwner
	}
	if session.CleanupState == "" {
		session.CleanupState = TerminalCleanupNone
	}
	if session.ControllerTaskID == "" {
		session.ControllerTaskID = session.OwnerTaskID
	}
}

func terminalControllerTaskID(session TerminalSession) string {
	if session.ControllerTaskID != "" {
		return session.ControllerTaskID
	}
	return session.OwnerTaskID
}

// TransferTerminal hands active control to a declared destination task after
// the coordinator has validated task lifecycle state and operator authority.
// The source remains the immutable provenance owner in OwnerTaskID.
func (m *TerminalSessionManager) TransferTerminal(_ context.Context, req TerminalTransferRequest) (TerminalSession, error) {
	if req.SessionID == "" || req.RunID == "" || req.SourceTaskID == "" || req.DestinationTaskID == "" {
		return TerminalSession{}, errors.New("transfer terminal: session, run, source task, and destination task IDs are required")
	}
	if req.SourceTaskID == req.DestinationTaskID {
		return TerminalSession{}, errors.New("transfer terminal: source and destination tasks must differ")
	}
	if req.AcceptSessionID != req.SessionID {
		return TerminalSession{}, errors.New("transfer terminal: destination must explicitly accept the session ID")
	}
	if req.AcceptMode == "" {
		return TerminalSession{}, errors.New("transfer terminal: destination must explicitly accept the terminal mode")
	}
	if req.Reason == "" || req.OperatorAuthorization == "" {
		return TerminalSession{}, errors.New("transfer terminal: reason and operator authorization are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[req.SessionID]
	if !ok {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q: session not found", req.SessionID)
	}
	s := &managed.session
	normalizeTerminalSessionDefaults(s)
	if s.RunID != req.RunID {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q: run %q does not match requested run %q", s.ID, s.RunID, req.RunID)
	}
	if s.ControllerTaskID != req.SourceTaskID {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q: source task %q does not control it", s.ID, req.SourceTaskID)
	}
	if !s.Running || s.State != TerminalSessionRunning {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q is not running", s.ID)
	}
	if s.Mode != req.AcceptMode {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q mode %q was not accepted", s.ID, s.Mode)
	}
	if s.Controller == TerminalControllerUser || s.LeaseID != "" {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q: user lease must be released first", s.ID)
	}
	if s.Custodian != TerminalCustodianOwner || s.CleanupState != TerminalCleanupNone || managed.cleanupInProgress {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q is unavailable during custody or cleanup", s.ID)
	}
	if m.activeTaskRound != nil && m.activeTaskRound(req.SourceTaskID) {
		return TerminalSession{}, fmt.Errorf("transfer terminal session %q: source task %q still has an active model round", s.ID, req.SourceTaskID)
	}
	s.ControllerTaskID = req.DestinationTaskID
	s.HandoffReason = req.Reason
	s.HandoffAuthorizedBy = req.OperatorAuthorization
	s.HandedOffAt = time.Now().UTC()
	if err := m.persistLocked(); err != nil {
		return TerminalSession{}, err
	}
	copy := deepCopyTerminalSession(*s)
	m.emit(string(TerminalTaskTransferred), copy, map[string]interface{}{
		"source_task_id": req.SourceTaskID, "destination_task_id": req.DestinationTaskID,
		"reason": req.Reason, "operator_authorization": req.OperatorAuthorization,
	})
	return copy, nil
}

// SetActiveTaskRoundChecker lets coordinator lifecycle code prevent cleanup
// from racing an owner model round. The checker is intentionally not part of
// the worker-facing TerminalManager interface.
func (m *TerminalSessionManager) SetActiveTaskRoundChecker(checker func(string) bool) {
	m.mu.Lock()
	m.activeTaskRound = checker
	m.mu.Unlock()
}

func (m *TerminalSessionManager) CleanupTaskTerminals(ctx context.Context, req TerminalCleanupRequest) ([]TerminalCleanupResult, error) {
	if req.OwnerTaskID == "" {
		return nil, errors.New("cleanup terminal sessions: owner task ID is required")
	}
	return m.cleanupMatching(ctx, req, func(s TerminalSession) bool {
		normalizeTerminalSessionDefaults(&s)
		return s.ControllerTaskID == req.OwnerTaskID
	})
}

func (m *TerminalSessionManager) CleanupRunTerminals(ctx context.Context, runID string, reason TerminalCleanupReason) ([]TerminalCleanupResult, error) {
	if runID == "" {
		return nil, errors.New("cleanup terminal sessions: run ID is required")
	}
	return m.cleanupMatching(ctx, TerminalCleanupRequest{Reason: reason}, func(s TerminalSession) bool { return s.RunID == runID })
}

// CleanupRunTerminalsAfterRoundTimeout is the coordinator's bounded-shutdown
// escape hatch. The owner round was already cancelled but did not unregister
// in time. Holding the manager mutex while switching custody prevents any
// later owner tool call from writing to the terminal before termination.
func (m *TerminalSessionManager) CleanupRunTerminalsAfterRoundTimeout(ctx context.Context, runID string, reason TerminalCleanupReason) ([]TerminalCleanupResult, error) {
	if runID == "" {
		return nil, errors.New("cleanup terminal sessions: run ID is required")
	}
	return m.cleanupMatching(ctx, TerminalCleanupRequest{Reason: reason, allowActiveRound: true}, func(s TerminalSession) bool { return s.RunID == runID })
}

func (m *TerminalSessionManager) cleanupMatching(ctx context.Context, req TerminalCleanupRequest, match func(TerminalSession) bool) ([]TerminalCleanupResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req.Reason == "" {
		req.Reason = TerminalCleanupTaskFailed
	}
	if req.GracePeriod <= 0 {
		req.GracePeriod = 100 * time.Millisecond
	}
	if req.ForceAfter <= 0 {
		req.ForceAfter = 500 * time.Millisecond
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.sessions))
	for id, managed := range m.sessions {
		if match(managed.session) && (managed.session.Running || managed.session.State == TerminalSessionRunning || managed.session.State == TerminalSessionUnknown) {
			ids = append(ids, id)
		}
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	results := make([]TerminalCleanupResult, 0, len(ids))
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		result, err := m.cleanupOne(ctx, id, req)
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (m *TerminalSessionManager) cleanupOne(ctx context.Context, id string, req TerminalCleanupRequest) (TerminalCleanupResult, error) {
	m.mu.Lock()
	managed, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: session not found", id)
	}
	if !req.allowActiveRound && m.activeTaskRound != nil && m.activeTaskRound(managed.session.ControllerTaskID) {
		m.mu.Unlock()
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: controlling task %q still has an active model round", id, managed.session.ControllerTaskID)
	}
	if managed.cleanupInProgress {
		m.mu.Unlock()
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q is already in progress", id)
	}
	if managed.session.CleanupState == TerminalCleanupCompleted && !managed.session.Running {
		copy := deepCopyTerminalSession(managed.session)
		m.mu.Unlock()
		return TerminalCleanupResult{Session: copy, Graceful: true}, nil
	}
	if !managed.session.Running && managed.session.State != TerminalSessionUnknown && managed.session.State != TerminalSessionRunning {
		copy := deepCopyTerminalSession(managed.session)
		m.mu.Unlock()
		return TerminalCleanupResult{Session: copy}, nil
	}
	leaseRevoked := false
	if managed.session.Controller == TerminalControllerUser {
		managed.session.Controller = TerminalControllerNone
		managed.session.LeaseID = ""
		managed.session.AttachedAt = time.Time{}
		leaseRevoked = true
	}
	managed.session.Custodian = TerminalCustodianCoordinator
	managed.session.CleanupState = TerminalCleanupRequested
	managed.session.CleanupReason = req.Reason
	managed.session.CleanupRequestedAt = time.Now().UTC()
	managed.session.CleanupError = ""
	managed.cleanupInProgress = true
	if err := m.persistLocked(); err != nil {
		managed.cleanupInProgress = false
		m.mu.Unlock()
		return TerminalCleanupResult{}, err
	}
	snapshot := deepCopyTerminalSession(managed.session)
	cmd, done, stdin := managed.cmd, managed.done, managed.stdin
	m.emit(string(TerminalCustodyTransferred), snapshot, map[string]interface{}{"from": TerminalCustodianOwner, "to": TerminalCustodianCoordinator})
	m.emit(string(TerminalCleanupRequestedEvent), snapshot, map[string]interface{}{"reason": req.Reason})
	if leaseRevoked {
		m.emit(string(TerminalLeaseRevoked), snapshot, map[string]interface{}{"reason": "coordinator_cleanup"})
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if current := m.sessions[id]; current == managed {
			managed.cleanupInProgress = false
		}
		m.mu.Unlock()
	}()

	if cmd == nil || cmd.Process == nil {
		return m.cleanupRestored(ctx, id, req)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	containErr := containTerminalProcessGroup(managed, syscall.SIGTERM)
	if containErr != nil && !errors.Is(containErr, errTerminalLeaderStillRunning) {
		return m.markCleanupManual(id, fmt.Sprintf("terminal group containment unavailable: %v", containErr))
	}
	if waitTerminalDone(ctx, done, req.GracePeriod) && m.terminalCleanupResolved(managed, ctx, req.GracePeriod) {
		return m.finishCleanup(id, true, false, "graceful termination")
	}

	m.mu.Lock()
	var forcedPersistErr error
	if current := m.sessions[id]; current == managed {
		managed.session.CleanupState = TerminalCleanupForced
		forcedPersistErr = m.persistLocked()
		if forcedPersistErr == nil {
			m.emit(string(TerminalCleanupForcedEvent), managed.session, map[string]interface{}{"reason": "grace period expired"})
		}
	}
	m.mu.Unlock()
	if forcedPersistErr != nil {
		return TerminalCleanupResult{}, fmt.Errorf("persist forced terminal cleanup state: %w", forcedPersistErr)
	}
	if err := containTerminalProcessGroup(managed, syscall.SIGKILL); err != nil {
		return m.markCleanupManual(id, fmt.Sprintf("terminal group containment unavailable after escalation: %v", err))
	}
	if waitTerminalDone(ctx, done, req.ForceAfter) && m.terminalCleanupResolved(managed, ctx, req.ForceAfter) {
		return m.finishCleanup(id, false, true, "forced termination")
	}
	return m.markCleanupManual(id, "process survived forced termination")
}

// terminalCleanupResolved verifies the process-group ownership boundary after
// the command's direct process has exited. A PTY descendant can outlive that
// process while retaining the slave, so cmd.Wait alone is not sufficient
// evidence that terminal custody can be released.
func (m *TerminalSessionManager) terminalCleanupResolved(managed *managedTerminalSession, _ context.Context, _ time.Duration) bool {
	if managed == nil || !terminalDoneClosed(managed.done) {
		return false
	}
	if managed.session.Running || managed.session.State == TerminalSessionRunning || managed.session.State == TerminalSessionUnknown || managed.session.CleanupState == TerminalCleanupManual {
		return false
	}
	return true
}

func (m *TerminalSessionManager) cleanupRestored(ctx context.Context, id string, req TerminalCleanupRequest) (TerminalCleanupResult, error) {
	m.mu.RLock()
	managed := m.sessions[id]
	if managed == nil {
		m.mu.RUnlock()
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: session not found", id)
	}
	pid := managed.session.PID
	m.mu.RUnlock()
	if pid > 0 && isPIDAlive(pid) {
		return m.markCleanupManual(id, "restored live process is not waitable by this manager; manual custody required")
	}
	contained, containmentErr := restoredTerminalGroupContained(managed)
	if containmentErr != nil || !contained {
		if containmentErr == nil {
			containmentErr = errors.New("owned process group containment is unproven")
		}
		return m.markCleanupManual(id, containmentErr.Error())
	}
	return m.finishRestoredCleanup(id, true, false, "verified process group is contained")
}

func (m *TerminalSessionManager) finishCleanup(id string, graceful, forced bool, reason string) (TerminalCleanupResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed := m.sessions[id]
	if managed == nil {
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: session not found", id)
	}
	if graceful {
		managed.session.CleanupState = TerminalCleanupGraceful
		if err := m.persistLocked(); err != nil {
			return TerminalCleanupResult{}, err
		}
		gracefulCopy := deepCopyTerminalSession(managed.session)
		m.emit(string(TerminalCleanupGracefulEvent), gracefulCopy, map[string]interface{}{"reason": reason})
	}
	if managed.session.Running && terminalDoneClosed(managed.done) {
		now := time.Now().UTC()
		managed.session.Running = false
		if managed.session.State != TerminalSessionClosed {
			managed.session.State = TerminalSessionExited
		}
		if managed.session.ExitedAt.IsZero() {
			managed.session.ExitedAt = now
		}
		managed.session.ReleasedAt = now
	}
	managed.session.CleanupState = TerminalCleanupCompleted
	managed.session.CleanupCompletedAt = time.Now().UTC()
	managed.session.CleanupError = ""
	if err := m.persistLocked(); err != nil {
		return TerminalCleanupResult{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit(string(TerminalCleanupCompletedEvent), copy, map[string]interface{}{"reason": reason, "forced": forced})
	return TerminalCleanupResult{Session: copy, Graceful: graceful, Forced: forced}, nil
}

func terminalDoneClosed(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func (m *TerminalSessionManager) finishRestoredCleanup(id string, graceful, forced bool, reason string) (TerminalCleanupResult, error) {
	m.mu.Lock()
	managed := m.sessions[id]
	if managed == nil {
		m.mu.Unlock()
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: session not found", id)
	}
	now := time.Now().UTC()
	managed.session.Running = false
	managed.session.State = TerminalSessionExited
	if managed.session.ExitedAt.IsZero() {
		managed.session.ExitedAt = now
	}
	managed.session.ReleasedAt = now
	if graceful {
		managed.session.CleanupState = TerminalCleanupGraceful
		if err := m.persistLocked(); err != nil {
			m.mu.Unlock()
			return TerminalCleanupResult{}, err
		}
		gracefulCopy := deepCopyTerminalSession(managed.session)
		m.emit(string(TerminalCleanupGracefulEvent), gracefulCopy, map[string]interface{}{"reason": reason})
	}
	managed.session.CleanupState = TerminalCleanupCompleted
	managed.session.CleanupCompletedAt = now
	managed.session.CleanupError = ""
	if err := m.persistLocked(); err != nil {
		m.mu.Unlock()
		return TerminalCleanupResult{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit(string(TerminalCleanupCompletedEvent), copy, map[string]interface{}{"reason": reason, "forced": forced})
	m.mu.Unlock()
	return TerminalCleanupResult{Session: copy, Graceful: graceful, Forced: forced}, nil
}

func (m *TerminalSessionManager) markCleanupManual(id, reason string) (TerminalCleanupResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed := m.sessions[id]
	if managed == nil {
		return TerminalCleanupResult{}, fmt.Errorf("cleanup terminal session %q: session not found", id)
	}
	managed.session.Custodian = TerminalCustodianCoordinator
	managed.session.CleanupState = TerminalCleanupManual
	managed.session.CleanupError = reason
	if err := m.persistLocked(); err != nil {
		return TerminalCleanupResult{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit(string(TerminalCleanupManualEvent), copy, map[string]interface{}{"reason": reason})
	return TerminalCleanupResult{Session: copy, ManualAction: true}, nil
}

func waitTerminalDone(ctx context.Context, done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return errors.New("invalid process-group ID")
	}
	return syscall.Kill(-pgid, signal)
}

func signalTerminalLeader(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return errors.New("invalid process ID")
	}
	return syscall.Kill(pid, signal)
}

func configureTerminalProcessGroup(cmd *exec.Cmd, mode TerminalMode) {
	if mode != TerminalModePipe {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
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
	if req.Mode == "" {
		req.Mode = TerminalModePipe
	}
	if req.Mode != TerminalModePipe && req.Mode != TerminalModePTY {
		return nil, fmt.Errorf("start terminal session: unknown mode %q", req.Mode)
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
	if req.NetworkBlock {
		if err := tools.SetNetNamespace(cmd); err != nil {
			_ = outputFile.Close()
			return nil, fmt.Errorf("set network namespace for terminal command: %w", err)
		}
	}
	var stdin io.WriteCloser
	var ptyMaster *os.File
	var ptyCopyDone chan struct{}
	var ptyCopyErr chan error
	if req.Mode == TerminalModePTY {
		ptyMaster, err = startTerminalPTY(cmd, req.Rows, req.Cols)
		if err != nil {
			_ = outputFile.Close()
			return nil, err
		}
		stdin = ptyMaster
		ptyCopyDone = make(chan struct{})
		ptyCopyErr = make(chan error, 1)
	} else {
		configureTerminalProcessGroup(cmd, req.Mode)
		cmd.Stdout = outputFile
		cmd.Stderr = outputFile
		stdin, err = cmd.StdinPipe()
		if err != nil {
			_ = outputFile.Close()
			return nil, fmt.Errorf("open terminal stdin: %w", err)
		}
		if err := cmd.Start(); err != nil {
			_ = outputFile.Close()
			return nil, fmt.Errorf("start terminal command: %w", err)
		}
	}

	relOutput, _ := filepath.Rel(m.workspace, outputPath)
	now := time.Now().UTC()
	launchIdentity, launchErr := getTerminalLaunchIdentity(cmd.Process.Pid)
	identityFn := m.processIdentity
	if identityFn == nil {
		identityFn = getProcessIdentity
	}
	identity, identityErr := identityFn(cmd.Process.Pid)
	identityValid := identityErr == nil && identity != nil && identity.PID == cmd.Process.Pid && identity.PGID == cmd.Process.Pid
	if !identityValid && identityErr == nil {
		identityErr = fmt.Errorf("leader PID %d does not own an equal process group", cmd.Process.Pid)
	}
	if launchErr != nil {
		identityErr = errors.Join(identityErr, fmt.Errorf("establish launch process-group custody: %w", launchErr))
	}
	if identityErr == nil && !identityValid {
		identityErr = fmt.Errorf("could not validate terminal leader identity")
	}

	managed := &managedTerminalSession{session: TerminalSession{
		ID: id, RunID: req.RunID, OwnerTaskID: req.OwnerTaskID, ControllerTaskID: req.OwnerTaskID, Agent: req.Agent,
		Command: append([]string(nil), req.Command...), WorkingDir: req.WorkingDir,
		StartedAt: now, Running: true, State: TerminalSessionRunning, PID: cmd.Process.Pid,
		ProcessIdentity: identity, Mode: req.Mode, Controller: TerminalControllerAgent, Rows: req.Rows, Cols: req.Cols,
		OutputRefs: []ArtifactRef{{Path: relOutput, Type: "terminal_output", Description: "complete terminal session output"}},
		Custodian:  TerminalCustodianCoordinator, CleanupState: TerminalCleanupRequested,
	}, launchIdentity: launchIdentity, launchCustodySeen: true, cmd: cmd, stdin: stdin, outputPath: outputPath, outputFile: outputFile, done: make(chan struct{}), processWaitDone: make(chan struct{}), finalized: make(chan error, 1), ptyMaster: ptyMaster, ptyCopyDone: ptyCopyDone, ptyCopyErr: ptyCopyErr}

	// The PTY copier is started only after managed custody exists. If identity
	// validation or initial persistence fails, the same managed record owns the
	// copier and output artifact during rollback.
	if req.Mode == TerminalModePTY {
		m.mu.RLock()
		copyStartGate := m.ptyCopyStartGate
		m.mu.RUnlock()
		go func() {
			if copyStartGate != nil {
				<-copyStartGate
			}
			_, copyErr := io.Copy(outputFile, ptyMaster)
			ptyCopyErr <- copyErr
			close(ptyCopyDone)
		}()
	}

	m.mu.Lock()
	m.sessions[id] = managed
	if identityErr == nil {
		managed.session.Custodian = TerminalCustodianOwner
		managed.session.CleanupState = TerminalCleanupNone
		err = m.persistLocked()
	} else {
		err = identityErr
	}
	m.mu.Unlock()
	if err != nil {
		if identityErr != nil {
			err = fmt.Errorf("start terminal session: could not establish leader process-group identity: %w", err)
		}
		return nil, m.abortStartedTerminal(managed, err)
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit(string(TerminalProcessStarted), copy, map[string]interface{}{"command": req.Command, "pid": copy.PID})
	// Keep the old event spelling during the migration to the process lifecycle
	// contract so existing event-store readers remain compatible.
	m.emit("terminal_session_started", copy, map[string]interface{}{"command": req.Command, "pid": copy.PID})

	go m.waitForExit(managed, req.ChildTimeout)
	return &copy, nil
}

// abortStartedTerminal is the single rollback path for failures after the OS
// has launched a terminal leader. It never uses a leader-only signal: the
// launch process group must be contained before the one and only cmd.Wait.
// Until containment and output finalization succeed, the managed session and
// artifact remain owned by the coordinator for manual recovery.
func (m *TerminalSessionManager) abortStartedTerminal(managed *managedTerminalSession, startErr error) error {
	if managed == nil {
		return startErr
	}

	m.mu.Lock()
	if current := m.sessions[managed.session.ID]; current == managed {
		managed.session.Custodian = TerminalCustodianCoordinator
		managed.session.CleanupState = TerminalCleanupRequested
		managed.session.CleanupReason = TerminalCleanupTaskFailed
		managed.session.CleanupRequestedAt = time.Now().UTC()
		managed.session.CleanupError = startErr.Error()
		managed.cleanupInProgress = true
		// A failed initial persist may have left no durable record. Best effort
		// persistence here establishes coordinator custody before any recovery
		// decision; the in-memory record is retained if persistence is unavailable.
		_ = m.persistLocked()
	}
	m.mu.Unlock()

	containErr := containTerminalProcessGroup(managed, syscall.SIGKILL)
	if containErr != nil {
		return m.retainStartedTerminalCustody(managed, errors.Join(startErr, fmt.Errorf("abort terminal session: %w", containErr)))
	}

	// Finalize output while the leader is still waitable. If PTY/output
	// finalization is unresolved, do not reap the leader and retain the record.
	finalErr := finalizeStartedTerminalOutput(managed)
	if finalErr != nil {
		return m.retainStartedTerminalCustody(managed, errors.Join(startErr, fmt.Errorf("abort terminal session finalization: %w", finalErr)))
	}

	// Containment and finalization are proven; this is the sole rollback reap.
	waitErr := managed.cmd.Wait()
	if managed.processWaitDone != nil {
		close(managed.processWaitDone)
	}
	if waitErr != nil {
		// A SIGKILL-induced exit is expected, but preserve it in diagnostics
		// without changing the caller-visible injected/start failure.
		startErr = errors.Join(startErr, fmt.Errorf("wait aborted terminal session: %w", waitErr))
	}

	m.mu.Lock()
	if current := m.sessions[managed.session.ID]; current == managed {
		delete(m.sessions, managed.session.ID)
		if persistErr := m.persistLocked(); persistErr != nil {
			m.sessions[managed.session.ID] = managed
			m.mu.Unlock()
			return m.retainStartedTerminalCustody(managed, errors.Join(startErr, fmt.Errorf("remove aborted terminal session record: %w", persistErr)))
		}
	}
	m.mu.Unlock()
	if removeErr := os.Remove(managed.outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
		m.mu.Lock()
		if _, exists := m.sessions[managed.session.ID]; !exists {
			m.sessions[managed.session.ID] = managed
		}
		m.mu.Unlock()
		return m.retainStartedTerminalCustody(managed, errors.Join(startErr, fmt.Errorf("remove aborted terminal output: %w", removeErr)))
	}
	return startErr
}

func finalizeStartedTerminalOutput(managed *managedTerminalSession) error {
	if managed == nil {
		return errors.New("terminal session is unavailable during abort")
	}
	var finalErr error
	if managed.session.Mode == TerminalModePTY {
		ioErr, ready := finalizeTerminalPTYCopy(managed)
		if !ready && ioErr == nil {
			ioErr = errors.New("terminal PTY output finalization unresolved")
		}
		finalErr = errors.Join(finalErr, ioErr)
	}
	if managed.outputFile != nil {
		if err := managed.outputFile.Sync(); err != nil {
			finalErr = errors.Join(finalErr, fmt.Errorf("sync terminal output: %w", err))
		}
		if err := managed.outputFile.Close(); err != nil {
			finalErr = errors.Join(finalErr, fmt.Errorf("close terminal output: %w", err))
		}
		if finalErr == nil {
			managed.outputFile = nil
		}
	}
	return finalErr
}

func (m *TerminalSessionManager) retainStartedTerminalCustody(managed *managedTerminalSession, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.sessions[managed.session.ID]; current == managed {
		// Preserve the launch proof in the durable record without treating it as
		// successful durable identity validation. Restored cleanup remains
		// fail-closed because this proof has no start-time component.
		if managed.session.ProcessIdentity == nil && managed.launchIdentity != nil {
			identityCopy := *managed.launchIdentity
			managed.session.ProcessIdentity = &identityCopy
		}
		managed.session.Running = true
		managed.session.State = TerminalSessionRunning
		managed.session.Custodian = TerminalCustodianCoordinator
		managed.session.CleanupState = TerminalCleanupManual
		managed.session.CleanupError = cause.Error()
		managed.cleanupInProgress = false
		if err := m.persistLocked(); err != nil {
			return errors.Join(cause, fmt.Errorf("persist aborted terminal custody: %w", err))
		}
		copy := deepCopyTerminalSession(managed.session)
		m.emit(string(TerminalCleanupManualEvent), copy, map[string]interface{}{"reason": cause.Error()})
		return cause
	}
	return cause
}

func (m *TerminalSessionManager) waitForExit(managed *managedTerminalSession, timeout time.Duration) {
	var finalErr error
	defer func() {
		managed.finalized <- finalErr
		close(managed.done)
	}()
	leaderState, observeErr := waitTerminalLeaderExit(managed, timeout)
	var containmentErr error
	if observeErr == nil && leaderState == terminalLeaderExited {
		containmentErr = containTerminalProcessGroup(managed, 0)
	} else if observeErr == nil && leaderState == terminalLeaderRunning {
		containmentErr = containTerminalProcessGroup(managed, syscall.SIGTERM)
		if errors.Is(containmentErr, errTerminalLeaderStillRunning) {
			containmentErr = containTerminalProcessGroup(managed, syscall.SIGKILL)
		}
	} else if observeErr == nil {
		containmentErr = fmt.Errorf("leader observation is not waitable: %s", leaderState)
	} else {
		containmentErr = observeErr
	}

	if containmentErr != nil {
		m.mu.Lock()
		if current := m.sessions[managed.session.ID]; current == managed {
			finalErr = m.retainTerminalCustodyLocked(managed, containmentErr)
		}
		m.mu.Unlock()
		return
	}

	// The group was contained while the leader remained waitable. This is the
	// only point at which the manager may reap the leader, and cmd.Wait is
	// called exactly once.
	err := managed.cmd.Wait()
	if managed.processWaitDone != nil {
		close(managed.processWaitDone)
	}

	ioErr, terminalReady := finalizeTerminalPTYCopy(managed)
	if managed.session.Mode != TerminalModePTY {
		terminalReady = true
	}
	if !terminalReady && ioErr == nil {
		ioErr = errors.New("terminal output finalization unresolved")
	}
	copyDrained := terminalReady || terminalPTYCopyDrained(managed)
	if managed.outputFile != nil && copyDrained {
		if sErr := managed.outputFile.Sync(); sErr != nil && ioErr == nil {
			ioErr = sErr
		}
		if cErr := managed.outputFile.Close(); cErr != nil && ioErr == nil {
			ioErr = cErr
		}
		managed.outputFile = nil
	}

	m.mu.Lock()
	if current := m.sessions[managed.session.ID]; current == managed {
		if !terminalReady || ioErr != nil {
			if ioErr == nil {
				ioErr = errors.New("terminal finalization unresolved")
			}
			finalErr = m.retainTerminalCustodyLocked(managed, ioErr)
			m.mu.Unlock()
			return
		}

		code := exitCode(err)
		managed.cleanupInProgress = false
		managed.session.ExitCode = &code
		managed.session.Running = false
		managed.session.ExitedAt = time.Now().UTC()
		managed.session.ReleasedAt = managed.session.ExitedAt
		if managed.closeRequested {
			managed.session.State = TerminalSessionClosed
			managed.session.CleanupState = TerminalCleanupCompleted
			managed.session.CleanupCompletedAt = managed.session.ExitedAt
			managed.session.CleanupError = ""
		} else {
			managed.session.State = TerminalSessionExited
		}
		pErr := m.persistLocked()
		if pErr != nil {
			finalErr = m.retainTerminalCustodyLocked(managed, fmt.Errorf("persist terminal finalization: %w", pErr))
			m.mu.Unlock()
			return
		}
		payload := map[string]interface{}{"exit_code": code}
		m.emit(string(TerminalProcessExited), managed.session, payload)
		m.emit(string(TerminalResourceReleased), managed.session, map[string]interface{}{"reason": "process_output_closed"})
		m.emit("terminal_session_exited", managed.session, payload)
		if managed.closeRequested {
			m.emit("terminal_session_closed", managed.session, map[string]interface{}{"reason": "close finalized"})
		}
	}
	m.mu.Unlock()
}

func waitTerminalLeaderExit(managed *managedTerminalSession, timeout time.Duration) (terminalLeaderObservation, error) {
	if managed == nil || managed.cmd == nil || managed.cmd.Process == nil {
		return terminalLeaderUnknown, errors.New("terminal leader handle is unavailable")
	}
	observe := managed.leaderObserve
	if observe == nil {
		observe = observeTerminalLeader
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		state, err := observe(managed.cmd.Process.Pid)
		if err != nil || state == terminalLeaderUnknown || state == terminalLeaderReaped {
			return state, err
		}
		if state == terminalLeaderExited {
			return state, nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return terminalLeaderRunning, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func containTerminalProcessGroup(managed *managedTerminalSession, initialSignal syscall.Signal) error {
	if managed == nil || managed.cmd == nil || managed.cmd.Process == nil {
		return errors.New("terminal leader handle is unavailable")
	}
	identity := managed.launchIdentity
	if !managed.launchCustodySeen {
		identity = managed.session.ProcessIdentity
	}
	if identity == nil || identity.PID <= 0 || identity.PGID <= 0 || identity.PID != managed.cmd.Process.Pid || identity.PGID != identity.PID {
		return errors.New("terminal leader/group ownership is unproven")
	}
	observe := managed.leaderObserve
	if observe == nil {
		observe = observeTerminalLeader
	}
	signalGroup := managed.groupSignal
	if signalGroup == nil {
		signalGroup = signalProcessGroup
	}
	contained := managed.groupContained
	if contained == nil {
		contained = terminalProcessGroupContained
	}

	state, err := observe(identity.PID)
	if err != nil {
		return fmt.Errorf("observe terminal leader: %w", err)
	}
	if state != terminalLeaderRunning && state != terminalLeaderExited {
		return fmt.Errorf("terminal leader is not waitable: %s", state)
	}
	if initialSignal != 0 {
		if err := signalGroup(identity.PGID, initialSignal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal owned terminal group: %w", err)
		}
	}
	if initialSignal != 0 && state == terminalLeaderRunning {
		state, err = waitTerminalLeaderExitWithObserver(identity.PID, observe, terminalPTYDrainTimeout)
		if err != nil || state != terminalLeaderExited {
			if err == nil {
				err = fmt.Errorf("%w: leader did not become waitable after termination", errTerminalLeaderStillRunning)
			}
			return err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		ok, checkErr := contained(identity.PGID, identity.PID)
		if checkErr != nil {
			return fmt.Errorf("verify terminal group containment: %w", checkErr)
		}
		if ok {
			return nil
		}
		// The leader is observed exited but remains waitable, so its PID/PGID
		// cannot be recycled. Escalation is therefore still safe here.
		if err := signalGroup(identity.PGID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("escalate owned terminal group: %w", err)
		}
		if attempt == 0 {
			time.Sleep(terminalPTYDrainTimeout)
		}
	}
	return errors.New("terminal process-group containment unresolved")
}

func waitTerminalLeaderExitWithObserver(pid int, observe func(int) (terminalLeaderObservation, error), timeout time.Duration) (terminalLeaderObservation, error) {
	deadline := time.Now().Add(timeout)
	for {
		state, err := observe(pid)
		if err != nil || state != terminalLeaderRunning {
			return state, err
		}
		if !time.Now().Before(deadline) {
			return state, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// retainTerminalCustodyLocked converts any finalization or persistence failure
// into a durable, fail-closed state. The process may be dead, but the manager
// has not established that all owned resources were safely released.
func (m *TerminalSessionManager) retainTerminalCustodyLocked(managed *managedTerminalSession, cause error) error {
	managed.session.Running = true
	managed.session.State = TerminalSessionRunning
	managed.session.Custodian = TerminalCustodianCoordinator
	managed.session.CleanupState = TerminalCleanupManual
	managed.session.CleanupError = cause.Error()
	managed.cleanupInProgress = false
	if err := m.persistLocked(); err != nil {
		return errors.Join(cause, fmt.Errorf("persist terminal custody state: %w", err))
	}
	copy := deepCopyTerminalSession(managed.session)
	m.emit(string(TerminalCleanupManualEvent), copy, map[string]interface{}{"reason": cause.Error()})
	return cause
}

const terminalPTYDrainTimeout = 500 * time.Millisecond

func finalizeTerminalPTYCopy(managed *managedTerminalSession) (error, bool) {
	if managed == nil || managed.ptyMaster == nil {
		return nil, true
	}
	var errs []error
	copyDrained := managed.ptyCopyDone == nil
	if managed.ptyCopyDone != nil {
		timer := time.NewTimer(terminalPTYDrainTimeout)
		select {
		case <-managed.ptyCopyDone:
			copyDrained = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			// A descendant retaining the slave can prevent the PTY reader from
			// seeing EOF. This timeout is diagnostic: terminate the owned
			// process group before closing the master, then classify the final
			// group and copier evidence below. Otherwise the descendant remains
			// live after the session is reported as exited.
		}
	}

	if !copyDrained {
		// The process group has already been contained by the custody protocol;
		// closing the master now releases a reader blocked behind a
		// descendant-owned slave. Keep
		// the wait bounded so a broken PTY implementation cannot deadlock the
		// coordinator.
		if closeErr := managed.ptyMaster.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrInvalid) {
			errs = append(errs, fmt.Errorf("close PTY master during drain escalation: %w", closeErr))
		}
		managed.ptyMaster = nil
		timer := time.NewTimer(terminalPTYDrainTimeout)
		select {
		case <-managed.ptyCopyDone:
			copyDrained = true
		case <-timer.C:
			errs = append(errs, errors.New("PTY output copier did not stop after master close"))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	if copyDrained && managed.ptyCopyErr != nil && !managed.ptyCopyErrRead {
		managed.ptyCopyErrRead = true
		if copyErr := <-managed.ptyCopyErr; copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, syscall.EIO) {
			errs = append(errs, fmt.Errorf("copy PTY output: %w", copyErr))
		}
	}
	if managed.ptyMaster != nil {
		if closeErr := managed.ptyMaster.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrInvalid) {
			errs = append(errs, fmt.Errorf("close PTY master: %w", closeErr))
		}
	}
	fatalErr := errors.Join(errs...)
	return fatalErr, copyDrained && fatalErr == nil
}

func terminalPTYCopyDrained(managed *managedTerminalSession) bool {
	if managed == nil || managed.ptyCopyDone == nil {
		return true
	}
	select {
	case <-managed.ptyCopyDone:
		return true
	default:
		return false
	}
}

func restoredTerminalGroupContained(managed *managedTerminalSession) (bool, error) {
	if managed == nil || managed.session.ProcessIdentity == nil {
		return false, errors.New("restored terminal process identity is unavailable")
	}
	identity := managed.session.ProcessIdentity
	if identity.PID <= 0 || identity.PGID <= 0 || identity.PID != identity.PGID {
		return false, errors.New("restored terminal leader/group ownership is unproven")
	}
	return terminalProcessGroupContained(identity.PGID, identity.PID)
}

func terminalProcessGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// AcquireUserLease grants exclusive terminal input to a local human operator.
func (m *TerminalSessionManager) AcquireUserLease(id string) (TerminalLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[id]
	if !ok {
		return TerminalLease{}, fmt.Errorf("terminal session %q not found", id)
	}
	if !managed.session.Running {
		return TerminalLease{}, fmt.Errorf("terminal session %q is not running", id)
	}
	if managed.session.Controller == TerminalControllerUser {
		return TerminalLease{}, fmt.Errorf("terminal session %q is already controlled by a user", id)
	}
	leaseID, err := newTerminalSessionID()
	if err != nil {
		return TerminalLease{}, err
	}
	managed.session.Controller = TerminalControllerUser
	managed.session.LeaseID = leaseID
	managed.session.AttachedAt = time.Now().UTC()
	if err := m.persistLocked(); err != nil {
		return TerminalLease{}, err
	}
	m.emit("terminal_session_taken_over", managed.session, nil)
	return TerminalLease{ID: leaseID}, nil
}

// ReleaseUserLease returns terminal input to the owning agent.
func (m *TerminalSessionManager) ReleaseUserLease(id, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("terminal session %q not found", id)
	}
	if managed.session.Controller != TerminalControllerUser || managed.session.LeaseID != leaseID {
		return fmt.Errorf("terminal session %q lease is no longer active", id)
	}
	managed.session.Controller = TerminalControllerAgent
	managed.session.LeaseID = ""
	managed.session.AttachedAt = time.Time{}
	if err := m.persistLocked(); err != nil {
		return err
	}
	m.emit("terminal_session_released", managed.session, nil)
	return nil
}

// AbandonUserLease records an unexpected client disconnect. It intentionally
// does not return control to the agent: the coordinator keeps the task paused
// until a human reconnects and explicitly detaches.
func (m *TerminalSessionManager) AbandonUserLease(id, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("terminal session %q not found", id)
	}
	if managed.session.Controller != TerminalControllerUser || managed.session.LeaseID != leaseID {
		return fmt.Errorf("terminal session %q lease is no longer active", id)
	}
	managed.session.Controller = TerminalControllerNone
	managed.session.LeaseID = ""
	managed.session.AttachedAt = time.Time{}
	if err := m.persistLocked(); err != nil {
		return err
	}
	m.emit("terminal_session_abandoned", managed.session, nil)
	return nil
}

// WriteUserLease forwards input from the current human controller.
func (m *TerminalSessionManager) WriteUserLease(id, leaseID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("terminal session %q not found", id)
	}
	if managed.session.Controller != TerminalControllerUser || managed.session.LeaseID != leaseID {
		return fmt.Errorf("terminal session %q lease is no longer active", id)
	}
	if !managed.session.Running || managed.stdin == nil {
		return fmt.Errorf("terminal session %q is not running", id)
	}
	if _, err := managed.stdin.Write(data); err != nil {
		return fmt.Errorf("write terminal session %q: %w", id, err)
	}
	return nil
}

// Resize changes the dimensions of a live PTY session.
func (m *TerminalSessionManager) Resize(ctx context.Context, id string, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return fmt.Errorf("terminal session %q resize dimensions must be positive", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	managed, err := m.ownerSessionLocked(ctx, id, "")
	if err != nil {
		return err
	}
	if managed.session.Mode != TerminalModePTY || managed.ptyMaster == nil {
		return fmt.Errorf("terminal session %q is not a PTY", id)
	}
	if err := resizeTerminalPTY(managed.ptyMaster, rows, cols); err != nil {
		return err
	}
	managed.session.Rows = rows
	managed.session.Cols = cols
	return m.persistLocked()
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
	if managed.session.Controller == TerminalControllerUser {
		m.mu.Unlock()
		return fmt.Errorf("terminal session %q is controlled by a user", id)
	}
	if managed.session.Controller != TerminalControllerAgent {
		m.mu.Unlock()
		return fmt.Errorf("terminal session %q is not controlled by its agent", id)
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
	now := time.Now().UTC()
	managed.session.LastReadAt = now
	firstObservation := len(output) > 0 && managed.session.ObservedAt.IsZero()
	if firstObservation {
		managed.session.ObservedAt = now
	}
	if err := m.persistLocked(); err != nil {
		return TerminalReadResult{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	if firstObservation {
		m.emit(string(TerminalProcessObserved), copy, map[string]interface{}{"bytes": len(output)})
	}
	m.emit("terminal_session_read", copy, map[string]interface{}{"bytes": len(output)})
	eof := !copy.Running && copy.State != TerminalSessionUnknown
	screen := terminalANSISequence.ReplaceAllString(string(data), "")
	if len(screen) > terminalScreenMaxBytes {
		screen = screen[len(screen)-terminalScreenMaxBytes:]
	}
	return TerminalReadResult{Session: copy, Output: output, Screen: screen, EOF: eof}, nil
}

func (m *TerminalSessionManager) Close(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if finalized, ok := m.sessions[id]; ok {
		normalizeTerminalSessionDefaults(&finalized.session)
		controller := terminalTaskID(ctx)
		if !finalized.session.Running && finalized.session.State == TerminalSessionClosed && finalized.session.CleanupState == TerminalCleanupCompleted {
			if controller == "" || controller != finalized.session.ControllerTaskID {
				m.mu.Unlock()
				return fmt.Errorf("terminal session %q belongs to task %q (currently controlled by task %q)", id, finalized.session.OwnerTaskID, finalized.session.ControllerTaskID)
			}
			m.mu.Unlock()
			return nil
		}
	}
	managed, err := m.ownerSessionLocked(ctx, id, "")
	if err != nil {
		m.mu.Unlock()
		return err
	}

	// cmd.Process remains non-nil after the manager's sole cmd.Wait. A
	// naturally finalized session is already contained and released; do not
	// re-enter live cleanup, signal a reaped leader, or manufacture manual
	// custody. Close promotes the durable lifecycle fact to closed and is
	// idempotent thereafter.
	if !managed.session.Running && managed.session.State == TerminalSessionExited {
		previousState := managed.session.State
		previousCleanup := managed.session.CleanupState
		previousCompletedAt := managed.session.CleanupCompletedAt
		managed.session.State = TerminalSessionClosed
		managed.session.CleanupState = TerminalCleanupCompleted
		managed.session.CleanupCompletedAt = time.Now().UTC()
		managed.session.CleanupError = ""
		managed.closeRequested = true
		if err := m.persistLocked(); err != nil {
			managed.session.State = previousState
			managed.session.CleanupState = previousCleanup
			managed.session.CleanupCompletedAt = previousCompletedAt
			managed.closeRequested = false
			m.mu.Unlock()
			return err
		}
		emitClose := !managed.closeEventEmitted
		managed.closeEventEmitted = true
		copy := deepCopyTerminalSession(managed.session)
		m.mu.Unlock()
		if emitClose {
			m.emit("terminal_session_closed", copy, map[string]interface{}{"reason": "already finalized"})
		}
		return nil
	}
	if !managed.session.Running && managed.session.State == TerminalSessionClosed {
		m.mu.Unlock()
		return nil
	}

	if managed.cmd != nil && managed.cmd.Process != nil {
		if !managed.closeRequested {
			managed.closeRequested = true
			managed.cleanupInProgress = true
			managed.session.Custodian = TerminalCustodianCoordinator
			managed.session.CleanupState = TerminalCleanupRequested
			managed.session.CleanupReason = TerminalCleanupTaskCancelled
			managed.session.CleanupRequestedAt = time.Now().UTC()
			managed.session.CleanupError = ""
			err = m.persistLocked()
			if err != nil {
				managed.closeRequested = false
				managed.cleanupInProgress = false
				m.mu.Unlock()
				return err
			}
			snapshot := deepCopyTerminalSession(managed.session)
			m.emit(string(TerminalCustodyTransferred), snapshot, map[string]interface{}{"from": TerminalCustodianOwner, "to": TerminalCustodianCoordinator})
			m.emit(string(TerminalCleanupRequestedEvent), snapshot, map[string]interface{}{"reason": TerminalCleanupTaskCancelled})
		}
		finalized := managed.finalized
		m.mu.Unlock()
		if managed.session.Mode != TerminalModePTY && managed.stdin != nil {
			_ = managed.stdin.Close()
		}
		if err := containTerminalProcessGroup(managed, syscall.SIGKILL); err != nil {
			_, _ = m.markCleanupManual(id, fmt.Sprintf("terminal group containment unavailable during close: %v", err))
			return err
		}
		if finalized == nil {
			return errors.New("terminal session finalization is unavailable")
		}
		select {
		case err := <-finalized:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// For restored sessions without a live cmd handle (e.g. unknown state)
	pid := managed.session.PID
	managed.closeRequested = true
	managed.cleanupInProgress = true
	managed.session.Custodian = TerminalCustodianCoordinator
	managed.session.CleanupState = TerminalCleanupRequested
	managed.session.CleanupReason = TerminalCleanupTaskCancelled
	managed.session.CleanupRequestedAt = time.Now().UTC()
	managed.session.CleanupError = ""
	if err := m.persistLocked(); err != nil {
		managed.closeRequested = false
		managed.cleanupInProgress = false
		m.mu.Unlock()
		return err
	}
	if pid > 0 && isPIDAlive(pid) {
		managed.session.CleanupState = TerminalCleanupManual
		managed.session.CleanupError = "restored process is not waitable by this manager; manual custody required"
		managed.cleanupInProgress = false
		_ = m.persistLocked()
		m.mu.Unlock()
		return fmt.Errorf("cannot close restored terminal session %q: process %d is not waitable by this manager; manual custody required", id, pid)
	}

	contained, containmentErr := restoredTerminalGroupContained(managed)
	if !contained || containmentErr != nil {
		if containmentErr == nil {
			containmentErr = errors.New("owned process group containment is unproven")
		}
		managed.session.CleanupState = TerminalCleanupManual
		managed.session.CleanupError = containmentErr.Error()
		managed.cleanupInProgress = false
		_ = m.persistLocked()
		m.mu.Unlock()
		return fmt.Errorf("cannot close restored terminal session %q: %v", id, containmentErr)
	}

	// Reaching this path proves the restored child is already gone and its
	// original process group contains no live descendants. Persist an exit fact
	// as well as resource release so an explicit exit waiter cannot wait forever.
	now := time.Now().UTC()
	managed.session.Running = false
	managed.session.State = TerminalSessionClosed
	if managed.session.ExitedAt.IsZero() {
		managed.session.ExitedAt = now
	}
	managed.session.ReleasedAt = now
	managed.session.CleanupState = TerminalCleanupCompleted
	managed.session.CleanupCompletedAt = now
	managed.session.CleanupError = ""
	err = m.persistLocked()
	if err != nil {
		managed.session.Running = true
		managed.session.State = TerminalSessionRunning
		managed.session.CleanupState = TerminalCleanupManual
		managed.session.CleanupError = err.Error()
		managed.cleanupInProgress = false
		_ = m.persistLocked()
		m.mu.Unlock()
		return err
	}
	copy := deepCopyTerminalSession(managed.session)
	m.mu.Unlock()
	m.emit(string(TerminalProcessExited), copy, map[string]interface{}{
		"reason": "restored_process_terminated_or_dead",
	})
	m.emit(string(TerminalResourceReleased), copy, map[string]interface{}{
		"reason": "restored_process_terminated_or_dead",
	})
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
	processExited := false
	if managed.session.State == TerminalSessionUnknown || !managed.session.Running {
		pid := managed.session.PID
		if pid > 0 && !isPIDAlive(pid) {
			contained, containmentErr := restoredTerminalGroupContained(managed)
			if contained && containmentErr == nil {
				managed.session.State = TerminalSessionExited
				managed.session.Running = false
				if managed.session.ExitedAt.IsZero() {
					managed.session.ExitedAt = time.Now().UTC()
				}
				if managed.session.ReleasedAt.IsZero() {
					managed.session.ReleasedAt = managed.session.ExitedAt
				}
				reconciled = true
				processExited = true
				reason = "pid_not_running_group_contained"
			} else if containmentErr != nil {
				reason = "pid_not_running_containment_unproven: " + containmentErr.Error()
			} else {
				reason = "pid_not_running_containment_unproven"
			}
		} else if pid > 0 && isPIDAlive(pid) {
			valid, _ := verifyProcessIdentity(managed.session.ProcessIdentity)
			if !valid {
				reason = "pid_identity_mismatch"
			} else {
				reason = "pid_still_running"
			}
		}
	}
	if prevState == TerminalSessionUnknown && !processExited {
		managed.session.Custodian = TerminalCustodianCoordinator
		managed.session.CleanupState = TerminalCleanupManual
		managed.session.CleanupError = reason
	}
	managed.session.ReconciledAt = time.Now().UTC()
	if err := m.persistLocked(); err != nil {
		return TerminalSession{}, err
	}
	copy := deepCopyTerminalSession(managed.session)
	payload := map[string]interface{}{
		"reconciled":     reconciled,
		"reason":         reason,
		"previous_state": prevState,
		"state":          copy.State,
	}
	if processExited {
		// A restored manager did not observe the original wait(2), but it has
		// now established the process is gone. Record that fact before the
		// reconciliation event so consumers never need to infer exit from an
		// output artifact or from reconciliation itself.
		m.emit(string(TerminalProcessExited), copy, map[string]interface{}{
			"reason": "reconciled_pid_not_running",
		})
		m.emit(string(TerminalResourceReleased), copy, map[string]interface{}{
			"reason": "reconciled_pid_not_running",
		})
	}
	m.emit(string(TerminalProcessReconciled), copy, payload)
	m.emit("terminal_session_reconciled", copy, payload)
	return copy, nil
}

// RequireTaskClosed rejects completion/retry while an owner still has a running
// or unknown child. Unknown is deliberately fail-closed after a restart.
func (m *TerminalSessionManager) RequireTaskClosed(taskID string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, managed := range m.sessions {
		s := managed.session
		normalizeTerminalSessionDefaults(&s)
		if s.ControllerTaskID == taskID && (s.State == TerminalSessionRunning || s.State == TerminalSessionUnknown || s.Running) {
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
	controller := terminalTaskID(ctx)
	if controller == "" {
		controller = suppliedTaskID
	}
	normalizeTerminalSessionDefaults(&managed.session)
	if controller == "" || controller != managed.session.ControllerTaskID {
		return nil, fmt.Errorf("terminal session %q belongs to task %q (currently controlled by task %q)", id, managed.session.OwnerTaskID, managed.session.ControllerTaskID)
	}
	if managed.session.Custodian != "" && managed.session.Custodian != TerminalCustodianOwner {
		return nil, fmt.Errorf("terminal session %q is under %s custody", id, managed.session.Custodian)
	}
	if managed.session.CleanupState != "" && managed.session.CleanupState != TerminalCleanupNone {
		return nil, fmt.Errorf("terminal session %q is under cleanup state %s", id, managed.session.CleanupState)
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
	writeFile := AtomicWriteFile
	if m.persistFile != nil {
		writeFile = m.persistFile
	}
	if err := writeFile(m.path, data, 0o644); err != nil {
		return fmt.Errorf("persist terminal sessions: %w", err)
	}
	return nil
}

func (m *TerminalSessionManager) emit(eventType string, session TerminalSession, payload map[string]interface{}) {
	if m.eventSink == nil {
		return
	}
	envelope := map[string]interface{}{
		"session_id":            session.ID,
		"run_id":                session.RunID,
		"owner_task_id":         session.OwnerTaskID,
		"controller_task_id":    session.ControllerTaskID,
		"agent":                 session.Agent,
		"state":                 session.State,
		"working_dir":           session.WorkingDir,
		"output_refs":           session.OutputRefs,
		"custodian":             session.Custodian,
		"cleanup_state":         session.CleanupState,
		"cleanup_reason":        session.CleanupReason,
		"handoff_reason":        session.HandoffReason,
		"handoff_authorized_by": session.HandoffAuthorizedBy,
		"handed_off_at":         session.HandedOffAt,
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
