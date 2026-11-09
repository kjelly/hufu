//go:build linux || darwin

package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
)

func TestWaitForPolling(t *testing.T) {
	type observation struct {
		output string
		exit   int
	}
	cases := []struct {
		name        string
		input       string
		polls       []observation
		pollDelay   time.Duration
		cancelAfter time.Duration
		wantElapsed time.Duration
		wantError   bool
		wantText    string
	}{
		{
			name:        "retry interval is clamped to one second",
			input:       `{"command":"probe","interval_seconds":0.01,"timeout_seconds":30}`,
			polls:       []observation{{exit: 1}, {exit: 1}, {output: "ready"}},
			wantElapsed: 2 * time.Second, wantText: "condition met after 3 attempt(s)",
		},
		{
			name:        "explicit interval",
			input:       `{"command":"probe","interval_seconds":2,"timeout_seconds":30}`,
			polls:       []observation{{exit: 1}, {output: "ready"}},
			wantElapsed: 2 * time.Second, wantText: "ready",
		},
		{
			name:        "default interval",
			input:       `{"command":"probe","timeout_seconds":30}`,
			polls:       []observation{{exit: 1}, {output: "ready"}},
			wantElapsed: 5 * time.Second, wantText: "ready",
		},
		{
			name:        "until failure waits for nonzero exit",
			input:       `{"command":"probe","until":"failure","interval_seconds":1,"timeout_seconds":30}`,
			polls:       []observation{{}, {exit: 1}},
			wantElapsed: time.Second, wantText: "condition met after 2 attempt(s)",
		},
		{
			name:        "unchanging observations stop before an unaffordable interval",
			input:       `{"command":"probe","interval_seconds":1,"timeout_seconds":3}`,
			polls:       []observation{{output: "booting", exit: 1}, {output: "booting", exit: 1}, {output: "booting", exit: 1}},
			wantElapsed: 2 * time.Second, wantError: true, wantText: "not observing a changing state",
		},
		{
			name:        "silent success pattern stops after three polls",
			input:       `{"command":"probe","interval_seconds":1,"success_pattern":"ready"}`,
			polls:       []observation{{}, {}, {}},
			wantElapsed: 2 * time.Second, wantError: true, wantText: "produced no output at all",
		},
		{
			name:  "deadline is not an until failure observation",
			input: `{"command":"probe","until":"failure","timeout_seconds":1}`,
			polls: []observation{{}}, pollDelay: 2 * time.Second,
			wantElapsed: time.Second, wantError: true, wantText: "timed out",
		},
		{
			name:  "parent cancellation interrupts the interval",
			input: `{"command":"probe","interval_seconds":2,"timeout_seconds":30}`,
			polls: []observation{{exit: 1}}, cancelAfter: 500 * time.Millisecond,
			wantElapsed: 500 * time.Millisecond, wantError: true, wantText: "cancelled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelAfter > 0 {
					go func() { time.Sleep(tc.cancelAfter); cancel() }()
				}
				calls := 0
				poll := func(ctx context.Context, _ string, _ []string, _ string, _ bool) (string, int, error) {
					if calls >= len(tc.polls) {
						return "", -1, fmt.Errorf("unexpected poll %d", calls+1)
					}
					got := tc.polls[calls]
					calls++
					if tc.pollDelay > 0 {
						select {
						case <-time.After(tc.pollDelay):
						case <-ctx.Done():
							return "", -1, nil
						}
					}
					return got.output, got.exit, nil
				}
				started := time.Now()
				response, err := executeWaitForWithRuntime(ctx, fantasy.ToolCall{Input: tc.input}, ToolConfig{}, waitForRuntime{minWaitForInterval, poll})
				if err != nil || response.IsError != tc.wantError || !strings.Contains(response.Content, tc.wantText) {
					t.Fatalf("response: %v %+v", err, response)
				}
				if calls != len(tc.polls) || time.Since(started) != tc.wantElapsed {
					t.Fatalf("polls=%d elapsed=%s, want %d and %s", calls, time.Since(started), len(tc.polls), tc.wantElapsed)
				}
			})
		})
	}
}

func TestWaitForCommandDeadline(t *testing.T) {
	// Keep a real subprocess check alongside the virtual-time polling tests.
	started := time.Now()
	response, err := executeWaitFor(t.Context(), fantasy.ToolCall{Input: `{"command":"sleep 5","until":"failure","timeout_seconds":0.1}`}, ToolConfig{})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "timed out") {
		t.Fatalf("deadline response: %v %+v", err, response)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("deadline did not stop the subprocess promptly")
	}
}
