//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/kjelly/hufu/internal/audit"
)

var askUserActive atomic.Int32
var interactiveAbortRequested atomic.Bool
var closeStdinOnce sync.Once

// denialReasonStdin is the reader used by promptDenialReason. It is a
// package-level variable so tests can inject a fake stdin.
var denialReasonStdin = func() *bufio.Reader { return bufio.NewReader(os.Stdin) }

// denialReasonStderr is the writer used by promptDenialReason. Tests may
// redirect this to capture output. Must be an unbuffered writer (os.Stderr
// or os.Pipe); a bufio.Writer would defeat capture-by-redirection since
// the buffered bytes would not be flushed before the test reads the pipe.
var denialReasonStderr = os.Stderr

type auditLoggerKeyType struct{}

var AuditLoggerKey = auditLoggerKeyType{}

// ToolExecutionDisposition is emitted by a tool when it intentionally refuses
// a call before starting any external action.  It is deliberately structured:
// coordinator recovery must never infer "not executed" from response text.
type ToolExecutionDisposition struct {
	Kind       string
	ReasonCode string
	ToolName   string
	ToolCallID string
	Executed   bool
}

// ToolExecutionDispositionReporter receives pre-execution tool decisions for
// the current agent attempt. It is a context value to avoid coupling tools to
// the coordinator package.
type ToolExecutionDispositionReporter func(ToolExecutionDisposition)

type toolExecutionDispositionReporterKey struct{}

var ToolExecutionDispositionReporterKey = toolExecutionDispositionReporterKey{}

// ReportToolExecutionDisposition records a best-effort disposition. A missing
// reporter is valid for standalone tool use and tests.
func ReportToolExecutionDisposition(ctx context.Context, disposition ToolExecutionDisposition) {
	if reporter, ok := ctx.Value(ToolExecutionDispositionReporterKey).(ToolExecutionDispositionReporter); ok && reporter != nil {
		reporter(disposition)
	}
}

var onAskUserStart func()
var onAskUserDone func()

func SetAuditLogger(ctx context.Context, logger *audit.AuditLogger) context.Context {
	return context.WithValue(ctx, AuditLoggerKey, logger)
}

func GetAuditLogger(ctx context.Context) *audit.AuditLogger {
	if v, ok := ctx.Value(AuditLoggerKey).(*audit.AuditLogger); ok {
		return v
	}
	return nil
}

func SetOnAskUserStart(fn func()) {
	onAskUserStart = fn
}

func SetOnAskUserDone(fn func()) {
	onAskUserDone = fn
}

// NotifyAskUserStart is called by the ask_user tool just before reading stdin.
// It invokes the registered start hook (e.g. to release the terminal in TUI mode).
func NotifyAskUserStart() {
	if onAskUserStart != nil {
		onAskUserStart()
	}
}

// NotifyAskUserDone is called by the ask_user tool after reading stdin.
// It invokes the registered done hook (e.g. to restore the terminal in TUI mode).
func NotifyAskUserDone() {
	if onAskUserDone != nil {
		onAskUserDone()
	}
}

func SetAskUserActive(active bool) {
	interactiveWait.mu.Lock()
	if active {
		askUserActive.Store(1)
		if interactiveWait.start.IsZero() {
			interactiveWait.start = time.Now()
		}
	} else {
		askUserActive.Store(0)
		if !interactiveWait.start.IsZero() {
			interactiveWait.total += time.Since(interactiveWait.start)
			interactiveWait.start = time.Time{}
		}
	}
	interactiveWait.mu.Unlock()
	if !active {
		NotifyAskUserDone()
	}
}

func IsAskUserActive() bool {
	return askUserActive.Load() == 1
}

// interactiveWait accumulates time this process has spent blocked on
// interactive prompts (ask_user, path consent). total is the completed wait
// and start is the in-flight prompt's start, or zero when none is active;
// prompts are serialized on StdinMu so at most one runs at a time. The mutex
// keeps both fields one snapshot: reading them independently could briefly
// drop a finished prompt's wait and expire a compensated deadline. time.Time
// keeps the monotonic clock reading, so wall-clock jumps do not skew it.
var interactiveWait struct {
	mu    sync.Mutex
	start time.Time
	total time.Duration
}

// InteractiveWaitTotal returns the cumulative time spent waiting on
// interactive prompts, including the currently active one. Task deadlines
// take the delta of this value so human response time does not count
// against an agent's time budget (see WithInteractiveAwareTimeout).
func InteractiveWaitTotal() time.Duration {
	interactiveWait.mu.Lock()
	defer interactiveWait.mu.Unlock()
	total := interactiveWait.total
	if !interactiveWait.start.IsZero() {
		total += time.Since(interactiveWait.start)
	}
	return total
}

// RequestInteractiveAbort marks interactive input as aborted and closes stdin
// once so any in-flight prompt/consent read returns immediately.
func RequestInteractiveAbort() {
	interactiveAbortRequested.Store(true)
	closeStdinOnce.Do(func() {
		_ = os.Stdin.Close()
	})
}

func IsInteractiveAbortRequested() bool {
	return interactiveAbortRequested.Load()
}

// AskUserTUIOption is a choice for the ask_user TUI dialog.
// Mirrors tui.AskUserOption without a cross-package dependency.
var onAskUserTUI func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool)

func SetOnAskUserTUI(fn func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool)) {
	onAskUserTUI = fn
}

// TryAskUserTUI attempts to handle ask_user via a TUI-native dialog.
// Returns (jsonResp, true) if handled; ("", false) if TUI is not active or ctx is cancelled.
func TryAskUserTUI(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool) {
	if onAskUserTUI == nil {
		return "", false
	}
	return onAskUserTUI(ctx, question, qtype, opts, allowAny)
}

// IsUnattended reports whether the context marks the run as unattended
// (no human available to answer prompts or grant permissions).
func IsUnattended(ctx context.Context) bool {
	v, _ := ctx.Value(UnattendedKey).(bool)
	return v
}

// IsAutoApprove reports whether ask_user should auto-select clearly safe
// options instead of prompting the user when possible.
func IsAutoApprove(ctx context.Context) bool {
	v, _ := ctx.Value(AutoApproveKey).(bool)
	return v
}

var interactiveEnvPinned atomic.Value // bool, set once at startup

// CaptureInteractiveEnvironment pins the interactivity decision for the rest
// of the process. Call it once at startup: stdin can stop looking like a
// terminal mid-run (TUI widgets or prompts taking over the fd), and a
// per-call check would silently flip every permission decision from allow to
// deny partway through a session.
func CaptureInteractiveEnvironment() {
	interactiveEnvPinned.Store(detectInteractiveEnvironment())
}

// IsInteractiveEnvironment reports whether stdin is a terminal and the process
// is not running in a known CI environment. Uses the value pinned by
// CaptureInteractiveEnvironment when available and probes live otherwise
// (tests swap os.Stdin and rely on the live probe).
func IsInteractiveEnvironment() bool {
	if v, ok := interactiveEnvPinned.Load().(bool); ok {
		return v
	}
	return detectInteractiveEnvironment()
}

func detectInteractiveEnvironment() bool {
	for _, v := range CIEnvVars {
		if os.Getenv(v) != "" {
			return false
		}
	}
	return term.IsTerminal(os.Stdin.Fd())
}

var onNeedsHuman func(question string)

// SetOnNeedsHuman registers a hook invoked when an agent requests human input
// but none is available (unattended / non-interactive). Used to push a
// notification so an operator can follow up out-of-band.
func SetOnNeedsHuman(fn func(question string)) { onNeedsHuman = fn }

// NotifyNeedsHuman fires the needs-human hook, if registered.
func NotifyNeedsHuman(question string) {
	if onNeedsHuman != nil {
		onNeedsHuman(question)
	}
}

// normalizeWorkspacePath rewrites a path that uses the workspace directory name
// as if it were at the filesystem root (e.g. /workspace/x) into a relative path
// (./workspace/x) so it resolves correctly under the workDir.
func normalizeWorkspacePath(path, workspaceName string) string {
	if workspaceName == "" || path == "" {
		return path
	}
	prefix := "/" + workspaceName
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return "." + path
	}
	return path
}

var CIEnvVars = []string{
	"CI",
	"CI_SERVER",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"CIRCLECI",
	"TRAVIS",
	"JENKINS_URL",
	"TF_BUILD",
	"APPVEYOR",
	"BUILDKITE",
	"DRONE",
}
