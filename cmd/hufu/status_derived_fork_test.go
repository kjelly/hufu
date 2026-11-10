package main

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

func TestDerivedForkOverviewAndStatusAwaitNewRequestWithoutParentRun(t *testing.T) {
	f := derivedCLIFixture(t)
	if _, err := runCLI(t, "session", "fork", "main", "--new-task", "--name", "investigation", "--workspace", f.control); err != nil {
		t.Fatal(err)
	}
	before := runtimeStoreFiles(t, f.control)
	envelope, err := inspectpkg.InspectOverview(t.Context(), inspectpkg.InspectQuery{Workspace: f.control})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := envelope.Data.(inspectpkg.OverviewData).Snapshot
	if snapshot == nil || snapshot.DerivedFork == nil || !snapshot.DerivedFork.AwaitingNewRequest || snapshot.Scope.RunID != "" ||
		snapshot.Scope.InvocationID != "" || snapshot.Activity.State != operatorpkg.ActivityWaitingInput || snapshot.Outcome.GoalSatisfied != nil ||
		snapshot.DerivedFork.SourceRunID != "parent-run" || snapshot.DerivedFork.SourceRunMembership != "visible" ||
		snapshot.DerivedFork.SourceBranchID != "main" || snapshot.DerivedFork.CaptureOrigin != "known" ||
		snapshot.PrimaryAction == nil || snapshot.PrimaryAction.ID != operatorpkg.ActionProvideInput || len(snapshot.PrimaryAction.Argv) != 0 {
		t.Fatalf("pending branch was projected as a parent execution: %+v", snapshot)
	}
	previousWorkspace, previousJSON, previousWorkers := statusWorkspace, statusJSON, statusWorkers
	t.Cleanup(func() { statusWorkspace, statusJSON, statusWorkers = previousWorkspace, previousJSON, previousWorkers })
	statusWorkspace, statusJSON, statusWorkers = f.control, true, false
	command := &cobra.Command{}
	var out bytes.Buffer
	command.SetOut(&out)
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	var status workspaceStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil || status.RunID != "" || status.Total != 0 ||
		status.DerivedFork == nil || !status.DerivedFork.AwaitingNewRequest {
		t.Fatalf("pending status = %s (%v)", out.String(), err)
	}
	statusJSON = false
	out.Reset()
	if err := runStatus(command, nil); err != nil || !strings.Contains(out.String(), "awaiting_new_request") ||
		strings.Contains(out.String(), "Run:        parent-run") {
		t.Fatalf("pending status text = %s (%v)", out.String(), err)
	}
	parent, err := inspectpkg.InspectOverview(t.Context(), inspectpkg.InspectQuery{Workspace: f.control, RunID: "parent-run"})
	if err != nil {
		t.Fatal(err)
	}
	parentSnapshot := parent.Data.(inspectpkg.OverviewData).Snapshot
	if parentSnapshot.Scope.RunID != "parent-run" || parentSnapshot.DerivedFork != nil {
		t.Fatal("explicit historical parent was replaced by pending scope")
	}
	if !reflect.DeepEqual(before, runtimeStoreFiles(t, f.control)) {
		t.Fatal("offline derived overview/status modified the canonical workspace")
	}
}
