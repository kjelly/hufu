package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
)

type rejectingResultSink struct{ err error }

func (s rejectingResultSink) Submit(context.Context, string, TaskResult) error { return s.err }

func TestLocalResultAdmissionPreservesToolBoundary(t *testing.T) {
	for _, scenario := range []struct {
		name           string
		status         string
		artifact       string
		receipt        bool
		stale          bool
		assertion      bool
		sinkFails      bool
		missingSummary bool
		wantReason     RepairFailureReason
	}{
		{name: "success", status: TaskResultStatusSuccess, artifact: "report.txt"},
		{name: "completed_with_gaps", status: TaskResultStatusCompletedWithGaps},
		{name: "honest_partial", status: TaskResultStatusPartial},
		{name: "honest_failed", status: TaskResultStatusFailed},
		{name: "honest_blocked", status: TaskResultStatusBlocked},
		{name: "missing_summary", status: TaskResultStatusSuccess, missingSummary: true, wantReason: RepairFailureInvalidSchema},
		{name: "invalid_status", status: "unknown", wantReason: RepairFailureInvalidSchema},
		{name: "missing_artifact", status: TaskResultStatusSuccess, artifact: "missing.txt", wantReason: RepairFailureContract},
		{name: "forged_receipt", status: TaskResultStatusSuccess, receipt: true, wantReason: RepairFailureSource},
		{name: "stale_occurrence", status: TaskResultStatusSuccess, artifact: "report.txt", stale: true, wantReason: RepairFailureSource},
		{name: "assertion_after_materialization", status: TaskResultStatusSuccess, artifact: "report.txt", assertion: true, wantReason: RepairFailureContract},
		{name: "sink_failure", status: TaskResultStatusSuccess, artifact: "report.txt", sinkFails: true, wantReason: RepairFailureCommit},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var admitted []*TaskResult
			for _, transport := range []string{"tool", "direct"} {
				t.Run(transport, func(t *testing.T) {
					c := newTestCoordinator(t)
					task := TaskDef{Execution: ExecutionContract{RequiresResult: true}}
					if scenario.assertion {
						task.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
							{Pointer: "/summary", Op: "equals", Value: "required summary"},
						}}
					}
					item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", Execution: task.Execution, VerifySpec: task.VerifySpec}})[0]
					if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
						t.Fatal(err)
					}
					ctx := occurrenceTestContext(c, item.ID, 1)
					if scenario.stale {
						c.setCurrentTaskAttempt(item.ID, 2)
					}
					var sink TaskResultSink
					if scenario.sinkFails {
						sink = rejectingResultSink{err: errors.New("storage unavailable")}
					}
					const report = "verified report\n"
					if err := os.WriteFile(filepath.Join(c.session.Workspace, "report.txt"), []byte(report), 0o600); err != nil {
						t.Fatal(err)
					}
					input := SubmitResultInput{Status: scenario.status, Summary: "review complete", Details: report}
					if scenario.missingSummary {
						input.Summary = ""
					}
					if scenario.artifact != "" {
						input.Artifacts = []SubmitArtifactInput{{Path: scenario.artifact, Type: "report"}}
					}
					if scenario.receipt {
						input.ReceiptIDs = []string{"fabricated"}
					}
					var reason RepairFailureReason
					if transport == "tool" {
						raw, err := json.Marshal(input)
						if err != nil {
							t.Fatal(err)
						}
						response, err := (&submitResultTool{coordinator: c, todoID: item.ID, sink: sink}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
						if err != nil {
							t.Fatal(err)
						}
						if response.IsError {
							reason = submissionFailureMetadata(response.Metadata)
							if reason == "" {
								t.Fatalf("unclassified rejection: %s", response.Content)
							}
						}
					} else {
						admission := localResultAdmission{coordinator: c, todoID: item.ID, sink: sink}
						if err := admission.admit(ctx, input, taskResultSubmissionContractForTask(task), nil); err != nil {
							reason = localResultRejectionReason(err)
						}
					}
					if reason != scenario.wantReason {
						t.Fatalf("reason=%s, want %s", reason, scenario.wantReason)
					}
					result := c.GetTaskResult(item.ID)
					if scenario.wantReason != "" {
						if result != nil || todoByID(t, c, item.ID).TypedResult != nil {
							t.Fatal("rejected claims published a result")
						}
						controller := c.occurrenceController(item.ID)
						controller.mu.Lock()
						reserved, pending := controller.reserved, len(controller.pending)
						controller.mu.Unlock()
						if reserved || pending != 0 {
							t.Fatal("rejected claims left a reservation or pending artifacts")
						}
						return
					}
					if result == nil || result.Source != "submitted" || result.Status != scenario.status || result.Attempt != 1 || result.Confidence != 1 {
						t.Fatalf("invalid canonical result: %+v", result)
					}
					if todoByID(t, c, item.ID).Status != TaskInProgress {
						t.Fatal("admission bypassed lifecycle completion")
					}
					if scenario.artifact != "" {
						sum := sha256.Sum256([]byte(report))
						if len(result.Artifacts) != 1 || result.Artifacts[0].SHA256 != hex.EncodeToString(sum[:]) || result.Artifacts[0].Bytes != int64(len(report)) {
							t.Fatalf("artifact was not snapshotted exactly: %+v", result.Artifacts)
						}
						// Store-local metadata differs across independent fixtures.
						// Compare the rest of the complete canonical handoff below.
						result.Artifacts = nil
					}
					admitted = append(admitted, result)
				})
			}
			if scenario.wantReason == "" && (len(admitted) != 2 || !reflect.DeepEqual(admitted[0], admitted[1])) {
				t.Fatalf("tool and direct canonical handoffs differ: %+v", admitted)
			}
		})
	}
}
