package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

const derivedParentInstructions = "FAILED_PARENT_INSTRUCTIONS_582391"

func prepareDerivedProcessBranch(t *testing.T, f historicalRunProcessFixture, binary string) func() {
	t.Helper()
	store, err := team.NewEventStore(f.control, "failed-parent-run", "failed-parent-session")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{"team":"dev"}`)},
		{Type: string(team.EventWorkflowCycleStarted), Actor: "coordinator", Payload: []byte(`{"task_id_floor":1,"request":"` + derivedParentInstructions + `"}`)},
		{Type: string(team.EventTaskCreated), Actor: "coordinator", TaskID: "2", Payload: []byte(`{"id":"2","agent":"researcher","desc":"` + derivedParentInstructions + `","status":"pending","side_effect":"none"}`)},
		{Type: string(team.EventTaskFailed), Actor: "researcher", TaskID: "2", Payload: []byte(`{"id":"2","agent":"researcher","status":"error","error":"fixture failure","side_effect":"none"}`)},
		{Type: string(team.EventWorkflowStateChanged), Actor: "runtime", Payload: []byte(`{"state":"FAILED"}`)},
		{Type: string(team.EventRunFinished), Actor: "coordinator", Payload: []byte(`{"outcome":"failed","goal_satisfied":false,"response":"` + derivedParentInstructions + `"}`)},
	} {
		if _, err := store.AppendPersistedContext(t.Context(), event); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	parent, err := store.ReadEvents()
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(f.control, team.ReduceToSessionData(parent)); err != nil {
		t.Fatal(err)
	}
	if _, err := derivedNativeProcess(t, f, binary, "workspace", "version", "snapshot", "--workspace", f.control); err != nil {
		t.Fatal(err)
	}
	reports := derivedProcessReports(t, f.control)
	if len(reports) < 2 {
		t.Fatal("fixture lacks original accepted and failed run reports")
	}
	output, err := derivedNativeProcess(t, f, binary, "session", "fork", "main", "--name", "investigation", "--new-task", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var fork sessionForkOutput
	if err := json.Unmarshal(output, &fork); err != nil || !fork.NewTask || fork.InitialState != "awaiting_new_request" {
		t.Fatalf("derived binary fork: %v / %s", err, output)
	}
	return func() {
		t.Helper()
		events, err := team.OpenEventStoreReadOnly(f.control)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = events.Close() }()
		all, err := events.ReadEvents()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < len(parent) || !reflect.DeepEqual(all[:len(parent)], parent) {
			t.Fatal("derived binary rewrote original parent events")
		}
		after := derivedProcessReports(t, f.control)
		for id, before := range reports {
			if !bytes.Equal(before, after[id]) {
				t.Fatal("derived binary rewrote original parent report", id)
			}
		}
		tree, err := team.LoadSessionTree(f.control)
		if err != nil {
			t.Fatal(err)
		}
		lineage, err := team.ProjectValidatedEventsForBranch(all, tree, "main")
		if err != nil || team.ReduceToSessionData(lineage).WorkflowState != team.PhaseFailed {
			t.Fatalf("derived child cleared parent failure: %v", err)
		}
		child := team.LoadSession(f.control)
		if child == nil || child.DerivedFork == nil || child.DerivedFork.AwaitingNewRequest || child.WorkflowState != team.PhaseDone || len(child.Tasks) != 2 {
			t.Fatal("derived binary did not complete fresh work")
		}
		for _, item := range child.Tasks {
			id, err := strconv.Atoi(item.ID)
			if err != nil || id <= 2 || item.Status != team.TaskDone || item.TypedResult == nil {
				t.Fatalf("derived result reuses a parent occurrence: %+v", item)
			}
		}
	}
}

func derivedNativeProcess(t *testing.T, f historicalRunProcessFixture, binary string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = f.subject
	command.Env = []string{"HOME=" + os.Getenv("HOME"), "HUFU_STATE_HOME=" + os.Getenv("HUFU_STATE_HOME"), "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "NO_COLOR=1", "TERM=dumb"}
	output, err := command.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("native fixture command: %w: %s", err, output)
	}
	return output, nil
}

func derivedProcessReports(t *testing.T, workspace string) map[string][]byte {
	t.Helper()
	store, err := runtimestore.OpenReadOnly(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rows, err := store.DB().QueryContext(t.Context(), `SELECT source_event_id, json_array(source_hash,schema_version,state,reason,hex(snapshot)) FROM run_reports`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	reports := make(map[string][]byte)
	for rows.Next() {
		var id string
		var snapshot []byte
		if err := rows.Scan(&id, &snapshot); err != nil {
			t.Fatal(err)
		}
		reports[id] = snapshot
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return reports
}
