package team

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func TestPermissionDispositionSurvivesFailurePersistence(t *testing.T) {
	c := newTestCoordinator(t)
	for _, tt := range []struct {
		name        string
		disposition ToolExecutionDisposition
		want        bool
	}{
		{name: "operating system error"},
		{name: "permission", disposition: ToolExecutionDisposition{Kind: ToolExecutionPolicyDenied, ReasonCode: "tool_permission_denied"}, want: true},
		{name: "guard", disposition: ToolExecutionDisposition{Kind: ToolExecutionGuardDenied}, want: true},
		{name: "executed", disposition: ToolExecutionDisposition{Kind: ToolExecutionPolicyDenied, ReasonCode: "tool_permission_denied", Executed: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cause := errors.New("permission denied | permission_blocked=true")
			err := withAttemptDenials(cause, []ToolExecutionDisposition{tt.disposition})
			if !errors.Is(err, cause) {
				t.Fatal("lost underlying error")
			}
			detail := c.FailureDetail(fmt.Errorf("worker: %w", err), FailureSourceError)
			bound := bindTaskFailureDetail(detail, "worker", "", "1", 1)
			if got := isPermissionBlockedFailureDetail(bound); got != tt.want {
				t.Fatalf("permission gate = %v, want %v: %s", got, tt.want, bound)
			}
			if tt.want && classifyTaskFailure(err) != FailurePolicy {
				t.Fatal("native denial must classify as policy")
			}
		})
	}
}

func TestNativeSafetyDenialStopsRetry(t *testing.T) {
	for _, tc := range []struct {
		code  string
		calls int
	}{
		{code: "path_out_of_scope", calls: 1},
		{code: "read_only_command_denied", calls: 1},
		{code: "", calls: 2},
	} {
		t.Run(tc.code, func(t *testing.T) {
			worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				if tc.code != "" {
					tools.ReportToolExecutionDisposition(ctx, tools.ToolExecutionDisposition{Kind: "policy_denied", ReasonCode: tc.code, ToolName: "view", ToolCallID: "denied"})
				}
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "partial evidence"}}}}, errors.New("path is outside allowed paths")
			}})
			c := newTestCoordinator(t, withWorker(worker), withMaxRetries(2))
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect"}})[0]
			_, err := c.executeTask(t.Context(), TaskDef{Agent: "worker", Goal: "inspect", Recovery: RecoveryRetry, SideEffect: SideEffectNone}, item.ID)
			if err == nil {
				t.Fatal("failed task was accepted")
			}
			if worker.Calls() != tc.calls {
				t.Fatalf("calls = %d, want %d", worker.Calls(), tc.calls)
			}
		})
	}
}

func TestDenialMetadataPreservesFailureClassification(t *testing.T) {
	cases := []struct {
		name  string
		input FailureClassificationInput
		want  TaskFailureClass
	}{
		{name: "verifier", input: FailureClassificationInput{Err: errors.New("command failed"), ExitCode: 2, ExitCodeSource: ExitCodeSourceVerify}, want: FailureVerify},
		{name: "typed protocol", input: FailureClassificationInput{Err: withFailureClassOverride(errors.New("invalid handoff"), FailureProtocol)}, want: FailureProtocol},
		{name: "environment", input: FailureClassificationInput{Err: errors.New("unavailable"), ResolveFindings: []ContractFinding{{Code: FindingExecutableUnresolved, Severity: FindingSeverityError}}}, want: FailureEnvironment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.input.Err = withAttemptDenials(tc.input.Err, nil)
			if got := ClassifyTaskFailureStructured(tc.input); got != tc.want {
				t.Fatalf("class = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestReadOnlyPolicyRepairRequiresOnlyUnexecutedGrammarDenials(t *testing.T) {
	grammar := ToolExecutionDisposition{Kind: ToolExecutionPolicyDenied, ReasonCode: "read_only_shell_syntax_denied"}
	for _, tc := range []struct {
		name    string
		denials []ToolExecutionDisposition
		want    bool
	}{
		{name: "grammar", denials: []ToolExecutionDisposition{grammar}, want: true},
		{name: "missing evidence"},
		{name: "path consent", denials: []ToolExecutionDisposition{grammar, {Kind: ToolExecutionPolicyDenied, ReasonCode: "path_out_of_scope"}}},
		{name: "permission", denials: []ToolExecutionDisposition{{Kind: ToolExecutionPolicyDenied, ReasonCode: "tool_permission_denied"}}},
		{name: "already executed", denials: []ToolExecutionDisposition{{Kind: ToolExecutionPolicyDenied, ReasonCode: grammar.ReasonCode, Executed: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyReadOnlyPolicyDenials(tc.denials); got != tc.want {
				t.Fatalf("repair allowed = %v, want %v", got, tc.want)
			}
		})
	}
}
