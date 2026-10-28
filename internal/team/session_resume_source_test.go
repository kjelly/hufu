package team

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestResumeSourceSelectsCanonicalBranchWithoutFallback(t *testing.T) {
	workspace, tree, store := branchSwitchFixture(t)
	item := &TodoItem{ID: "4", Agent: "producer", Status: TaskDone, Desc: "original accepted result"}
	raw, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: string(EventTaskCompleted), TaskID: item.ID, Payload: raw}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CheckoutSessionBranch(t.Context(), workspace, tree, store, "sibling"); err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, workspace)
	for _, target := range []string{"missing", "sibling"} {
		if _, err := ReadResumeSource(workspace, target, ""); err == nil || !strings.Contains(err.Error(), "available recovery branches") {
			t.Fatalf("invalid resume source %s accepted: %v", target, err)
		}
		if !before.equal(snapshotRuntimeState(t, workspace)) {
			t.Fatal("read-only refusal mutated the session or branches")
		}
	}
	if _, err := ReadResumeSource(workspace, "main", "wrong-run"); err == nil {
		t.Fatal("unrelated run silently fell back to active session")
	}
	source, err := ReadResumeSource(workspace, "main", "run-switch")
	if err != nil || len(source.Session.Tasks) != 1 || source.Session.Tasks[0].ID != "4" {
		t.Fatalf("resume selected wrong canonical tasks: source=%+v err=%v", source, err)
	}
	if !before.equal(snapshotRuntimeState(t, workspace)) {
		t.Fatal("reading original branch changed active results")
	}
	if err := ActivateResumeSource(t.Context(), workspace, source); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadSessionReadOnly(workspace)
	if err != nil || len(loaded.Tasks) != 1 || !sameCanonicalEvidence(loaded.Tasks[0], source.Session.Tasks[0]) {
		t.Fatalf("activation failed to restore exact canonical task: %v", err)
	}
	tree, err = LoadSessionTree(workspace)
	if err != nil || tree.GetBranch("sibling") == nil || tree.ActiveBranch != "main" {
		t.Fatal("activation lost the outgoing branch")
	}
}

// Characterizes the metadata activation gap before the version-aware adapter
// replaces it. The behavior assertion is replaced by the activation contract
// test when the command path is fixed.
func TestResumeActivationMetadataLeavesMaterializedBranchUnchanged(t *testing.T) {
	f := newVersionFixture(t)
	f.write("version.txt", "A")
	appendBranchEvent(t, f.events, RunEvent{BranchID: "main", Type: string(EventTaskCompleted), TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","desc":"accepted work","status":"done"}`)})
	mustFork(t, f, "b", "")
	f.write("version.txt", "B")
	if _, _, err := f.ops().SnapshotNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateResumeSource(t.Context(), f.control, source); err != nil {
		t.Fatal(err)
	}
	tree, err := LoadSessionTree(f.control)
	if err != nil || tree.ActiveBranch != "main" {
		t.Fatalf("active branch: tree=%+v err=%v", tree, err)
	}
	state, ok, err := f.store.GetSubjectState(t.Context(), f.subject)
	if err != nil || !ok || state.MaterializedBranchID != "b" || f.read("version.txt") != "B" {
		t.Fatalf("metadata activation characterization changed: state=%+v exists=%v err=%v", state, ok, err)
	}
}
