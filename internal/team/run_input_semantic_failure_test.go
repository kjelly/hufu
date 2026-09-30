package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptedSemanticRunInputResolver returns its results in order and repeats
// the last one.
type scriptedSemanticRunInputResolver struct {
	results []scriptedSemanticResult
	calls   int
}

type scriptedSemanticResult struct {
	value json.RawMessage
	err   error
}

func (r *scriptedSemanticRunInputResolver) Resolve(context.Context, SemanticRunInputRequest) (json.RawMessage, error) {
	result := r.results[min(r.calls, len(r.results)-1)]
	r.calls++
	if result.err != nil {
		return nil, result.err
	}
	return json.RawMessage(string(result.value)), nil
}

func TestSemanticRunInputFailureDoesNotSilentlyApplyDefault(t *testing.T) {
	transportErr := errors.New("semantic input translation: stream transport error: unexpected EOF")
	malformedErr := errors.New("semantic input response is not one JSON value: unexpected EOF")
	lastN := json.RawMessage(`{"kind":"last_n","count":22,"history":"first_parent","head":"HEAD"}`)
	cases := []struct {
		name           string
		results        []scriptedSemanticResult
		explicit       json.RawMessage
		wantErr        string
		wantAssignment bool
		wantCalls      int
	}{
		{name: "transport failure twice", results: []scriptedSemanticResult{{err: transportErr}}, wantErr: "input_resolver_failed: review.scope", wantCalls: 2},
		{name: "malformed answer twice", results: []scriptedSemanticResult{{err: malformedErr}}, wantErr: "--input review.scope=<json>", wantCalls: 2},
		{name: "failure then success", results: []scriptedSemanticResult{{err: transportErr}, {value: lastN}}, wantAssignment: true, wantCalls: 2},
		{name: "resolver unavailable keeps the default", results: []scriptedSemanticResult{{err: errSemanticRunInputUnavailable}}, wantCalls: 1},
		{name: "null answer keeps the default", results: []scriptedSemanticResult{{value: json.RawMessage(`null`)}}, wantCalls: 1},
		{name: "explicit value wins over a failure", results: []scriptedSemanticResult{{err: transportErr}}, explicit: lastN, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
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
			if semantic.calls != tc.wantCalls {
				t.Fatalf("semantic resolver calls = %d, want %d", semantic.calls, tc.wantCalls)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRunInputCandidates: %v", err)
			}
			if got := len(assignments) == 1; got != tc.wantAssignment {
				t.Fatalf("assignments = %#v, want assignment %v", assignments, tc.wantAssignment)
			}
		})
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
