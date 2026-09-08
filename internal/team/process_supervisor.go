package team

import (
	"context"
	"fmt"
	"io"
)

// ProcessSpec describes one external-provider process to start
// (docs/hufu-external-coding-agent-runtime-spec.md §16.2). Argv is executed
// directly, never through a shell.
type ProcessSpec struct {
	Argv []string
	Dir  string
	// Env is the complete, already-sanitized process environment (see
	// buildAllowlistedEnvironment / ExecutionWorld) — never inherited from
	// the Hufu process itself.
	Env []string
	// Stdout/Stderr are optional bounded writers; nil discards.
	Stdout io.Writer
	Stderr io.Writer
}

// ProcessHandle identifies one started process for later
// Interrupt/TerminateTree/KillTree calls.
type ProcessHandle interface {
	Pid() int
	// Wait blocks until the process exits and returns its exit error, if any.
	Wait() error
}

// ProcessSupervisor provides reliable process-TREE cleanup, not just cleanup
// of the single directly-started process
// (docs/hufu-external-coding-agent-runtime-spec.md §16.2). Interrupt sends a
// graceful, catchable request (SIGINT on Unix); TerminateTree requests
// graceful shutdown (SIGTERM); KillTree forces termination (SIGKILL) and
// MUST reach every descendant, not only the direct child.
type ProcessSupervisor interface {
	Start(ctx context.Context, spec ProcessSpec) (ProcessHandle, error)
	Interrupt(ctx context.Context, handle ProcessHandle) error
	TerminateTree(ctx context.Context, handle ProcessHandle) error
	KillTree(ctx context.Context, handle ProcessHandle) error
}

// ErrProcessSupervisorUnsupported is returned when this platform cannot
// guarantee reliable process-tree cleanup. Per §16.2, unattended external
// provider mode MUST fail preflight in that case rather than pretend cleanup
// is guaranteed — callers should treat this error as a preflight failure,
// not attempt to proceed without cleanup guarantees.
var ErrProcessSupervisorUnsupported = fmt.Errorf("process supervisor: reliable process-tree cleanup is not supported on this platform")

// NewProcessSupervisor returns this platform's ProcessSupervisor
// implementation (Unix: dedicated process group; Windows: unsupported until
// a Job Object implementation lands, §16.2).
func NewProcessSupervisor() ProcessSupervisor {
	return newPlatformProcessSupervisor()
}
