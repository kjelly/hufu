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
