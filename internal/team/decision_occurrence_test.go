package team

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestPrimaryDecisionOccurrenceRequiresDurableAdmission(t *testing.T) {
	admitted := universalDecisionLifecycleEvents(t)[2]
	item, err := ProjectPrimaryDecisionOccurrence(admitted)
	if err != nil {
		t.Fatal(err)
	}
	if !IsPrimaryOccurrence(item) || IsSchedulableWorker(item) || IsBlockingSupportingWork(item) {
		t.Fatalf("unexpected primary predicates: primary=%v schedulable=%v blocking=%v", IsPrimaryOccurrence(item), IsSchedulableWorker(item), IsBlockingSupportingWork(item))
	}

	forged := cloneTodoItem(item)
	forged.PrimaryAdmission = nil
	if IsPrimaryOccurrence(forged) || IsSchedulableWorker(forged) {
		t.Fatal("reserved ID without admission became an executable occurrence")
	}
	if err := validatePrimaryOccurrenceForExecution(forged); !errors.Is(err, ErrDecisionOccurrenceAdmissionMissing) {
		t.Fatalf("forged occurrence error = %v", err)
	}
}

func TestPrimaryDecisionOccurrenceRoundTripsProjectionAndCheckpoint(t *testing.T) {
	item, err := ProjectPrimaryDecisionOccurrence(universalDecisionLifecycleEvents(t)[2])
	if err != nil {
		t.Fatal(err)
	}
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	if projection.RuntimeOccurrence == nil || projection.PrimaryAdmission == nil || projection.RuntimeOccurrence.DecisionID != item.RuntimeOccurrence.DecisionID {
		t.Fatalf("runtime projection lost primary metadata: %#v", projection)
	}

	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var restored TodoItem
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !IsPrimaryOccurrence(&restored) {
		t.Fatalf("checkpoint round-trip lost admission: %#v", restored)
	}
	cloned := cloneTodoItem(&restored)
	cloned.RuntimeOccurrence.DecisionID = "pd_" + testDigestB
	if restored.RuntimeOccurrence.DecisionID == cloned.RuntimeOccurrence.DecisionID {
		t.Fatal("clone aliases runtime metadata")
	}
}

func TestPrimaryDecisionAdmissionAtomicallyCreatesTodoProjection(t *testing.T) {
	events := universalDecisionLifecycleEvents(t)
	result := reduceToTodoList(events[:3])
	if len(result.tasks) != 1 || !IsPrimaryOccurrence(result.tasks[0]) {
		t.Fatalf("admitted event did not create primary occurrence: %#v", result.tasks)
	}
	if result.tasks[0].Status != TaskPending {
		t.Fatalf("primary status = %s, want pending", result.tasks[0].Status)
	}
}

func TestSchedulerAndResumeExcludePrimaryOccurrence(t *testing.T) {
	item, err := ProjectPrimaryDecisionOccurrence(universalDecisionLifecycleEvents(t)[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateTaskExecutionEnvelopes([]TaskDef{{Kind: TaskKindOutcome}}, []*TodoItem{item}, []TaskExecutionEnvelope{{}}); !errors.Is(err, ErrDecisionOccurrenceWrongOwner) {
		t.Fatalf("scheduler primary error = %v", err)
	}
	tracker := NewTaskTracker()
	tracker.todo.items = []*TodoItem{item}
	coordinator := &Coordinator{taskTracker: tracker}
	if interrupted := coordinator.getInterruptedTasks(); len(interrupted) != 0 {
		t.Fatalf("generic resume selected primary occurrence: %#v", interrupted)
	}
}

func TestPrimaryOccurrenceExcludedFromSupportingStatsAndUnresolved(t *testing.T) {
	primary, err := ProjectPrimaryDecisionOccurrence(universalDecisionLifecycleEvents(t)[2])
	if err != nil {
		t.Fatal(err)
	}
	supporting := &TodoItem{ID: "support-1", Status: TaskPending, Kind: TaskKindOutcome}
	stats := SummarizeRunStats([]*TodoItem{primary, supporting})
	if stats.TasksTotal != 1 || stats.TasksUnresolved != 1 || stats.RuntimeOccurrences != 1 || stats.PrimaryGenerations != 1 {
		t.Fatalf("stats = %#v", stats)
	}
	unresolved := UnresolvedTaskReferences([]*TodoItem{primary, supporting})
	if len(unresolved) != 1 || unresolved[0].ID != supporting.ID {
		t.Fatalf("unresolved = %#v", unresolved)
	}
	if len(pendingTodoItems([]*TodoItem{primary, supporting})) != 1 {
		t.Fatal("finish pending check included primary occurrence")
	}
}

func TestDecisionEvidenceManifestRequiresOneTypedPrimaryProof(t *testing.T) {
	primary, err := ProjectPrimaryDecisionOccurrence(universalDecisionLifecycleEvents(t)[2])
	if err != nil {
		t.Fatal(err)
	}
	primary.Status = TaskDone
	ref := primary.PrimaryAdmission.AdmissionRef
	primary.PrimaryManifestProof = &PrimaryManifestProofV1{
		SchemaVersion: 1, Kind: "primary_decision_valid", State: "satisfied",
		LogicalRunID: primary.RuntimeOccurrence.LogicalRunID, BranchID: primary.RuntimeOccurrence.BranchID,
		RequirementDigest: testDigestA, PrimaryTaskID: primary.ID, Generation: primary.RuntimeOccurrence.Generation,
		DecisionID: primary.RuntimeOccurrence.DecisionID, AdmissionRef: ref, RecordRef: ref, BaseEvidenceRef: ref,
		SealedEvidenceHash: testDigestB, RolePlanRef: ref, BindingEventID: "evt-bound",
		SupportRevisionDigest: testDigestC, ValidationVersion: "primary-decision-process@v1",
	}
	service := &defaultEvidenceService{}
	manifest, err := service.BuildRunManifest(context.Background(), EvidenceBuildRequest{
		RunID: "run-1", Workspace: t.TempDir(), Items: []*TodoItem{primary}, Strict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "accepted" || len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].PrimaryDecisionProof == nil {
		t.Fatalf("manifest = %#v", manifest)
	}

	manifest.EvidenceResults[0].PrimaryDecisionProof = nil
	manifest.ManifestHash = ""
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(context.Background(), store); err == nil {
		t.Fatal("accepted manifest without primary proof passed verification")
	}
}
