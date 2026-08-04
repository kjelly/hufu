package tools

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Terminal sessions are owned by the coordinator package, which imports this
// one — so a poller here cannot reach the session manager directly. The
// coordinator instead injects a read-only probe into the tool context, the same
// shape used for the SSH session manager and the tool allowlist.
//
// Without it a wait had no way to notice that the process it was watching was
// already gone. In one real run a worker started a deploy in a terminal, the
// child exited within two seconds ("secret environment variable
// ipa_admin_password is not set or is empty"), and the worker then polled that
// terminal's log for 29 minutes. It repeated the pattern three more times: 110
// minutes of waiting on processes that had already died, ended only by the
// run's own deadline. A single terminal_read would have shown eof and the
// error the whole time.

// TerminalLiveness is the bounded process fact a poller needs about a terminal
// session.
//
// It deliberately carries no output. Reading a session's output advances that
// session's read offset, so a probe that fetched output would consume bytes the
// agent's own terminal_read has not seen yet — silently turning a diagnostic
// aid into data loss.
type TerminalLiveness struct {
	SessionID string
	Running   bool
	// State is the manager's session state, reported verbatim so the message an
	// agent reads uses the same vocabulary as the terminal tools.
	State    string
	ExitCode *int
	ExitedAt time.Time
}

// TerminalLivenessFunc resolves a terminal session ID to a process fact. The
// second result is false when no session with that ID is known, which must be
// treated as "no information" rather than "not running": an unknown ID is not
// evidence that anything died.
type TerminalLivenessFunc func(ctx context.Context, sessionID string) (TerminalLiveness, bool)

type terminalLivenessKeyType struct{}

// TerminalLivenessKey is the context key carrying a TerminalLivenessFunc.
var TerminalLivenessKey = terminalLivenessKeyType{}

// WithTerminalLiveness attaches a terminal liveness probe to ctx. A nil probe
// is ignored so callers without a terminal manager need no special case.
func WithTerminalLiveness(ctx context.Context, probe TerminalLivenessFunc) context.Context {
	if probe == nil {
		return ctx
	}
	return context.WithValue(ctx, TerminalLivenessKey, probe)
}

func terminalLivenessFrom(ctx context.Context) TerminalLivenessFunc {
	probe, _ := ctx.Value(TerminalLivenessKey).(TerminalLivenessFunc)
	return probe
}

// terminalSessionIDRe matches a terminal session ID exactly as the manager
// generates it: the literal "term-" followed by 12 random bytes in hex.
// Anchoring on that shape is what makes scanning a poll command for one safe.
var terminalSessionIDRe = regexp.MustCompile(`\bterm-[0-9a-f]{24}\b`)

// exitedWatchedTerminals returns the facts for those watched sessions whose
// process has ended. A session the probe does not know is skipped: an
// unrecognized ID is missing information, not a dead process, and must never be
// what ends a legitimate wait.
func exitedWatchedTerminals(ctx context.Context, probe TerminalLivenessFunc, sessionIDs []string) []TerminalLiveness {
	if probe == nil || len(sessionIDs) == 0 {
		return nil
	}
	var exited []TerminalLiveness
	for _, id := range sessionIDs {
		fact, known := probe(ctx, id)
		if !known || fact.Running {
			continue
		}
		// "unknown" means the manager lost track of the process, typically
		// across a restart. That is not evidence the process ended, and treating
		// it as such would abandon a wait that might still succeed.
		if fact.State == terminalStateUnknown {
			continue
		}
		exited = append(exited, fact)
	}
	return exited
}

// terminalStateUnknown mirrors the coordinator's TerminalSessionUnknown state.
// It is duplicated as a string rather than imported because terminal sessions
// live in the package that imports this one.
const terminalStateUnknown = "unknown"

// allExitedBefore reports whether every one of these processes had already ended
// before the given instant.
func allExitedBefore(exited []TerminalLiveness, instant time.Time) bool {
	for _, fact := range exited {
		if fact.ExitedAt.IsZero() || !fact.ExitedAt.Before(instant) {
			return false
		}
	}
	return len(exited) > 0
}

// describeExitedTerminals renders the process facts for an error message.
func describeExitedTerminals(exited []TerminalLiveness) string {
	parts := make([]string, 0, len(exited))
	for _, fact := range exited {
		desc := "terminal session " + fact.SessionID + " has already ended"
		if fact.State != "" {
			desc += " (state " + fact.State
			if fact.ExitCode != nil {
				desc += ", exit code " + strconv.Itoa(*fact.ExitCode)
			}
			desc += ")"
		}
		parts = append(parts, desc)
	}
	return strings.Join(parts, "; ")
}

// terminalSessionIDsIn returns the terminal session IDs mentioned in text, in
// order and without duplicates.
//
// Scanning the command matters as much as the explicit terminal_id parameter:
// an agent waiting on a terminal names it in the command it polls — it tails
// that session's log — so the association is already there to be used, and an
// existing prompt benefits without being rewritten.
func terminalSessionIDsIn(text string) []string {
	matches := terminalSessionIDRe.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	ids := make([]string, 0, len(matches))
	for _, id := range matches {
		if _, done := seen[id]; done {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
