package team

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func verifiedResolutionFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem) {
	t.Helper()
	compiled, ref := compiledReviewContract(t, true)
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	c.executionRunID = "run-resolution"
	c.taskTracker.TodoList().SetRunID(c.executionRunID)
	spec := &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/structured_payload/value/verdict", Op: "equals", Value: "approve"},
	}}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "original review", ContractID: "review", ContractHash: "frozen", ResultContract: &ref, VerifySpec: spec, Execution: ExecutionContract{RequiresResult: true}},
		{Agent: "worker", Desc: "replacement review", ContractID: "review", ContractHash: "frozen", ResultContract: &ref, VerifySpec: spec, Execution: ExecutionContract{RequiresResult: true}},
	})
	original, replacement := items[0], items[1]
	c.taskTracker.TodoList().UpdateStatus(original.ID, TaskBlocked, "schema repair failed")
	original.FailureEvent = &FailureEventPayload{FailureClass: FailureProtocol, Summary: "schema repair failed"}
	c.taskTracker.TodoList().UpdateStatus(replacement.ID, TaskDone, "verified replacement")
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(`{"verdict":"approve"}`))
	if err != nil {
		t.Fatal(err)
	}
	replacement.TypedResult = &TaskResult{TaskID: replacement.ID, Agent: "worker", Status: TaskResultStatusSuccess, Summary: "reviewed", Source: "submitted", Attempt: 1, StructuredPayload: payload}
	replacement.VerifyResult, err = executeTaskResultAssertVerification("", NormalizeVerificationSpec(*spec, "", ""), replacement.TypedResult)
	if err != nil {
		t.Fatal(err)
	}
	store := mustArtifactStore(t, c.session.Workspace)
	for _, item := range items {
		artifact, err := store.Put(t.Context(), PutArtifactRequest{Kind: "task_transcript", Path: item.ID + ".jsonl", Content: []byte("transcript for task " + item.ID), RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
		if err != nil {
			t.Fatal(err)
		}
		item.ExecutionReceipt = &ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, ModelExecutionID: "exec-" + item.ID, ProducerID: item.Agent, TranscriptRef: artifact.ID, ExitCode: new(0)}
	}
	original.ExecutionReceipt.ExitCode = new(1)
	replacement.ExecutionReceipt.ResultValidation = ResultValidationValid
	replacement.ExecutionReceipt.ResultContractID = ref.ID
	replacement.ExecutionReceipt.ResultPayloadSHA256 = payload.SHA256
	return c, original, replacement
}

func TestVerifiedResolutionSatisfiesAllFinalizationGatesAndReplay(t *testing.T) {
	for _, status := range []string{"superseded", "reconciled"} {
		t.Run(status, func(t *testing.T) {
			c, original, replacement := verifiedResolutionFixture(t)
			journal := &recordingJournal{}
			c.SetEventJournal(journal)
			resolution := &TaskResolution{Status: status, ResolvedBy: replacement.ID, Reason: "same frozen requirement, verified replacement"}
			if err := c.CommitTaskResolution(t.Context(), original.ID, resolution); err != nil {
				t.Fatal(err)
			}
			items := c.taskTracker.TodoList().Items()
			if len(failedTodoItems(items)) != 0 || len(UnresolvedTaskReferences(items)) != 0 || SummarizeRunStats(items).TasksUnresolved != 0 {
				t.Fatal("verified resolution still appears unresolved")
			}
			c.acceptanceSpec = &AcceptanceSpec{RequireNoUnresolvedTasks: true, RequiredWorkers: []string{"worker"}}
			acceptance, err := c.runAcceptance(t.Context())
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("acceptance = %#v, %v", acceptance, err)
			}
			manifest, err := c.buildEvidenceManifest(t.Context(), true)
			if err != nil || manifest.Status != "accepted" {
				t.Fatalf("manifest = %#v, %v", manifest, err)
			}
			proof := taskManifestEvidence(manifest, original.ID)
			if proof == nil || proof.Resolution == nil || proof.Resolution.ResolvedBy != replacement.ID || proof.Binding.TaskID != replacement.ID {
				t.Fatalf("original requirement did not retain replacement provenance: %#v", proof)
			}
			if len(manifest.ArtifactRefs) != 2 {
				t.Fatalf("original forensic transcript lost: %#v", manifest.ArtifactRefs)
			}
			result := c.applyCompletionGate(t.Context(), &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true}, acceptance)
			if result.Outcome != RunOutcomeCompleted || !result.GoalSatisfied || result.ExitCode != 0 {
				t.Fatalf("completion = %#v", result)
			}
			current := todoItemByID(items, original.ID)
			if current.Status != TaskBlocked || current.FailureEvent.Summary != "schema repair failed" || *current.ExecutionReceipt.ExitCode != 1 {
				t.Fatal("resolution rewrote original failed execution")
			}
			// Replay canonical task snapshots and the actual resolution event,
			// not manually manufactured resolution-only data.
			events := make([]RunEvent, 0, 3)
			for _, item := range []*TodoItem{original, replacement} {
				payload, err := json.Marshal(c.taskTransitionPayloadWithCoordinator(item))
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Actor: item.Agent, Payload: payload})
			}
			events = append(events, journal.events[0])
			replayed := ReduceToTodoList(events)
			if len(failedTodoItems(replayed)) != 0 || todoItemByID(replayed, original.ID).Status != TaskBlocked {
				t.Fatalf("resolution replay = %#v", replayed)
			}
			resumed, err := (&defaultEvidenceService{}).BuildRunManifest(t.Context(), EvidenceBuildRequest{RunID: c.executionRunID, Workspace: c.session.Workspace, Items: replayed, Strict: true})
			if err != nil || resumed.Status != "accepted" {
				t.Fatalf("resumed manifest = %#v, %v", resumed, err)
			}
		})
	}
}

func TestResolutionRejectsUnverifiedOrWeakerReplacement(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TodoItem, *TodoItem)
	}{
		{"not_done", func(_, r *TodoItem) { r.Status = TaskBlocked }},
		{"no_verification", func(_, r *TodoItem) { r.VerifyResult = nil }},
		{"failed_verification", func(_, r *TodoItem) { r.VerifyResult.ExitCode = 1 }},
		{"timed_out_verification", func(_, r *TodoItem) { r.VerifyResult.TimedOut = true }},
		{"overturned_verification", func(_, r *TodoItem) { r.VerifyResult.Overturned = true }},
		{"different_contract", func(_, r *TodoItem) { r.ContractID = "unrelated" }},
		{"different_contract_hash", func(_, r *TodoItem) { r.ContractHash = "weaker" }},
		{"different_execution", func(_, r *TodoItem) { r.Execution.RequiresResult = false }},
		{"different_phase", func(_, r *TodoItem) { r.Phase = PhaseVerify }},
		{"different_scope", func(_, r *TodoItem) { r.ResourceScopeSnapshot = &TaskResourceScopeSnapshot{Digest: "other"} }},
		{"different_side_effect", func(_, r *TodoItem) { r.SideEffect = SideEffectWorkspaceWrite }},
		{"different_evidence_inputs", func(_, r *TodoItem) { r.EvidenceFrom = []string{"other"} }},
		{"missing_invariant_attestation", func(o, r *TodoItem) {
			o.InvariantVerification, r.InvariantVerification = InvariantVerificationGate, InvariantVerificationGate
		}},
		{"different_inputs", func(o, r *TodoItem) { o.RunInputSnapshotHash, r.RunInputSnapshotHash = "original", "other" }},
		{"weaker_verifier", func(_, r *TodoItem) { r.VerifySpec = nil }},
		{"wrong_verification_receipt", func(_, r *TodoItem) { r.VerifyResult.Spec = nil }},
		{"missing_payload", func(_, r *TodoItem) { r.TypedResult.StructuredPayload = nil }},
		{"tampered_payload", func(_, r *TodoItem) { r.TypedResult.StructuredPayload.Value = json.RawMessage(`{"verdict":"reject"}`) }},
		{"wrong_run", func(_, r *TodoItem) { r.ExecutionReceipt.RunID = "older-run" }},
		{"wrong_receipt_task", func(_, r *TodoItem) { r.ExecutionReceipt.TaskID = "other" }},
		{"wrong_receipt_attempt", func(_, r *TodoItem) { r.ExecutionReceipt.Attempt = 2 }},
		{"failed_receipt", func(_, r *TodoItem) { r.ExecutionReceipt.ExitCode = new(1) }},
		{"wrong_payload_receipt", func(_, r *TodoItem) { r.ExecutionReceipt.ResultPayloadSHA256 = "other" }},
		{"partial_canonical_result", func(_, r *TodoItem) { r.TypedResult.Status = TaskResultStatusPartial }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, original, replacement := verifiedResolutionFixture(t)
			tt.mutate(original, replacement)
			resolution := &TaskResolution{Status: "superseded", ResolvedBy: replacement.ID, Reason: "model claims it is equivalent"}
			if err := c.CommitTaskResolution(t.Context(), original.ID, resolution); err == nil {
				t.Fatal("unverified replacement committed")
			}
			// A forged/restored label must also fail the read-only projections.
			original.Resolution = resolution
			items := c.taskTracker.TodoList().Items()
			if VerifiedTaskResolution(original, items, c.executionRunID) != nil {
				t.Fatal("unverified restored resolution satisfied the requirement")
			}
			manifest, err := c.buildEvidenceManifest(t.Context(), false)
			if err == nil && taskManifestEvidence(manifest, original.ID).Status == "passed" {
				t.Fatal("forged restored resolution passed the manifest")
			}
		})
	}
}

func TestResolutionProofCannotBeForgedOrDetachedFromReplacement(t *testing.T) {
	for _, name := range []string{"missing_replacement", "failed_replacement", "wrong_binding", "cycle", "waived"} {
		t.Run(name, func(t *testing.T) {
			c, original, replacement := verifiedResolutionFixture(t)
			if err := c.CommitTaskResolution(t.Context(), original.ID, &TaskResolution{Status: "superseded", ResolvedBy: replacement.ID}); err != nil {
				t.Fatal(err)
			}
			manifest, err := c.buildEvidenceManifest(t.Context(), true)
			if err != nil {
				t.Fatal(err)
			}
			proof := taskManifestEvidence(manifest, original.ID)
			switch name {
			case "missing_replacement":
				manifest.EvidenceResults = manifest.EvidenceResults[:1]
			case "failed_replacement":
				taskManifestEvidence(manifest, replacement.ID).Status = "failed"
			case "wrong_binding":
				binding := *proof.Binding
				binding.Attempt++
				proof.Binding = &binding
			case "cycle":
				taskManifestEvidence(manifest, replacement.ID).Resolution = &EvidenceResolution{Status: "superseded", ResolvedBy: original.ID, OriginalStatus: string(TaskBlocked)}
			case "waived":
				proof.Resolution.Status = "waived"
			}
			if err := manifest.Seal(); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Verify(t.Context(), mustArtifactStore(t, c.session.Workspace)); err == nil {
				t.Fatal("forged resolution passed sealed manifest verification")
			}
		})
	}
}

func TestResolutionWithoutReplacementTranscriptFailsClosed(t *testing.T) {
	c, original, replacement := verifiedResolutionFixture(t)
	replacement.ExecutionReceipt.TranscriptRef = ""
	original.Resolution = &TaskResolution{Status: "superseded", ResolvedBy: replacement.ID}
	manifest, err := c.buildEvidenceManifest(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "failed" || taskManifestEvidence(manifest, original.ID).Resolution != nil {
		t.Fatal("replacement without transcript satisfied the original")
	}
}

func TestResolutionRejectedByToolDoesNotEraseOriginalFailure(t *testing.T) {
	c, original, replacement := verifiedResolutionFixture(t)
	replacement.VerifyResult = nil
	before := *original.FailureEvent
	response, err := (&reconcileTaskTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"task_id":"1","status":"superseded","resolved_by":"2","reason":"already fixed"}`})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "objective verification") {
		t.Fatalf("tool rejection = %#v, %v", response, err)
	}
	if original.Resolution != nil || original.Status != TaskBlocked || !reflect.DeepEqual(*original.FailureEvent, before) {
		t.Fatal("rejected resolution rewrote the failure")
	}
}
