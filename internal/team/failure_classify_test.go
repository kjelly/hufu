package team

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyTaskFailureStructured(t *testing.T) {
	commandNotFound := environmentFindingsFromVerifyResult(&VerificationResult{
		ExitCode: 127,
		Stderr:   "bash: nonexistent-cmd: command not found",
	})
	if len(commandNotFound) == 0 {
		t.Fatal("expected an environment finding from a command-not-found verify result")
	}
	verifyExit := func(code int) FailureClassificationInput {
		return FailureClassificationInput{Err: errors.New("agent failed: exit code 1"), ExitCode: code, ExitCodeSource: ExitCodeSourceVerify}
	}

	tests := []struct {
		name string
		in   FailureClassificationInput
		want TaskFailureClass
	}{
		// Structured cancellation beats every other signal.
		{"context.Canceled", FailureClassificationInput{Err: context.Canceled}, FailureCancelled},
		{"context.Canceled via ContextErr", FailureClassificationInput{Err: errors.New("agent stopped"), ContextErr: context.Canceled}, FailureCancelled},
		{"wrapped context.Canceled", FailureClassificationInput{Err: fmt.Errorf("worker: %w", context.Canceled)}, FailureCancelled},
		{"wrapped context.Canceled with timeout-like text", FailureClassificationInput{Err: fmt.Errorf("context canceled while waiting for deadline: %w", context.Canceled)}, FailureCancelled},
		{"deadline exceeded via ContextErr", FailureClassificationInput{Err: errors.New("agent stopped"), ContextErr: context.DeadlineExceeded}, FailureTimeout},

		// Resolve findings: an error-severity finding wins over text.
		{
			name: "executable_unresolved finding",
			in: FailureClassificationInput{Err: errors.New("command not found"), ResolveFindings: []ContractFinding{
				{Severity: FindingSeverityError, Code: FindingExecutableUnresolved, Field: "verify", Message: "missing binary"},
			}},
			want: FailureEnvironment,
		},
		{
			name: "other error finding",
			in: FailureClassificationInput{Err: errors.New("verifier malformed"), ResolveFindings: []ContractFinding{
				{Severity: FindingSeverityError, Code: FindingVerifierNotAsserting, Field: "verify", Message: "tail || echo"},
			}},
			want: FailureContract,
		},
		{
			name: "warning finding does not override text",
			in: FailureClassificationInput{Err: errors.New("verification failed: deliverable missing"), ResolveFindings: []ContractFinding{
				{Severity: FindingSeverityWarning, Code: FindingExecutableUnresolved, Field: "verify"},
			}},
			want: FailureVerify,
		},
		{
			name: "environment finding beats verify exit code",
			in: FailureClassificationInput{
				Err:             errors.New("deliverable verification failed: exit code 127"),
				ExitCode:        127,
				ExitCodeSource:  ExitCodeSourceVerify,
				ResolveFindings: commandNotFound,
			},
			want: FailureEnvironment,
		},

		// source= details.
		{"source=environment", FailureClassificationInput{Err: errors.New("source=environment | error=command not found")}, FailureEnvironment},
		{"source=cancelled", FailureClassificationInput{Err: errors.New("source=cancelled | error=user sigint")}, FailureCancelled},
		{"source=sigint", FailureClassificationInput{Err: errors.New("source=sigint | error=user sigint")}, FailureCancelled},
		{"source=context_canceled", FailureClassificationInput{Err: errors.New("source=context_canceled | error=parent cancel")}, FailureCancelled},
		{"source=contract", FailureClassificationInput{Err: errors.New("source=contract | error=verifier_not_asserting")}, FailureContract},
		{"source=contract with verifier detail", FailureClassificationInput{Err: errors.New("source=contract | error=verifier contract error: verify (verifier_not_asserting): ...")}, FailureContract},

		// Text fallback.
		{"text: command not found", FailureClassificationInput{Err: errors.New("bash: foo: command not found")}, FailureEnvironment},
		{"text: executable unresolved", FailureClassificationInput{Err: errors.New("executable unresolved in verify stage")}, FailureEnvironment},
		{"text: environment", FailureClassificationInput{Err: errors.New("environment setup failed")}, FailureEnvironment},
		{"text: contract preflight", FailureClassificationInput{Err: errors.New("contract preflight failed: verify (verifier_invalid): missing path")}, FailureContract},
		{"text: verification failed", FailureClassificationInput{Err: errors.New("verification failed: deliverable missing")}, FailureVerify},
		{"text: deadline exceeded", FailureClassificationInput{Err: errors.New("context deadline exceeded")}, FailureTimeout},
		{"text: execution fallback", FailureClassificationInput{Err: errors.New("agent failed: exit code 1")}, FailureExecution},
		{"nil error defaults to execution", FailureClassificationInput{}, FailureExecution},

		// Verify exit codes override execution-like text only when non-zero
		// and sourced from the verifier.
		{"verify exit 2", verifyExit(2), FailureVerify},
		{"verify exit 1", verifyExit(1), FailureVerify},
		{"verify exit 0 is ambiguous", verifyExit(0), FailureExecution},
		{"verify exit unknown", verifyExit(-1), FailureExecution},
		{"worker exit code does not override", FailureClassificationInput{Err: errors.New("agent failed: exit code 1"), ExitCode: 1, ExitCodeSource: ExitCodeSourceWorker}, FailureExecution},
		{"unspecified exit source does not override", FailureClassificationInput{Err: errors.New("agent failed: exit code 1"), ExitCode: 1}, FailureExecution},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyTaskFailureStructured(tt.in); got != tt.want {
				t.Errorf("ClassifyTaskFailureStructured() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyTaskFailureLegacyWrapper(t *testing.T) {
	tests := []struct {
		err  error
		want TaskFailureClass
	}{
		{errors.New("contract preflight failed: verify"), FailureContract},
		{errors.New("protocol failure: empty output"), FailureProtocol},
		{errors.New("verification failed"), FailureVerify},
		{errors.New("policy blocked"), FailurePolicy},
		{errors.New("context deadline exceeded"), FailureTimeout},
		{errors.New("command not found"), FailureEnvironment},
		{errors.New("agent failed: exit 1"), FailureExecution},
		{context.Canceled, FailureCancelled},
	}
	for _, tt := range tests {
		if got := classifyTaskFailure(tt.err); got != tt.want {
			t.Errorf("classifyTaskFailure(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestIsCancelledClass(t *testing.T) {
	for class, want := range map[TaskFailureClass]bool{FailureCancelled: true, FailureExecution: false, FailureTimeout: false} {
		if got := IsCancelledClass(class); got != want {
			t.Errorf("IsCancelledClass(%s) = %v, want %v", class, got, want)
		}
	}
}

func TestEnvironmentFindingsFromVerifyResult(t *testing.T) {
	// Only unambiguous shell or executable diagnostics count. Generic "not
	// found" phrases appear in ordinary missing-artifact verifier output and
	// must stay verification failures.
	tests := []struct {
		name string
		vr   *VerificationResult
		want bool
	}{
		{"stderr command not found", &VerificationResult{ExitCode: 127, Stderr: "bash: nonexistent-cmd: command not found"}, true},
		{"executable file not found", &VerificationResult{ExitCode: 127, Stderr: "executable file not found in $PATH"}, true},
		{"executable unresolved", &VerificationResult{ExitCode: 127, Stderr: "verify stage executable unresolved"}, true},
		{"artifact not found assertion", &VerificationResult{ExitCode: 1, Stderr: "assertion failed: artifact not found"}, false},
		{"grep missing input file", &VerificationResult{ExitCode: 2, Stderr: "grep: report.json: No such file or directory"}, false},
		{"sh missing path exec", &VerificationResult{ExitCode: 127, Stderr: "sh: /missing/path: No such file or directory"}, false},
		{"plain not found in stdout", &VerificationResult{ExitCode: 1, Stdout: "target not found"}, false},
		{"plain assertion failure", &VerificationResult{ExitCode: 1, Stderr: "assertion failed: expected file to exist"}, false},
		{"nil result", nil, false},
		{"empty output", &VerificationResult{ExitCode: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := environmentFindingsFromVerifyResult(tt.vr)
			got := len(findings) > 0 && findings[0].Code == FindingExecutableUnresolved
			if got != tt.want {
				t.Errorf("environment finding = %v, want %v (findings=%#v)", got, tt.want, findings)
			}
		})
	}
}

func TestVerifyResultForTodo(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	todoID := addTodo(t, c, "do work")

	check := func(name string, c *Coordinator, id string, wantExit int, wantNil bool) {
		t.Helper()
		vr := verifyResultForTodo(c, id)
		if (vr == nil) != wantNil {
			t.Errorf("%s: verifyResultForTodo = %v, want nil=%v", name, vr, wantNil)
		}
		if got := exitCodeFromVerifyResult(vr); got != wantExit {
			t.Errorf("%s: exit code = %d, want %d", name, got, wantExit)
		}
	}
	check("no result stored", c, todoID, -1, true)
	if err := c.taskTracker.TodoList().SetVerificationResult(todoID, &VerificationResult{ExitCode: 2}); err != nil {
		t.Fatalf("SetVerificationResult: %v", err)
	}
	check("stored result", c, todoID, 2, false)
	check("unknown todo", c, "nonexistent", -1, true)
	check("nil coordinator", nil, todoID, -1, true)
}
