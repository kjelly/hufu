package team

import (
	"reflect"
	"strings"
	"testing"
)

func taskResultIsolationFixture(t *testing.T) (*Coordinator, *TodoItem, *TaskResult) {
	t.Helper()
	c, item := newOccurrenceTransactionCoordinator(t)
	c.setCurrentTaskAttempt(item.ID, 1)
	result := &TaskResult{
		TaskID: item.ID, Attempt: 1, Agent: item.Agent, Status: TaskResultStatusSuccess,
		Summary: "original", Source: "submitted",
		RawOutputRef: &ArtifactRef{ID: "raw-original", Path: "raw.jsonl"},
		Outputs: map[string]StructuredOutputValue{
			"artifact": {Kind: ExecutionOutputArtifact, Artifact: &ArtifactRef{ID: "artifact-original"}},
			"fact": {
				Kind: ExecutionOutputFact,
				Fact: &StructuredFact{Name: "fact", Value: map[string]any{
					"nested": []any{map[string]any{"value": "original"}},
				}},
			},
		},
		Facts: map[string]any{"json": map[string]any{"items": []any{"original"}}},
		Verification: []VerificationResult{{Spec: &VerificationSpec{
			Assertions: []JSONAssertion{{Path: "/json", Equals: map[string]any{"expected": "original"}}},
		}}},
	}
	c.storeSubmittedTaskResult(item.ID, result)
	return c, item, result
}

func TestGetTaskResultDeepIsolationPreservesCanonicalAndProjections(t *testing.T) {
	c, item, _ := taskResultIsolationFixture(t)
	before := c.GetTaskResult(item.ID)
	if before == nil {
		t.Fatal("missing canonical result")
	}

	snapshot := c.GetTaskResult(item.ID)
	snapshot.RawOutputRef.ID = "mutated-raw"
	snapshot.Outputs["artifact"].Artifact.ID = "mutated-artifact"
	snapshot.Outputs["fact"].Fact.Value.(map[string]any)["nested"].([]any)[0].(map[string]any)["value"] = "mutated-fact"
	snapshot.Facts["json"].(map[string]any)["items"].([]any)[0] = "mutated-json"
	snapshot.Verification[0].Spec.Assertions[0].Equals.(map[string]any)["expected"] = "mutated-assertion"

	if got := c.GetTaskResult(item.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("canonical controller result changed through snapshot: got=%#v before=%#v", got, before)
	}
	c.taskResultsMu.RLock()
	stored := c.taskResults[item.ID]
	c.taskResultsMu.RUnlock()
	if !reflect.DeepEqual(stored, before) {
		t.Fatalf("taskResults projection changed through snapshot: got=%#v before=%#v", stored, before)
	}
	projected := c.taskTracker.TodoList().Items()[0].TypedResult
	if !reflect.DeepEqual(projected, before) {
		t.Fatalf("TodoItem.TypedResult projection changed through snapshot: got=%#v before=%#v", projected, before)
	}
}

func newTranscriptForOccurrenceTest(t *testing.T, c *Coordinator, item *TodoItem, attempt int) *taskTranscript {
	t.Helper()
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := transcript.RecordAssistantOutput("authoritative transcript"); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func TestRejectedTranscriptFinalizationDoesNotMutateExistingResult(t *testing.T) {
	c, item, _ := taskResultIsolationFixture(t)
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("missing active occurrence")
	}
	before := c.GetTaskResult(item.ID)
	transcript := newTranscriptForOccurrenceTest(t, c, item, identity.Attempt)
	contract := taskResultSubmissionContractForTask(TaskDef{
		OutputMode: TaskOutputModeVerbatim,
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{
			Pointer: "/raw_output_ref/id", Op: "equals", Value: "not-the-sealed-transcript",
		}}},
	})

	if _, err := c.finalizeTaskResultOccurrence(identity, transcript, contract); err == nil || !strings.Contains(err.Error(), "transcript finalization") {
		t.Fatalf("rejected finalization error=%v, want transcript assertion failure", err)
	}
	if got := c.GetTaskResult(item.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected finalization changed canonical result: got=%#v before=%#v", got, before)
	}
	if got := c.taskTracker.TodoList().Items()[0].TypedResult; !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected finalization changed TodoItem projection: got=%#v before=%#v", got, before)
	}
}

func TestStaleTranscriptFinalizationCannotAlterCurrentOccurrence(t *testing.T) {
	c, item, _ := taskResultIsolationFixture(t)
	oldIdentity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("missing first occurrence")
	}
	c.setCurrentTaskAttempt(item.ID, 2)
	current := &TaskResult{TaskID: item.ID, Attempt: 2, Agent: item.Agent, Status: TaskResultStatusSuccess, Summary: "current", Source: "submitted"}
	c.storeSubmittedTaskResult(item.ID, current)
	before := c.GetTaskResult(item.ID)
	oldTranscript := newTranscriptForOccurrenceTest(t, c, item, oldIdentity.Attempt)
	_, err := c.finalizeTaskResultOccurrence(oldIdentity, oldTranscript, taskResultSubmissionContract{})
	if err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("stale finalization error=%v, want provenance rejection", err)
	}
	if got := c.GetTaskResult(item.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("stale finalization changed current occurrence: got=%#v before=%#v", got, before)
	}
}

func TestSuccessfulTranscriptFinalizationPreservesOutputsAndAddsBoundEvidence(t *testing.T) {
	c, item, _ := taskResultIsolationFixture(t)
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("missing active occurrence")
	}
	transcript := newTranscriptForOccurrenceTest(t, c, item, identity.Attempt)
	before := c.GetTaskResult(item.ID)
	output, err := c.finalizeTaskResultOccurrence(identity, transcript, taskResultSubmissionContract{})
	if err != nil {
		t.Fatalf("successful finalization: %v", err)
	}
	if !strings.Contains(output, "artifact_ref=") {
		t.Fatalf("finalization output=%q, want opaque artifact reference", output)
	}
	got := c.GetTaskResult(item.ID)
	if got == nil || !reflect.DeepEqual(got.Outputs["artifact"], before.Outputs["artifact"]) || !reflect.DeepEqual(got.Outputs["fact"], before.Outputs["fact"]) {
		t.Fatalf("preexisting outputs were not preserved: before=%#v after=%#v", before.Outputs, got)
	}
	if len(got.Outputs) != len(before.Outputs)+1 {
		t.Fatalf("outputs=%d, want exactly one new transcript output over %d existing outputs", len(got.Outputs), len(before.Outputs))
	}
	transcriptOutput, ok := got.Outputs[rawTranscriptOutputName]
	if !ok || transcriptOutput.Artifact == nil || got.RawOutputRef == nil || transcriptOutput.Artifact.ID != got.RawOutputRef.ID {
		t.Fatalf("transcript output=%#v raw_output_ref=%#v", transcriptOutput, got.RawOutputRef)
	}
	if len(got.Evidence) != len(before.Evidence)+1 {
		t.Fatalf("evidence=%d, want exactly one new transcript evidence entry", len(got.Evidence))
	}
	last := got.Evidence[len(got.Evidence)-1]
	if last.Type != "task_transcript" || last.Value != got.RawOutputRef.ID || last.SystemHMAC == "" {
		t.Fatalf("transcript evidence=%#v", last)
	}
	projected := c.taskTracker.TodoList().Items()[0].TypedResult
	if projected == nil || projected.Outputs[rawTranscriptOutputName].Artifact == nil || len(projected.Evidence) != len(got.Evidence) {
		t.Fatalf("TodoItem projection did not receive committed transcript result: %#v", projected)
	}
}
