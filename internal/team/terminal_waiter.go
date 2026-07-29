package team

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TerminalSessionSource supplies durable terminal-session snapshots to a
// waiter. It intentionally offers no process control or task mutation.
type TerminalSessionSource interface {
	List(context.Context, string) ([]TerminalSession, error)
}

// TerminalSessionWaiter consumes an explicit lifecycle target for a terminal
// session. It is separate from TerminalSessionManager so waiting cannot make a
// process fact into a task-completion decision.
type TerminalSessionWaiter struct {
	sessions TerminalSessionSource
}

func NewTerminalSessionWaiter(sessions TerminalSessionSource) *TerminalSessionWaiter {
	return &TerminalSessionWaiter{sessions: sessions}
}

// Wait waits for an explicit terminal lifecycle fact. It never runs a command
// or interprets output; artifact_verified must be waited on through the
// ArtifactVerifier layer, which owns freshness and attempt identity.
func (w *TerminalSessionWaiter) Wait(ctx context.Context, req TerminalWaitRequest) (TerminalWaitResult, error) {
	if w == nil || w.sessions == nil {
		return TerminalWaitResult{}, errors.New("wait terminal session: session source is required")
	}
	if req.SessionID == "" {
		return TerminalWaitResult{}, errors.New("wait terminal session: session ID is required")
	}
	if req.Target == "" {
		return TerminalWaitResult{}, errors.New("wait terminal session: target is required")
	}
	if req.Target == TerminalWaitArtifactVerified {
		return TerminalWaitResult{}, errors.New("wait terminal session: artifact_verified is owned by ArtifactVerifier; terminal output is not verification evidence")
	}
	if req.Target != TerminalWaitExit && req.Target != TerminalWaitResourceReleased {
		return TerminalWaitResult{}, fmt.Errorf("wait terminal session: unknown target %q", req.Target)
	}
	interval := req.PollInterval
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}
	for {
		session, found, err := w.session(ctx, req.SessionID)
		if err != nil {
			return TerminalWaitResult{}, err
		}
		if !found {
			return TerminalWaitResult{}, fmt.Errorf("terminal session %q not found", req.SessionID)
		}
		if session.State == TerminalSessionUnknown {
			return TerminalWaitResult{}, fmt.Errorf("terminal session %q is unknown; reconcile it before waiting", req.SessionID)
		}
		if (req.Target == TerminalWaitExit && !session.ExitedAt.IsZero()) ||
			(req.Target == TerminalWaitResourceReleased && !session.ReleasedAt.IsZero()) {
			return TerminalWaitResult{Session: session, Target: req.Target}, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return TerminalWaitResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *TerminalSessionWaiter) session(ctx context.Context, id string) (TerminalSession, bool, error) {
	sessions, err := w.sessions.List(ctx, "")
	if err != nil {
		return TerminalSession{}, false, err
	}
	for _, session := range sessions {
		if session.ID == id {
			return session, true, nil
		}
	}
	return TerminalSession{}, false, nil
}
