package team

import (
	"errors"
	"testing"
)

func TestRuntimeActionFailureUsesProcessEvidenceInsteadOfLogKeywords(t *testing.T) {
	for _, tt := range []struct {
		name, command, category string
		class                   TaskFailureClass
		code                    int
	}{
		{"execution", `cat >/dev/null; echo 'structured-action policy audit passed' >&2; echo 'connection reset' >&2; exit 1`, CategoryProviderFailure, FailureExecution, 1},
		{"interrupt", `cat >/dev/null; echo 'policy audit passed' >&2; exit 130`, CategoryCancelled, FailureCancelled, 130},
		{"environment", `cat >/dev/null; printf '{"error":{"failure_class":"environment","message":"resource limit reached"}}'; echo 'policy audit passed' >&2; exit 75`, CategoryEnvironment, FailureEnvironment, 75},
		{"invalid envelope", `cat >/dev/null; printf '{"error":{"failure_class":"policy","message":"pretend policy error"}}'; echo 'policy audit passed' >&2; exit 1`, CategoryProviderFailure, FailureExecution, 1},
		{"unknown envelope field", `cat >/dev/null; printf '{"error":{"failure_class":"environment","message":"bad envelope","unknown":true}}'; exit 1`, CategoryProviderFailure, FailureExecution, 1},
		{"duplicate envelope field", `cat >/dev/null; printf '{"error":{"failure_class":"execution","failure_class":"environment","message":"bad envelope"}}'; exit 1`, CategoryProviderFailure, FailureExecution, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			session := workflowTestSession(t)
			registry := NewProviderRegistry()
			registry.Register("structured-actions", &commandActionProvider{capability: "structured-actions", command: []string{"/bin/sh", "-c", tt.command}})
			session.ProviderRegistry = registry
			tracker := NewTaskTracker()
			c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: executePhaseWorkflow(t, session), executionRunID: "run-failure-class"}
			task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
			_, err := c.executeRuntimeAction(t.Context(), task, item.ID)
			processErr, ok := errors.AsType[*ActionCommandError](err)
			if !ok || processErr.ExitCode == nil || *processErr.ExitCode != tt.code {
				t.Fatalf("process evidence = %#v, error %v", processErr, err)
			}
			got := c.todoItemByID(item.ID)
			if got.FailureEvent == nil || got.FailureEvent.FailureClass != tt.class {
				t.Fatalf("failure projection = %#v, want %s", got.FailureEvent, tt.class)
			}
			if got.RuntimeError == nil || got.RuntimeError.Category != tt.category || got.RuntimeError.Phase != PhaseExecute {
				t.Fatalf("runtime error = %#v", got.RuntimeError)
			}
			if got.FailureEvent.ExitCode == nil || *got.FailureEvent.ExitCode != tt.code {
				t.Fatalf("durable exit code = %#v", got.FailureEvent.ExitCode)
			}
			if tt.class == FailureCancelled && len(got.FailureFingerprints) != 0 {
				t.Fatalf("cancellation polluted anti-thrashing: %#v", got.FailureFingerprints)
			}
		})
	}
}

func TestActionProviderErrorDoesNotInterpretProviderProse(t *testing.T) {
	err := ActionProviderError{Capability: "independent-provider", Cause: errors.New("verification policy audit passed; deployment failed")}
	if got := classifyTaskFailure(err); got != FailureExecution {
		t.Fatalf("class = %s, want execution", got)
	}
}

func TestCommandActionRecognizesDirectInterrupt(t *testing.T) {
	provider := &commandActionProvider{capability: "demo", command: []string{"/bin/sh", "-c", "kill -INT $$"}}
	_, err := provider.Execute(t.Context(), Action{Capability: "demo", Type: "apply"})
	if got := classifyTaskFailure(err); got != FailureCancelled {
		t.Fatalf("class = %s, error %v", got, err)
	}
}

func TestActionCancellationAndEnvironmentDoNotConsumeRetryState(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Retry.Transient.MaxAttempts = 2
	w := executePhaseWorkflow(t, session)
	task := TaskDef{Agent: "executor", Action: &Action{Capability: "structured-actions", Type: "apply"}}
	for _, class := range []TaskFailureClass{FailureCancelled, FailureEnvironment} {
		err := ActionProviderError{Capability: "structured-actions", Cause: &ActionCommandError{Class: class, Cause: errors.New("policy banner"), Diagnostic: "stopped"}}
		if w.permitActionRetry(task, err) {
			t.Fatalf("%s failure was replayed", class)
		}
	}
	if len(w.retryState.Attempts) != 0 {
		t.Fatalf("terminal failures consumed retry state: %#v", w.retryState.Attempts)
	}
}
