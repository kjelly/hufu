package team

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func TestDerivedForkResetsExecutableProjectionAndPreservesParent(t *testing.T) {
	f, ops := derivedForkPreparationFixture(t)
	compaction := testCompactionState(t, f.control)
	attachCompactionEventStore(t, f.control, "main", newTestCoordinator(t), compaction)
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventTaskCreated), TaskID: "12", RunID: "source-run",
		Payload: []byte(`{"id":"12","agent":"worker","desc":"parent instructions","status":"in_progress","side_effect":"none"}`)})
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "source-run", Payload: []byte(`{"state":"FAILED"}`)})
	parent := mustReadEvents(t, f.events)
	live := ReduceToSessionData(parent)
	live.ConversationHistorySourceOffset, live.ConversationHistoryNextSourceIndex = 8, 15
	live.ConversationHistorySourceCounts = []int{8}
	if err := SaveSession(f.control, live); err != nil {
		t.Fatal(err)
	}
	result, err := ops.ForkNewTask(t.Context(), "investigation", "main")
	if err != nil {
		t.Fatal(err)
	}
	events := mustReadEvents(t, f.events)
	lineage, err := ProjectValidatedEventsForBranch(events, f.tree, result.Branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	child := LoadSession(f.control)
	if child == nil || child.DerivedFork == nil || !child.DerivedFork.AwaitingNewRequest || child.WorkflowTaskIDFloor != 12 ||
		child.WorkflowState != PhaseInit || child.WorkflowCycleID != "" || len(child.Tasks) != 0 || len(child.Entries) != 0 ||
		child.ExecutionPolicySnapshot != nil || child.HistoricalResultGrants != nil || len(child.RunInputSnapshots) != 0 ||
		len(child.AcceptanceContractRevisions) != 0 || child.ConversationHistorySourceOffset != 0 || child.ConversationHistoryNextSourceIndex != 0 {
		t.Fatalf("child inherited executable or conversation state: %+v", child)
	}
	if err := CompareCanonicalProjection(child, lineage); err != nil {
		t.Fatal(err)
	}
	state, exists, err := LoadConversationCompactionState(f.control)
	if err != nil || !exists || !reflect.DeepEqual(state.Branches["main"], compaction.Branches["main"]) ||
		state.Branches[result.Branch.ID].GenerationID != "" || len(state.Checkpoints[result.Branch.ID]) != 0 {
		t.Fatalf("derived fork copied parent compaction or changed its source: %v", err)
	}
	if _, err := ops.PrepareDerivedFork(t.Context(), result.Branch.ID); err == nil {
		t.Fatal("awaiting child accepted as a new-task source")
	}
	parentAfter, err := ProjectValidatedEventsForBranch(events, f.tree, "main")
	if err != nil || !reflect.DeepEqual(parent, parentAfter[:len(parent)]) || ReduceToSessionData(parentAfter).WorkflowState != PhaseFailed ||
		len(ReduceToSessionData(parentAfter).Tasks) != 1 {
		t.Fatalf("derived reset rewrote the failed parent: %v", err)
	}
	ordinary, err := ops.Fork(t.Context(), "ordinary-child", result.Branch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := LoadSession(f.control); got.DerivedFork == nil || *got.DerivedFork != *child.DerivedFork || len(got.Tasks) != 0 || ordinary.Branch.Derivation != nil {
		t.Fatal("ordinary descendant lost pending ancestor identity")
	}
}

func TestDerivedForkLineageRequiresCompletePublicationAndOriginalSources(t *testing.T) {
	f, ops := derivedForkPreparationFixture(t)
	result, err := ops.ForkNewTask(t.Context(), "investigation", "main")
	if err != nil {
		t.Fatal(err)
	}
	events := mustReadEvents(t, f.events)
	for _, failure := range []string{"half_publication", "missing_intent", "wrong_source_hash", "wrong_floor", "wrong_capture"} {
		t.Run(failure, func(t *testing.T) {
			tree := cloneSessionTree(f.tree)
			lineage := append([]RunEvent(nil), events...)
			last := &lineage[len(lineage)-1]
			payload, err := decodeDerivedForkCreated(*last)
			if err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "half_publication":
				lineage = lineage[:len(lineage)-1]
			case "missing_intent":
				tree.Branches[result.Branch.ID].Derivation = nil
			case "wrong_source_hash":
				payload.Intent.SourceEventHash = strings.Repeat("b", 64)
			case "wrong_floor":
				payload.Intent.TaskIDFloor++
			case "wrong_capture":
				payload.Intent.CaptureEventHash = strings.Repeat("c", 64)
			}
			if failure == "wrong_source_hash" || failure == "wrong_floor" || failure == "wrong_capture" {
				tree.Branches[result.Branch.ID].Derivation = new(payload.Intent)
				last.Payload, err = json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ProjectValidatedEventsForBranch(lineage, tree, result.Branch.ID); err == nil {
				t.Fatal("incomplete or altered derived lineage was accepted")
			}
		})
	}
}
