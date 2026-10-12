package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

type fakeChatCoordinator struct {
	calls         []string
	prompts       []string
	invocationIDs []string
	results       []error
	answer        string
	last          *team.RunResult
	publish       bool
	cancel        context.CancelFunc
}

func (f *fakeChatCoordinator) next(kind string) (string, error) {
	f.calls = append(f.calls, kind)
	var err error
	if len(f.results) > 0 {
		err, f.results = f.results[0], f.results[1:]
	}
	if f.publish {
		f.last = &team.RunResult{Outcome: team.RunOutcomeUnverified}
	}
	if errors.Is(err, context.Canceled) && f.cancel != nil {
		f.cancel()
	}
	return f.answer, err
}

func (f *fakeChatCoordinator) Run(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	return f.next("run")
}

func (f *fakeChatCoordinator) ContinueWithPrompt(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
	return f.next("continue")
}

func (f *fakeChatCoordinator) SetInvocationID(id string) {
	f.invocationIDs = append(f.invocationIDs, id)
}

func (f *fakeChatCoordinator) LastRunResult() *team.RunResult { return f.last }

// preserveInvocationID restores the process-wide invocation ID that
// runChatTurn sets, so later tests (for example JSON goldens) are unaffected.
func preserveInvocationID(t *testing.T) {
	t.Helper()
	previous := opts.invocationID
	t.Cleanup(func() { opts.invocationID = previous })
}

func fakeChatTurnDeps(coord *fakeChatCoordinator, persisted *[]string) chatTurnDeps {
	return chatTurnDeps{
		coordinator:  coord,
		prepare:      func(line string) string { return "prepared:" + line },
		deliver:      func(answer string, err error) (string, error) { return answer, err },
		snapshotRuns: func() map[string]int { return map[string]int{} },
		collectRuns: func(map[string]int) []cliRunIdentity {
			return []cliRunIdentity{{Team: "t", RunID: "run-1"}}
		},
		persist: func(line string) { *persisted = append(*persisted, line) },
	}
}

func TestRunChatTurnSequencesRunAndContinue(t *testing.T) {
	failure := errors.New("provider down")
	tests := []struct {
		name      string
		results   []error
		cancelOn  int // 1-based turn whose context is cancelled; 0 = none
		resetAt   int // reset the sequencer before this 1-based turn; 0 = never
		wantCalls []string
		wantSaved int
	}{
		{name: "success then continue", results: []error{nil, nil}, wantCalls: []string{"run", "continue"}, wantSaved: 2},
		{name: "cancelled first turn counts", results: []error{context.Canceled, nil}, cancelOn: 1, wantCalls: []string{"run", "continue"}, wantSaved: 1},
		{name: "failed first turn retries run", results: []error{failure, nil}, wantCalls: []string{"run", "run"}, wantSaved: 1},
		{name: "reset returns to run", results: []error{nil, nil}, resetAt: 2, wantCalls: []string{"run", "run"}, wantSaved: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveInvocationID(t)
			coord := &fakeChatCoordinator{results: append([]error(nil), tt.results...), answer: "answer"}
			var persisted []string
			deps := fakeChatTurnDeps(coord, &persisted)
			var seq chatTurnSequencer
			for turn := 1; turn <= len(tt.results); turn++ {
				if turn == tt.resetAt {
					seq.reset()
				}
				ctx, cancel := context.WithCancel(context.Background())
				if turn == tt.cancelOn {
					coord.cancel = cancel
				}
				outcome := runChatTurn(ctx, deps, &seq, "inv-"+string(rune('0'+turn)), "line")
				cancel()
				coord.cancel = nil
				if turn == tt.cancelOn && !outcome.Cancelled {
					t.Fatalf("turn %d: Cancelled = false, want true", turn)
				}
				if turn != tt.cancelOn && outcome.Cancelled {
					t.Fatalf("turn %d: Cancelled = true, want false", turn)
				}
			}
			if len(coord.calls) != len(tt.wantCalls) {
				t.Fatalf("calls = %v, want %v", coord.calls, tt.wantCalls)
			}
			for i := range tt.wantCalls {
				if coord.calls[i] != tt.wantCalls[i] {
					t.Fatalf("calls = %v, want %v", coord.calls, tt.wantCalls)
				}
			}
			if len(persisted) != tt.wantSaved {
				t.Fatalf("persisted %d turns, want %d", len(persisted), tt.wantSaved)
			}
		})
	}
}

func TestRunChatTurnBindsInvocationAndDetectsFreshResult(t *testing.T) {
	stale := &team.RunResult{Outcome: team.RunOutcomeCompleted}
	tests := []struct {
		name       string
		publish    bool
		wantResult bool
	}{
		{name: "fresh result is reported", publish: true, wantResult: true},
		{name: "stale result is not reused", publish: false, wantResult: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveInvocationID(t)
			coord := &fakeChatCoordinator{results: []error{nil}, answer: "done", last: stale, publish: tt.publish}
			var persisted []string
			var seq chatTurnSequencer
			outcome := runChatTurn(context.Background(), fakeChatTurnDeps(coord, &persisted), &seq, "inv-42", "hello")
			if len(coord.invocationIDs) != 1 || coord.invocationIDs[0] != "inv-42" {
				t.Fatalf("invocation IDs = %v, want [inv-42]", coord.invocationIDs)
			}
			if opts.invocationID != "inv-42" {
				t.Fatalf("opts.invocationID = %q, want inv-42", opts.invocationID)
			}
			if (outcome.Result != nil) != tt.wantResult {
				t.Fatalf("Result = %v, want present %v", outcome.Result, tt.wantResult)
			}
			if outcome.Result == stale {
				t.Fatal("stale RunResult from an earlier turn was reported as this turn's result")
			}
			if len(coord.prompts) != 1 || coord.prompts[0] != "prepared:hello" {
				t.Fatalf("coordinator prompts = %v, want the prepared prompt", coord.prompts)
			}
			if outcome.Answer != "done" || len(outcome.RunIdentities) != 1 {
				t.Fatalf("outcome = %+v", outcome)
			}
		})
	}
}
