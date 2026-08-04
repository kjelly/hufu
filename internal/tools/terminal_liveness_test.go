//go:build linux || darwin

package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTerminalSessionIDsIn(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "the shape a real wait used",
			text: "cd /home/ubuntu/nfs/github/pilot && tail -200 logs/terminal/term-6f6a0856c7be1193795c0674.log",
			want: []string{"term-6f6a0856c7be1193795c0674"},
		},
		{
			name: "repeated mentions collapse to one",
			text: "ls -la logs/terminal/term-a581bd09ae3fa163e41d119c.log && wc -c logs/terminal/term-a581bd09ae3fa163e41d119c.log",
			want: []string{"term-a581bd09ae3fa163e41d119c"},
		},
		{
			name: "two sessions keep command order",
			text: "cat term-a581bd09ae3fa163e41d119c.log term-10e075fb2cc6c2027de0a49d.log",
			want: []string{"term-a581bd09ae3fa163e41d119c", "term-10e075fb2cc6c2027de0a49d"},
		},
		{
			name: "a word that merely starts with term is not an id",
			text: "systemctl status terminal-server && echo terminate",
		},
		{
			name: "wrong length is not an id",
			text: "term-abc123 term-6f6a0856c7be1193795c067 term-6f6a0856c7be1193795c06745",
		},
		{
			name: "uppercase hex is not the generated form",
			text: "term-6F6A0856C7BE1193795C0674",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := terminalSessionIDsIn(tc.text)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("terminalSessionIDsIn = %v, want %v", got, tc.want)
			}
		})
	}
}

func staticLivenessProbe(facts map[string]TerminalLiveness) TerminalLivenessFunc {
	return func(_ context.Context, id string) (TerminalLiveness, bool) {
		fact, ok := facts[id]
		return fact, ok
	}
}

func TestExitedWatchedTerminals(t *testing.T) {
	exitCode := 1
	now := time.Now()
	probe := staticLivenessProbe(map[string]TerminalLiveness{
		"term-000000000000000000000001": {SessionID: "term-000000000000000000000001", Running: true, State: "running"},
		"term-000000000000000000000002": {SessionID: "term-000000000000000000000002", State: "exited", ExitCode: &exitCode, ExitedAt: now},
		// A session the manager lost track of across a restart. Not evidence
		// that the process ended.
		"term-000000000000000000000003": {SessionID: "term-000000000000000000000003", State: "unknown"},
	})

	tests := []struct {
		name  string
		probe TerminalLivenessFunc
		ids   []string
		want  int
	}{
		{name: "no probe attached", ids: []string{"term-000000000000000000000002"}},
		{name: "no session watched", probe: probe},
		{name: "running", probe: probe, ids: []string{"term-000000000000000000000001"}},
		{name: "exited", probe: probe, ids: []string{"term-000000000000000000000002"}, want: 1},
		{name: "unknown is not exited", probe: probe, ids: []string{"term-000000000000000000000003"}},
		{name: "an id the probe does not know is not exited", probe: probe, ids: []string{"term-0000000000000000000000ff"}},
		{name: "mixed", probe: probe, ids: []string{"term-000000000000000000000001", "term-000000000000000000000002"}, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := exitedWatchedTerminals(context.Background(), tc.probe, tc.ids)
			if len(got) != tc.want {
				t.Fatalf("exitedWatchedTerminals = %+v, want %d entries", got, tc.want)
			}
		})
	}
}

func TestAllExitedBefore(t *testing.T) {
	instant := time.Now()
	before := TerminalLiveness{ExitedAt: instant.Add(-time.Minute)}
	after := TerminalLiveness{ExitedAt: instant.Add(time.Minute)}
	unknownTime := TerminalLiveness{}

	if !allExitedBefore([]TerminalLiveness{before}, instant) {
		t.Error("a process that ended before the wait began must be reported as such")
	}
	if allExitedBefore([]TerminalLiveness{before, after}, instant) {
		t.Error("one process ending mid-wait must keep the grace poll")
	}
	if allExitedBefore([]TerminalLiveness{unknownTime}, instant) {
		t.Error("an unknown exit time must not be assumed to predate the wait")
	}
	if allExitedBefore(nil, instant) {
		t.Error("no exited processes means nothing to report")
	}
}

func TestDescribeExitedTerminals(t *testing.T) {
	code := 2
	got := describeExitedTerminals([]TerminalLiveness{
		{SessionID: "term-000000000000000000000002", State: "exited", ExitCode: &code},
	})
	for _, want := range []string{"term-000000000000000000000002", "already ended", "exited", "exit code 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("description %q is missing %q", got, want)
		}
	}
}
