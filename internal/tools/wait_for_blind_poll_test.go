//go:build linux || darwin

package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestExposePollErrorsStripsStderrSuppression(t *testing.T) {
	tests := []struct {
		name      string
		command   string
		want      string
		rewritten bool
	}{
		{
			name:      "the shape that hid a missing path for 29 minutes",
			command:   "tail -200 logs/terminal/term-abc.log 2>/dev/null",
			want:      "tail -200 logs/terminal/term-abc.log",
			rewritten: true,
		},
		{
			name:      "spaced redirection",
			command:   "ls -la x 2> /dev/null && wc -c x 2>/dev/null",
			want:      "ls -la x && wc -c x",
			rewritten: true,
		},
		{
			name:      "closing the descriptor hides errors just as well",
			command:   "cat missing 2>&-",
			want:      "cat missing",
			rewritten: true,
		},
		{
			name:      "merging stderr into stdout is what we want and is left alone",
			command:   "ps -p 123 -o pid= >/dev/null 2>&1",
			want:      "ps -p 123 -o pid= >/dev/null 2>&1",
			rewritten: false,
		},
		{
			name:      "an ordinary command is untouched",
			command:   "test -f /tmp/ready",
			want:      "test -f /tmp/ready",
			rewritten: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, rewritten := exposePollErrors(tc.command)
			if got != tc.want || rewritten != tc.rewritten {
				t.Fatalf("exposePollErrors(%q) = (%q, %t), want (%q, %t)", tc.command, got, rewritten, tc.want, tc.rewritten)
			}
		})
	}
}

// TestWaitForReportsWhyASilentPollFailed is the regression for the observed
// stall: `tail <missing path> 2>/dev/null` exits non-zero with no output, so the
// wait reported only "(no output)" and the agent could not tell a broken check
// from a slow one. It then repeated the same mistake three more times.
func TestWaitForReportsWhyASilentPollFailed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created.log")
	resp := runWaitFor(t, `{"command":"tail -5 `+missing+` 2>/dev/null","interval_seconds":1,"timeout_seconds":2}`)

	if !resp.IsError {
		t.Fatalf("a never-met condition must be an error: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "No such file") {
		t.Errorf("the real error must reach the agent, not be discarded: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "removed stderr suppression") {
		t.Errorf("the rewrite must be disclosed rather than silent: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "not observing a changing state") {
		t.Errorf("an unchanging poll must be named as such: %q", resp.Content)
	}
}

// TestWaitForAbortsUnfalsifiableSuccessPattern covers the second failing shape:
// a pipeline that masks the exit code and prints nothing, combined with a
// success_pattern that therefore can never match. That combination burned a full
// 30-minute cap twice.
func TestWaitForAbortsUnfalsifiableSuccessPattern(t *testing.T) {
	resp := runWaitFor(t, `{"command":"true","success_pattern":"PLAY RECAP","interval_seconds":1,"timeout_seconds":600}`)

	if !resp.IsError {
		t.Fatalf("a silent poll with a success_pattern must fail, not wait: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "can never match") {
		t.Errorf("the response must explain that the pattern is unsatisfiable: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "no output at all") {
		t.Errorf("the response must name the missing evidence: %q", resp.Content)
	}
	// The abort must be prompt: three silent polls at one second each, not the
	// requested ten minutes.
	if strings.Contains(resp.Content, "timed out") {
		t.Errorf("the wait should have been abandoned early, not timed out: %q", resp.Content)
	}
}

// TestWaitForStillWaitsWhenOutputAppears keeps the early abort from breaking the
// ordinary case it must not touch: a check that is quiet at first and then
// prints what the pattern is looking for.
func TestWaitForStillWaitsWhenOutputAppears(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ready")
	// Poll 1 prints nothing; a later poll prints the marker after the file has
	// been created by the same command's side effect.
	command := "if [ -f " + marker + " ]; then echo PLAY RECAP ok; else touch " + marker + "; fi"
	resp := runWaitFor(t, `{"command":"`+command+`","success_pattern":"PLAY RECAP","interval_seconds":1,"timeout_seconds":20}`)

	if resp.IsError {
		t.Fatalf("a poll that becomes non-silent must still succeed: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "condition met") {
		t.Errorf("expected the wait to report success: %q", resp.Content)
	}
}

// TestWaitForFileAppearsIsNotAbortedAsSilent protects the canonical quiet
// predicate. `test -f` prints nothing by design and exits non-zero until the
// file exists; aborting that as "unfalsifiable" would break the tool's primary
// documented use.
func TestWaitForFileAppearsIsNotAbortedAsSilent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "appears")
	command := "test -f " + marker + " || { touch " + marker + "; false; }"
	resp := runWaitFor(t, `{"command":"`+command+`","interval_seconds":1,"timeout_seconds":20}`)

	if resp.IsError {
		t.Fatalf("a silent predicate with no success_pattern must keep waiting: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "condition met") {
		t.Errorf("expected the wait to report success: %q", resp.Content)
	}
}

// TestWaitForAbandonsWaitOnDeadTerminal is the regression for the largest single
// loss in the observed run: a worker started a deploy with terminal_start, the
// child exited within two seconds ("secret environment variable
// ipa_admin_password is not set or is empty"), and the worker then polled that
// session's log for 29 minutes. It repeated the shape three more times — 110
// minutes total — because nothing connected the poll to the process it depended
// on. The session id was in the polled command the whole time.
func TestWaitForAbandonsWaitOnDeadTerminal(t *testing.T) {
	code := 1
	exited := time.Now().Add(-time.Minute) // already gone before the wait began
	ctx := WithTerminalLiveness(context.Background(), staticLivenessProbe(map[string]TerminalLiveness{
		"term-6f6a0856c7be1193795c0674": {
			SessionID: "term-6f6a0856c7be1193795c0674",
			State:     "exited",
			ExitCode:  &code,
			ExitedAt:  exited,
		},
	}))

	started := time.Now()
	resp, err := executeWaitFor(ctx, fantasy.ToolCall{ID: "1", Name: "wait_for", Input: `{"command":"tail -200 logs/terminal/term-6f6a0856c7be1193795c0674.log","interval_seconds":1,"timeout_seconds":600}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeWaitFor: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("waiting on a dead process must fail: %q", resp.Content)
	}
	for _, want := range []string{"term-6f6a0856c7be1193795c0674", "already ended", "exit code 1", "terminal_read"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("response is missing %q: %q", want, resp.Content)
		}
	}
	// A process already gone when the wait started has nothing left to flush, so
	// this must end on the first poll rather than run the requested ten minutes.
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Errorf("wait took %s; it should have ended on the first poll", elapsed)
	}
}

// TestWaitForGivesOneGracePollWhenTerminalDiesMidWait protects the flush race:
// output written just before exit can still be in flight, so a process that dies
// during the wait gets one more poll before the wait is abandoned.
func TestWaitForGivesOneGracePollWhenTerminalDiesMidWait(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "flushed")
	sessionID := "term-a581bd09ae3fa163e41d119c"
	// ExitedAt is set after this wait begins, which is what marks the death as
	// mid-wait rather than pre-existing and so earns the grace poll.
	probe := staticLivenessProbe(map[string]TerminalLiveness{
		sessionID: {SessionID: sessionID, State: "exited", ExitedAt: time.Now().Add(time.Hour)},
	})
	ctx := WithTerminalLiveness(context.Background(), probe)

	// Poll 1 fails and creates the marker; the grace poll then succeeds, which
	// is exactly the late-flush case that must not be aborted.
	command := "test -f " + marker + " || { touch " + marker + "; false; }"
	resp, err := executeWaitFor(ctx, fantasy.ToolCall{ID: "1", Name: "wait_for", Input: `{"command":"` + command + `","terminal_id":"` + sessionID + `","interval_seconds":1,"timeout_seconds":30}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeWaitFor: %v", err)
	}
	if resp.IsError {
		t.Fatalf("output flushed on the grace poll must still satisfy the wait: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "condition met") {
		t.Errorf("expected the wait to report success: %q", resp.Content)
	}
}

// TestWaitForIgnoresTerminalsWithoutAProbe keeps the feature inert where no
// terminal manager is attached, so a plain tool invocation behaves as before.
func TestWaitForIgnoresTerminalsWithoutAProbe(t *testing.T) {
	resp := runWaitFor(t, `{"command":"test -f /definitely/not/here/term-6f6a0856c7be1193795c0674","interval_seconds":1,"timeout_seconds":2}`)
	if !resp.IsError {
		t.Fatalf("the condition is never met, so this must still time out: %q", resp.Content)
	}
	if strings.Contains(resp.Content, "already ended") {
		t.Errorf("with no probe attached no process fact may be claimed: %q", resp.Content)
	}
}

func TestWaitForRejectsMalformedTerminalID(t *testing.T) {
	resp := runWaitFor(t, `{"command":"true","terminal_id":"not-a-session"}`)
	if !resp.IsError || !strings.Contains(resp.Content, "terminal_start") {
		t.Fatalf("a malformed terminal_id must be rejected with guidance: %q", resp.Content)
	}
}
