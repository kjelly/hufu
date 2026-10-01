package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// newPreflightCoordinator returns a coordinator whose frozen review.scope was
// resolved with empty base and since, as the 2026-09-27 run froze it, and a
// produce-workset task bound to that input.
func newPreflightCoordinator(t *testing.T, mode AcceptanceMode, verifications []VerificationSpec) (*Coordinator, string) {
	t.Helper()
	c := newBudgetCoordinator(t)
	c.sessionData = NewSession()
	prof, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	prof.AcceptanceMode = mode
	c.SetExecutionProfile(prof)
	c.acceptanceSpec = &AcceptanceSpec{Verifications: verifications}
	const valueHash = "sha256:frozen-scope"
	if err := c.mutateSessionData(func(session *SessionData) error {
		session.RunInputSnapshots = []RunInputSnapshot{{
			ID: "run-inputs-1",
			Inputs: []ResolvedRunInput{{
				Name:           "review.scope",
				CanonicalValue: json.RawMessage(`{"base":"","count":22,"head":"HEAD","history":"first_parent","kind":"last_n","since":""}`),
				ValueHash:      valueHash,
			}},
		}}
		session.ActiveRunInputSnapshotID = "run-inputs-1"
		return nil
	}); err != nil {
		t.Fatalf("mutateSessionData: %v", err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "produce workset"}})[0]
	tl := c.taskTracker.TodoList()
	tl.mu.Lock()
	for _, todo := range tl.items {
		if todo.ID == item.ID {
			todo.ContractID = "produce-workset"
			todo.BoundInputs = map[string]string{"review.scope": valueHash}
		}
	}
	tl.mu.Unlock()
	return c, item.ID
}

func scopeAssertion(assertions ...TaskOutputAssertion) VerificationSpec {
	return VerificationSpec{Type: VerifyTaskOutputAssert, WorksetSourceTask: "produce-workset", TaskOutputName: "scope", Assertions: assertions}
}

func TestPreflightActionAcceptance(t *testing.T) {
	// The producer echoes the scope with empty fields omitted.
	outputs := map[string]any{"scope": map[string]any{
		"requested": map[string]any{"count": json.Number("22"), "head": "HEAD", "history": "first_parent", "kind": "last_n"},
		"satisfied": true,
	}, "verbatim_scope": map[string]any{
		"requested": map[string]any{"base": "", "count": json.Number("22"), "head": "HEAD", "history": "first_parent", "kind": "last_n", "since": ""},
	}}
	equalsInput := TaskOutputAssertion{Pointer: "/requested", Op: "equals_input", Input: "review.scope"}
	satisfied := TaskOutputAssertion{Pointer: "/satisfied", Op: "equals", Value: true}
	cases := []struct {
		name          string
		mode          AcceptanceMode
		verifications []VerificationSpec
		wantErr       string
	}{
		{name: "frozen input mismatch fails early", mode: AcceptanceBlocking, verifications: []VerificationSpec{scopeAssertion(satisfied, equalsInput)}, wantErr: `pointer "/requested" did not satisfy equals_input`},
		{name: "passing assertions", mode: AcceptanceBlocking, verifications: []VerificationSpec{scopeAssertion(satisfied)}},
		{name: "echo that keeps empty fields matches", mode: AcceptanceBlocking, verifications: []VerificationSpec{func() VerificationSpec {
			spec := scopeAssertion(equalsInput)
			spec.TaskOutputName = "verbatim_scope"
			return spec
		}()}},
		{name: "advisory acceptance waits for finish", mode: AcceptanceAdvisory, verifications: []VerificationSpec{scopeAssertion(equalsInput)}},
		{name: "observation mode is not a gate", mode: AcceptanceBlocking, verifications: []VerificationSpec{func() VerificationSpec {
			spec := scopeAssertion(equalsInput)
			spec.Mode = "observation"
			return spec
		}()}},
		{name: "other source task", mode: AcceptanceBlocking, verifications: []VerificationSpec{func() VerificationSpec {
			spec := scopeAssertion(equalsInput)
			spec.WorksetSourceTask = "verify-targeted-go-tests"
			return spec
		}()}},
		{name: "missing output", mode: AcceptanceBlocking, verifications: []VerificationSpec{func() VerificationSpec {
			spec := scopeAssertion(satisfied)
			spec.TaskOutputName = "documentation_verification"
			return spec
		}()}, wantErr: `runtime output "documentation_verification" does not exist`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, todoID := newPreflightCoordinator(t, tc.mode, tc.verifications)
			err := c.preflightActionAcceptance(todoID, outputs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("preflightActionAcceptance: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "the run cannot succeed") {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func newDoneWorkflowCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	tmpDir := t.TempDir()
	session := &TeamSession{Workspace: tmpDir, Dir: tmpDir, Config: agent.TeamConfig{Name: "review-team", Acceptance: "exit 1"}}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	prof, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	prof.RequireEvidenceManifest = false
	c.SetExecutionProfile(prof)
	c.acceptanceCmd = "exit 1"
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhaseDone, results: map[Phase]PhaseResult{PhaseVerify: {Status: PhaseStatusSuccess}}}
	return c
}

func TestFinishEndsRunWhenAcceptanceCannotBeRepaired(t *testing.T) {
	c := newDoneWorkflowCoordinator(t)
	resp, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"review report"}`})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if resp.IsError {
		t.Fatalf("finish was refused although no work can change the result: %q", resp.Content)
	}
	for _, want := range []string{"FINISHED:review report", "ACCEPTANCE CHECK FAILED", "accepts no new tasks"} {
		if !strings.Contains(resp.Content, want) {
			t.Fatalf("finish response lacks %q:\n%s", want, resp.Content)
		}
	}
	if c.selfHealingAttempts != 0 {
		t.Fatalf("selfHealingAttempts = %d; a done workflow cannot run repair tasks", c.selfHealingAttempts)
	}
	if !c.finishCalled.Load() {
		t.Fatal("finishCalled = false after the terminal finish")
	}
	if rejection := c.finishRejectionSnapshot(); rejection.count != 0 {
		t.Fatalf("finish recorded %d rejection(s), want none", rejection.count)
	}
}

func TestFinishRecordsRejections(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "still running"}})
	resp, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"the coordinator's report"}`})
	if err != nil || !resp.IsError {
		t.Fatalf("finish with a pending task = %#v, %v; want a rejection", resp, err)
	}
	rejection := c.finishRejectionSnapshot()
	if rejection.count != 1 || !strings.Contains(rejection.reason, "cannot finish while worker tasks") || rejection.response != "the coordinator's report" {
		t.Fatalf("rejection = %+v", rejection)
	}
}

func TestDeterministicFinishTextNamesRejections(t *testing.T) {
	cases := []struct {
		name       string
		rejection  finishRejection
		wantReason string
		wantHeader []string
		notHeader  string
	}{
		{name: "finish never called", wantReason: "coordinator omitted finish", wantHeader: []string{"The coordinator did not call finish"}},
		{name: "finish rejected", rejection: finishRejection{count: 4, reason: "Acceptance check failed (blocking): pointer mismatch", response: "full coordinator report"},
			wantReason: "finish was rejected 4 time(s)",
			wantHeader: []string{"rejected the coordinator's finish call 4 time(s)", "pointer mismatch", "full coordinator report"},
			notHeader:  "did not call finish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if reason := deterministicFinishReason(tc.rejection); !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			header := completedTasksSummaryHeader(tc.rejection)
			for _, want := range tc.wantHeader {
				if !strings.Contains(header, want) {
					t.Fatalf("header lacks %q:\n%s", want, header)
				}
			}
			if tc.notHeader != "" && strings.Contains(header, tc.notHeader) {
				t.Fatalf("header claims %q:\n%s", tc.notHeader, header)
			}
		})
	}
}

func TestPreflightFailureEvidenceKeepsBoundedRedactedOutput(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		want    string
		notWant string
	}{
		{name: "test log", output: `{"targeted_go_tests":{"passed":false,"diagnostic":"--- FAIL: TestX"}}`, want: "--- FAIL: TestX"},
		{name: "secret", output: "api_token=abcdef123456 --- FAIL", want: "[REDACTED]", notWant: "abcdef123456"},
		{name: "oversized", output: strings.Repeat("x", maxPreflightEvidenceRunes+100), want: "action output: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := preflightFailureEvidence(tc.output)
			if !strings.Contains(got, tc.want) || (tc.notWant != "" && strings.Contains(got, tc.notWant)) {
				t.Fatalf("evidence = %.200q", got)
			}
			if runes := len([]rune(got)); runes > maxPreflightEvidenceRunes+len("action output: ")+3 {
				t.Fatalf("evidence has %d runes, want at most %d", runes, maxPreflightEvidenceRunes)
			}
		})
	}
}
