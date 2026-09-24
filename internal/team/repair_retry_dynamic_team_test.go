package team

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestPermitRepairRetryByTeamShape(t *testing.T) {
	dynamic, err := newRuntimeWorkflow(&TeamSession{Config: agent.TeamConfig{Name: "dynamic"}})
	if err != nil {
		t.Fatalf("newRuntimeWorkflow(dynamic): %v", err)
	}
	failFast := &runtimeWorkflow{enabled: true, retryState: NewRetryState(), policies: Policies{FailFast: true, MaxRetries: 3}}
	bounded := &runtimeWorkflow{enabled: true, retryState: NewRetryState(), policies: Policies{MaxRetries: 1}}
	unbounded := &runtimeWorkflow{enabled: true, retryState: NewRetryState()}

	task := TaskDef{ID: "verify", Agent: "verifier", MaxRetries: 1}
	validation := ActionValidationError{Capability: "diagnostics", Cause: errors.New("bad payload")}
	cases := []struct {
		name     string
		workflow *runtimeWorkflow
		task     TaskDef
		err      error
		want     bool
	}{
		{name: "dynamic team repairs an ordinary failure", workflow: dynamic, task: task, err: errors.New("deliverable verification failed"), want: true},
		{name: "dynamic team with no task retry budget still defers to the scheduler bound", workflow: dynamic, task: TaskDef{Agent: "verifier"}, err: errors.New("failed"), want: true},
		{name: "dynamic team never repairs a validation failure", workflow: dynamic, task: task, err: validation, want: false},
		{name: "dynamic team ignores a nil error", workflow: dynamic, task: task, err: nil, want: false},
		{name: "workflow team with fail_fast still refuses", workflow: failFast, task: task, err: errors.New("failed"), want: false},
		{name: "workflow team repairs within its signature bound", workflow: bounded, task: task, err: errors.New("failed"), want: true},
		{name: "workflow team with no configured or task limit refuses", workflow: unbounded, task: TaskDef{Agent: "verifier"}, err: errors.New("failed"), want: false},
		{name: "workflow team never repairs a validation failure", workflow: bounded, task: task, err: validation, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.workflow.permitRepairRetry(tc.task, tc.err); got != tc.want {
				t.Fatalf("permitRepairRetry = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPermitRepairRetryWorkflowSignatureBoundIsUnchanged pins the enabled
// workflow's failure-signature limit: the dynamic-team fix must not let a
// workflow team repair the same signature more often than its policy allows.
func TestPermitRepairRetryWorkflowSignatureBoundIsUnchanged(t *testing.T) {
	w := &runtimeWorkflow{enabled: true, retryState: NewRetryState(), policies: Policies{MaxRetries: 1}}
	task := TaskDef{ID: "verify", Agent: "verifier"}
	failure := errors.New("same failure")
	if !w.permitRepairRetry(task, failure) {
		t.Fatal("first repair of a signature within the bound was refused")
	}
	if w.permitRepairRetry(task, failure) {
		t.Fatal("second repair of the same signature exceeded the workflow bound")
	}
}
