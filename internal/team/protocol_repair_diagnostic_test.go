package team

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProtocolRepairFailureProjectsCurrentValidationEvidence(t *testing.T) {
	for _, test := range []struct {
		name           string
		class          TaskFailureClass
		repairSuccess  bool
		reason         RepairFailureReason
		wantSubmission bool
	}{
		{name: "failed schema repair", class: FailureProtocol, reason: RepairFailureInvalidSchema, wantSubmission: true},
		{name: "later execution failure", class: FailureExecution, reason: RepairFailureInvalidSchema},
		{name: "successful repair is historical", class: FailureProtocol, repairSuccess: true, reason: RepairFailureInvalidSchema},
		{name: "other repair failure", class: FailureProtocol, reason: RepairFailureNoToolCall},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Coordinator{taskTracker: NewTaskTracker(), projectDir: "/workspace"}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect and report"}})[0]
			c.setCurrentTaskAttempt(item.ID, 1)
			registry := c.executionStepReceiptRegistry()
			if err := registry.Record(ExecutionStepReceipt{
				ID: "read-rejection", TaskID: item.ID, Attempt: 1, StepID: "read", Tool: "view",
				InputSHA256: "read-input", StartedAt: time.Now(), ExitCode: 1, Stderr: "old UTF-8 read error",
			}); err != nil {
				t.Fatal(err)
			}
			item.ExecutionReceipt = &ExecutionReceipt{
				TaskID: item.ID, Attempt: 1,
				RepairProvenance: &RepairProvenance{
					Attempted: true, Success: test.repairSuccess, FailureReason: test.reason,
					History: []RepairAttemptProvenance{
						{Attempt: 1, FailureReason: RepairFailureInvalidSchema, Error: "older schema rejection"},
						{Attempt: 2, FailureReason: test.reason, Error: "/record/index: got string, want null; api_token=repair-secret"},
					},
				},
			}
			if test.wantSubmission {
				item.VerifyResult = &VerificationResult{Command: "old verifier", ExitCode: 1, Stderr: "old verifier failure"}
			}
			event := c.failureEventForItem(item, test.class, ReconcileOnly, "result finalization failed", FailureFingerprint{Digest: "stable-fingerprint"}, item.ID)
			if test.wantSubmission {
				if event.Command != submitResultToolName || event.Phase != "protocol" || event.FailureType != "validation" || event.ExitCode != nil || event.ReceiptID != "" || event.FailedStepID != "" {
					t.Fatalf("submission rejection attributed to a historical command: %#v", event)
				}
				if !strings.Contains(event.Stderr, "/record/index: got string, want null") || !strings.Contains(event.Hint, "/record/index") {
					t.Fatalf("latest rejection missing from recovery evidence: %#v", event)
				}
				text := RenderFailureText(event)
				for _, forbidden := range []string{"old UTF-8", "older schema", "old verifier", "repair-secret"} {
					if strings.Contains(text, forbidden) {
						t.Fatalf("failure projection contains %q: %s", forbidden, text)
					}
				}
			} else if event.Command != "view" || event.ReceiptID != "read-rejection" {
				t.Fatalf("unrelated failure lost its original causal evidence: %#v", event)
			}
			if event.Fingerprint != "stable-fingerprint" {
				t.Fatalf("diagnostic changed the stable failure fingerprint: %#v", event)
			}
			if original, ok := registry.FirstFailure(item.ID, 1); !ok || original.ID != "read-rejection" {
				t.Fatal("projection removed the historical execution receipt")
			}
		})
	}
}

func TestProtocolRetryContextRetainsRejectionWithoutToolInput(t *testing.T) {
	prompt := buildRetryContext(FailureProtocol, errors.New("result finalization failed"), "", "", -1, nil,
		submitResultToolName, "", "/record/index: got string, want null; api_token=repair-secret", true, "", TaskDef{})
	if !strings.Contains(prompt, "last tool call: submit_result (exit: error") || !strings.Contains(prompt, "/record/index: got string, want null") {
		t.Fatalf("retry omitted the available schema rejection: %s", prompt)
	}
	if strings.Contains(prompt, "repair-secret") {
		t.Fatalf("retry exposed an unredacted diagnostic: %s", prompt)
	}
}
