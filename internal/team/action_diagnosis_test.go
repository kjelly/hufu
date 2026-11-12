package team

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
)

func testActionDiagnosis() *ActionFailureDiagnosis {
	return &ActionFailureDiagnosis{Version: 1, Code: "capacity_exceeded", Summary: "Evidence cannot fit the declared budget", CauseStatus: "confirmed", Cause: "Fixed evidence metadata exceeds the per-item budget", Evidence: []ActionDiagnosticFact{{Name: "observed_bytes", Value: "411383"}, {Name: "limit_bytes", Value: "393216"}}, Suggestions: []string{"Refine evidence partitioning; unchanged input will fail again"}}
}

func diagnosticFailureWire(t *testing.T, d *ActionFailureDiagnosis) []byte {
	t.Helper()
	var envelope actionCommandFailure
	envelope.Error.Class, envelope.Error.Message, envelope.Error.Diagnosis = FailureExecution, "capacity exceeded", d
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestActionFailureDiagnosisWireBoundary(t *testing.T) {
	exitErr := exec.Command("/bin/sh", "-c", "exit 7").Run()
	valid := string(diagnosticFailureWire(t, testActionDiagnosis()))
	for _, tc := range []struct {
		name, wire string
		err        error
		want       bool
	}{
		{"valid", valid, exitErr, true},
		{"legacy", `{"error":{"failure_class":"execution","message":"legacy"}}`, exitErr, false},
		{"stderr prose", `not JSON`, exitErr, false},
		{"forged identity", strings.Replace(valid, `"version":1`, `"version":1,"task_id":"forged"`, 1), exitErr, false},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1), exitErr, false},
		{"trailing", valid + `{}`, exitErr, false},
		{"oversized", valid + strings.Repeat(" ", 16*1024), exitErr, false},
		{"unknown version", strings.Replace(valid, `"version":1`, `"version":2`, 1), exitErr, false},
		{"invalid claim", strings.Replace(valid, `"confirmed"`, `"certain"`, 1), exitErr, false},
		{"cancelled", valid, context.Canceled, false},
		{"timeout", valid, context.DeadlineExceeded, false},
		{"not started", valid, errors.New("failed to start"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := commandActionFailure(tc.err, []byte(tc.wire), "legacy stderr")
			if got := actionDiagnosisFromError(err); (got != nil) != tc.want {
				t.Fatalf("diagnosis=%#v error=%v", got, err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatal("lost process cause")
			}
			if tc.err == context.Canceled && classifyTaskFailure(err) != FailureCancelled {
				t.Fatal("diagnosis replaced cancellation")
			}
		})
	}
}

func TestGolangActionFailureRetainsStructuredDiagnosis(t *testing.T) {
	exitErr := exec.Command("/bin/sh", "-c", "exit 7").Run()
	for _, truncated := range []bool{false, true} {
		provider := &golangActionProvider{capability: "demo", program: golangruntime.Program{Source: "/team/action", Digest: "sha256:fixture"}, execute: func(context.Context, golangruntime.Program, []byte, []string, int, int) (golangruntime.Result, error) {
			return golangruntime.Result{Stdout: diagnosticFailureWire(t, testActionDiagnosis()), StdoutTruncated: truncated, Stderr: []byte("action failed")}, exitErr
		}}
		_, err := provider.Execute(t.Context(), Action{Capability: "demo", Type: "apply"})
		if d := actionDiagnosisFromError(err); (d != nil) == truncated {
			t.Fatalf("truncated=%t diagnosis=%#v err=%v", truncated, d, err)
		}
		if !errors.Is(err, exitErr) {
			t.Fatal("Go adapter lost exit evidence")
		}
	}
}

func TestActionDiagnosisIsDetachedRedactedAndBounded(t *testing.T) {
	original := testActionDiagnosis()
	original.Cause = "api_token=diagnosis-private-value \x1b[31m cause"
	original.Evidence = append(original.Evidence, ActionDiagnosticFact{Name: "api_key", Value: "standalone-private-value"})
	event := &FailureEventPayload{TaskID: "1", FailureClass: FailureExecution, RetryDisposition: RetryNone, Diagnosis: original}
	got := RedactedFailureEvent(event)
	text := RenderFailureText(got)
	if strings.Contains(text, "diagnosis-private-value") || strings.Contains(text, "standalone-private-value") || strings.ContainsRune(text, '\x1b') || !strings.Contains(text, "411383") || !strings.Contains(text, "provider-reported confirmed") {
		t.Fatalf("unsafe/lossy rendering: %s", text)
	}
	got.Diagnosis.Evidence[0].Value = "changed"
	if original.Evidence[0].Value != "411383" {
		t.Fatal("diagnosis aliases original")
	}
	if got.NextAction != RecoveryNextAction(RetryNone) {
		t.Fatal("provider advice changed recovery authority")
	}
	for _, edit := range []func(*ActionFailureDiagnosis){
		func(d *ActionFailureDiagnosis) { d.Summary = strings.Repeat("x", 501) },
		func(d *ActionFailureDiagnosis) { d.Evidence = make([]ActionDiagnosticFact, 17) },
		func(d *ActionFailureDiagnosis) { d.Suggestions = make([]string, 9) },
		func(d *ActionFailureDiagnosis) { d.Code = "Invalid Code" },
	} {
		d := testActionDiagnosis()
		edit(d)
		if NormalizedActionDiagnosis(d) != nil {
			t.Fatal("invalid diagnosis accepted")
		}
	}
}

func TestActionDiagnosisSurvivesFailurePersistenceAndReplay(t *testing.T) {
	c, store := failureEventCoordinator(t, "diagnostic-action", "sh")
	failureEventSurfaces(t, c)
	session := workflowTestSession(t)
	session.Workspace = c.session.Workspace
	session.ProviderRegistry = NewProviderRegistry()
	c.session.Agents["worker"].Generation.Model = ""
	c.SetEventJournal(eventStoreJournal{store: store})
	c.phaseWorkflow = executePhaseWorkflow(t, session)
	task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "worker", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
	// Exercise the command adapter and complete runtime action failure path.
	c.phaseWorkflow.registry.Register("structured-actions", &commandActionProvider{capability: "structured-actions", command: []string{"/bin/sh", "-c", `cat >/dev/null; printf '%s' "$HUFU_DIAG_FAILURE"; echo "secondary process evidence" >&2; exit 7`}})
	d := testActionDiagnosis()
	d.Cause = "fixed overhead; api_token=diagnosis-persist-secret"
	wire := strings.Replace(string(diagnosticFailureWire(t, d)), "[REDACTED]", "diagnosis-persist-secret", 1)
	t.Setenv("HUFU_DIAG_FAILURE", wire)
	_, err := c.executeRuntimeAction(t.Context(), task, item.ID)
	if err == nil {
		t.Fatal("action succeeded")
	}
	got := c.todoItemByID(item.ID)
	if got.FailureEvent == nil || got.FailureEvent.Diagnosis == nil || got.TypedResult != nil || got.Status != TaskError {
		t.Fatalf("failed task=%#v", got)
	}
	receipt := readRuntimeActionReceipt(t, c.session.Workspace)
	if receipt.Status != "failure" || receipt.Diagnosis == nil || got.FailureEvent.ReceiptID != receipt.ActionID || receipt.Attempt != 1 || receipt.ExitCode == nil || *receipt.ExitCode != 7 || len(receipt.Diagnostics) != 1 || !strings.Contains(receipt.Diagnostics[0], "secondary process evidence") {
		t.Fatalf("unbound diagnosis receipt=%#v failure=%#v", receipt, got.FailureEvent)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 || replayed[0].FailureEvent == nil || replayed[0].FailureEvent.Diagnosis == nil {
		t.Fatalf("replay=%#v", replayed)
	}
	want, _ := json.Marshal(got.FailureEvent.Diagnosis)
	for name, value := range map[string]any{"receipt": receipt.Diagnosis, "replay": replayed[0].FailureEvent.Diagnosis, "runtime_error": got.RuntimeError.Diagnosis} {
		data, _ := json.Marshal(value)
		if string(data) != string(want) {
			t.Fatalf("%s diagnosis drift: %s != %s", name, data, want)
		}
	}
	found := false
	for _, e := range events {
		if e.Type == "action_failed" {
			var payload LifecycleEventPayload
			if err = json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			found = payload.ActionDiagnosis != nil
		}
	}
	if !found {
		t.Fatal("action lifecycle lost diagnosis")
	}
	assertFailureEventSurfacesRedacted(t, c, "diagnosis-persist-secret", "capacity_exceeded", "411383")
	// Session/task JSON and sparse legacy events retain the explanation, without
	// granting a disposition from provider suggestions.
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var restored TodoItem
	if err = json.Unmarshal(data, &restored); err != nil || restored.FailureEvent.Diagnosis == nil {
		t.Fatalf("session roundtrip: %v", err)
	}
	merged, ok := mergeFailureEventJSON(restored.FailureEvent, []byte(`{"failure_class":"execution","summary":"legacy update"}`))
	if !ok || merged.Diagnosis == nil || merged.RetryDisposition != RetryNone {
		t.Fatalf("sparse update=%#v", merged)
	}
	// Start a new occurrence; no stale provider diagnosis may survive success.
	c.phaseWorkflow.registry.Register("structured-actions", &commandActionProvider{capability: "structured-actions", command: []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"outputs":{"ok":true}}'`}})
	if err = c.commitTaskResetForRetry(t.Context(), item.ID, "retry", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = c.executeRuntimeAction(t.Context(), task, item.ID); err != nil {
		t.Fatal(err)
	}
	got = c.todoItemByID(item.ID)
	if got.RuntimeError != nil || got.FailureEvent != nil || got.Status != TaskDone {
		t.Fatalf("success retains failure: %#v", got)
	}
}
