package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// scriptedSemanticRunInputResolver returns its results in order and repeats
// the last one. It records each attempt number and the timeout it was given.
type scriptedSemanticRunInputResolver struct {
	results  []scriptedSemanticResult
	calls    int
	attempts []int
	timeouts []time.Duration
}

type scriptedSemanticResult struct {
	value json.RawMessage
	err   error
}

func (r *scriptedSemanticRunInputResolver) Resolve(ctx context.Context, request SemanticRunInputRequest) (json.RawMessage, error) {
	result := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	r.attempts = append(r.attempts, request.Attempt)
	if deadline, ok := ctx.Deadline(); ok {
		r.timeouts = append(r.timeouts, time.Until(deadline))
	}
	if result.err != nil {
		return nil, result.err
	}
	return json.RawMessage(string(result.value)), nil
}

func TestSemanticRunInputFailureDefaultsVisibly(t *testing.T) {
	transportErr := errors.New("semantic input translation: stream transport error: unexpected EOF")
	malformedErr := errors.New("semantic input response is not one JSON value: unexpected EOF")
	lastN := json.RawMessage(`{"kind":"last_n","count":22,"history":"first_parent","head":"HEAD"}`)
	cases := []struct {
		name           string
		results        []scriptedSemanticResult
		explicit       json.RawMessage
		wantAssignment bool
		wantCalls      int
		wantFailure    string
	}{
		{name: "transport failure on every attempt", results: []scriptedSemanticResult{{err: transportErr}}, wantCalls: 3, wantFailure: "unexpected EOF"},
		{name: "malformed answer on every attempt", results: []scriptedSemanticResult{{err: malformedErr}}, wantCalls: 3, wantFailure: "not one JSON value"},
		{name: "failure then success", results: []scriptedSemanticResult{{err: transportErr}, {value: lastN}}, wantAssignment: true, wantCalls: 2},
		{name: "success on the last attempt", results: []scriptedSemanticResult{{err: transportErr}, {err: malformedErr}, {value: lastN}}, wantAssignment: true, wantCalls: 3},
		{name: "resolver unavailable keeps the default", results: []scriptedSemanticResult{{err: errSemanticRunInputUnavailable}}, wantCalls: 1},
		{name: "null answer keeps the default", results: []scriptedSemanticResult{{value: json.RawMessage(`null`)}}, wantCalls: 1},
		{name: "explicit value wins over a failure", results: []scriptedSemanticResult{{err: transportErr}}, explicit: lastN, wantCalls: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
			var warnings []string
			coordinator.reportStatus = func(event StatusEvent) {
				if event.Type == "warning" {
					warnings = append(warnings, event.Message)
				}
			}
			semantic := &scriptedSemanticRunInputResolver{results: tc.results}
			coordinator.SetSemanticRunInputResolver(semantic)
			// The deterministic resolver validates candidates and finds nothing
			// in prose, as reviewprep does.
			registerSemanticCandidateValidator(coordinator, nil)
			explicit := map[string]json.RawMessage{}
			if tc.explicit != nil {
				explicit["review.scope"] = tc.explicit
			}

			assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "review 最近22個的 git commit", explicit, "run-1", true)
			if err != nil {
				t.Fatalf("resolveRunInputCandidates: %v; an unstable resolver must not fail the run", err)
			}
			if semantic.calls != tc.wantCalls {
				t.Fatalf("semantic resolver calls = %d, want %d", semantic.calls, tc.wantCalls)
			}
			for index, attempt := range semantic.attempts {
				if attempt != index+1 {
					t.Fatalf("attempts = %v, want 1-based consecutive attempts", semantic.attempts)
				}
				if index > 0 && semantic.timeouts[index] <= semantic.timeouts[index-1] {
					t.Fatalf("timeouts = %v, want each retry to get more time", semantic.timeouts)
				}
			}
			if got := len(assignments) == 1; got != tc.wantAssignment {
				t.Fatalf("assignments = %#v, want assignment %v", assignments, tc.wantAssignment)
			}
			reason, failed := coordinator.runInputResolutionFailure("review.scope")
			if tc.wantFailure == "" {
				if failed || len(warnings) != 0 {
					t.Fatalf("recorded failure %q and warnings %q, want none", reason, warnings)
				}
				return
			}
			if !failed || !strings.Contains(reason, tc.wantFailure) {
				t.Fatalf("recorded failure = %q, %v; want it to contain %q", reason, failed, tc.wantFailure)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], "--input review.scope=<json>") {
				t.Fatalf("warnings = %q, want one operator warning with the --input remedy", warnings)
			}
		})
	}
}

func TestRunInputResolutionNoticeNamesTheDefault(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	if got := coordinator.runInputResolutionNotice(); got != "" {
		t.Fatalf("notice without a failure = %q, want none", got)
	}
	if err := coordinator.mutateSessionData(func(session *SessionData) error {
		session.RunInputSnapshots = []RunInputSnapshot{{
			ID: "run-inputs-1",
			Inputs: []ResolvedRunInput{{
				Name: "review.scope", CanonicalValue: json.RawMessage(`{"count":10,"kind":"last_n"}`), Source: RunInputSourceDefault,
			}},
		}}
		session.ActiveRunInputSnapshotID = "run-inputs-1"
		return nil
	}); err != nil {
		t.Fatalf("mutateSessionData: %v", err)
	}
	coordinator.recordRunInputResolutionFailure("review.scope", errors.New("stream transport error: unexpected EOF"))

	notice := coordinator.runInputResolutionNotice()
	for _, want := range []string{"RUN INPUT DEFAULTED", `review.scope = {"count":10,"kind":"last_n"}`, "unexpected EOF", "--input review.scope=<json>"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice lacks %q:\n%s", want, notice)
		}
	}
	var prompt strings.Builder
	coordinator.appendCanonicalRunInputsPrompt(&prompt)
	if !strings.Contains(prompt.String(), "could not be translated") {
		t.Fatalf("coordinator prompt does not explain the failed translation:\n%s", prompt.String())
	}
	coordinator.resetRunInputResolutionFailures()
	if got := coordinator.runInputResolutionNotice(); got != "" {
		t.Fatalf("notice after reset = %q, want none", got)
	}
}

func TestCanonicalRunInputsPromptDisclosesDefaults(t *testing.T) {
	cases := []struct {
		name   string
		source RunInputSource
		want   bool
	}{
		{name: "default", source: RunInputSourceDefault, want: true},
		{name: "resolver", source: RunInputSourceResolver, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			if err := coordinator.mutateSessionData(func(session *SessionData) error {
				session.RunInputSnapshots = []RunInputSnapshot{{
					ID: "run-inputs-1",
					Inputs: []ResolvedRunInput{{
						Name: "review.scope", CanonicalValue: json.RawMessage(`{"count":10}`), Source: tc.source,
					}},
				}}
				session.ActiveRunInputSnapshotID = "run-inputs-1"
				return nil
			}); err != nil {
				t.Fatalf("mutateSessionData: %v", err)
			}
			var b strings.Builder
			coordinator.appendCanonicalRunInputsPrompt(&b)
			if got := strings.Contains(b.String(), "the team default applied"); got != tc.want {
				t.Fatalf("default disclosure present = %v, want %v:\n%s", got, tc.want, b.String())
			}
		})
	}
}
