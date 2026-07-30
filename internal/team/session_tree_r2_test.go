package team

import (
	"encoding/json"
	"testing"
)

func TestTypedVerification_EventStoreBranchReplayAndCheckpointDedup(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-typed", "session-typed")
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{eventStore: es}

	v1 := &TodoItem{
		ID: "1", Agent: "dev", Desc: "verify report", Status: TaskDone,
		VerifySpec: &VerificationSpec{Type: VerifyJSONAssert, Path: "report-v1.json", Assertions: []JSONAssertion{{Path: "status", Equals: "ok"}}},
	}
	c.emitTaskEventsFromCheckpoint([]*TodoItem{v1})
	c.emitTaskEventsFromCheckpoint([]*TodoItem{v1}) // unchanged contract: one event only
	events, err := es.ReadEvents()
	if err != nil || len(events) != 1 {
		t.Fatalf("unchanged typed checkpoint must emit one event, events=%d err=%v", len(events), err)
	}
	forkEventID := events[0].ID

	// Same task lifecycle state with a changed verifier is a distinct durable
	// contract and must be emitted so replay observes the newer assertion.
	v2 := cloneTodoItem(v1)
	v2.VerifySpec = &VerificationSpec{Type: VerifyJSONAssert, Path: "report-v2.json", Assertions: []JSONAssertion{{Path: "code", Equals: 200}}}
	c.emitTaskEventsFromCheckpoint([]*TodoItem{v2})
	events, err = es.ReadEvents()
	if err != nil || len(events) != 2 {
		t.Fatalf("changed typed contract must emit an updated event, events=%d err=%v", len(events), err)
	}

	st := NewSessionTree()
	exp, err := st.CreateBranch("typed-exp", forkEventID, es)
	if err != nil {
		t.Fatal(err)
	}
	es.SetBranchID(exp.ID)
	expTask := &TodoItem{
		ID: "2", Agent: "dev", Desc: "verify branch artifact", Status: TaskDone,
		VerifySpec: &VerificationSpec{Type: VerifyFileExists, Path: "branch-report.json"},
	}
	c.emitTaskEventsFromCheckpoint([]*TodoItem{expTask})
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen the real JSONL store before replaying: this exercises producer
	// serialization, disk read, branch lineage selection, and session rebuild.
	reopened, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := RebuildSessionForBranch(workspace, st, reopened, "main"); err != nil {
		t.Fatal(err)
	}
	main := LoadSession(workspace)
	if main == nil || len(main.Tasks) != 1 || main.Tasks[0].VerifySpec == nil {
		t.Fatalf("main branch typed replay lost task contract: %#v", main)
	}
	mainSpec := main.Tasks[0].VerifySpec
	if mainSpec.Path != "report-v2.json" || len(mainSpec.Assertions) != 1 || mainSpec.Assertions[0].Path != "code" || !equalJSONValues(mainSpec.Assertions[0].Equals, 200) {
		t.Fatalf("main replay restored wrong latest contract: %#v", mainSpec)
	}

	if err := RebuildSessionForBranch(workspace, st, reopened, exp.ID); err != nil {
		t.Fatal(err)
	}
	branch := LoadSession(workspace)
	if branch == nil || len(branch.Tasks) != 2 || branch.Tasks[0].VerifySpec == nil || branch.Tasks[1].VerifySpec == nil {
		t.Fatalf("branch lineage did not restore typed contracts: %#v", branch)
	}
	if got := branch.Tasks[0].VerifySpec.Path; got != "report-v1.json" {
		t.Fatalf("fork lineage must retain pre-fork typed contract, got %q", got)
	}
	if got := branch.Tasks[1].VerifySpec.Path; got != "branch-report.json" {
		t.Fatalf("branch event typed contract lost, got %q", got)
	}

	// Mutating a decoded projection cannot mutate the persisted event payload;
	// rebuilding again must restore the original typed values.
	branch.Tasks[0].VerifySpec.Assertions[0].Equals = "mutated"
	if err := RebuildSessionForBranch(workspace, st, reopened, exp.ID); err != nil {
		t.Fatal(err)
	}
	replayed := LoadSession(workspace)
	if got := replayed.Tasks[0].VerifySpec.Assertions[0].Equals; got != "ok" {
		t.Fatalf("replay reused mutable typed contract instead of event data: %#v", replayed.Tasks[0].VerifySpec)
	}
}

// Task events must carry verify command + result so branch diffs can compare
// verification outcomes, and completed tasks with typed artifacts must emit
// artifact_created events exactly once (R2).
func TestEmitTaskEvents_IncludesVerifyAndArtifacts(t *testing.T) {
	tempDir := t.TempDir()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer func() { _ = es.Close() }()

	c := &Coordinator{eventStore: es}
	tasks := []*TodoItem{{
		ID:           "1",
		Agent:        "dev",
		Desc:         "build report",
		Status:       TaskDone,
		Verify:       "test -f report.md",
		VerifyMode:   "success",
		VerifyResult: &VerificationResult{Command: "test -f report.md", ExitCode: 0},
		TypedResult: &TaskResult{
			Summary:   "done",
			Artifacts: []ArtifactRef{{Path: "report.md"}, {Path: "data.csv"}},
		},
	}}

	c.emitTaskEventsFromCheckpoint(tasks)
	c.emitTaskEventsFromCheckpoint(tasks) // second checkpoint must not duplicate

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	var completed *RunEvent
	artifactPaths := map[string]int{}
	for i := range events {
		switch events[i].Type {
		case "task_completed":
			completed = &events[i]
		case "artifact_created":
			var p struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(events[i].Payload, &p); err == nil {
				artifactPaths[p.Path]++
			}
		}
	}

	if completed == nil {
		t.Fatalf("expected a task_completed event")
	}
	var payload struct {
		Verify       string `json:"verify"`
		VerifyResult struct {
			ExitCode int  `json:"exit_code"`
			TimedOut bool `json:"timed_out"`
		} `json:"verify_result"`
	}
	if err := json.Unmarshal(completed.Payload, &payload); err != nil {
		t.Fatalf("unmarshal task payload: %v", err)
	}
	if payload.Verify != "test -f report.md" {
		t.Errorf("expected verify command in payload, got %q", payload.Verify)
	}
	if payload.VerifyResult.ExitCode != 0 {
		t.Errorf("expected verify_result.exit_code 0, got %d", payload.VerifyResult.ExitCode)
	}

	if len(artifactPaths) != 2 {
		t.Fatalf("expected 2 distinct artifact_created events, got %v", artifactPaths)
	}
	for path, n := range artifactPaths {
		if n != 1 {
			t.Errorf("artifact %s emitted %d times, expected exactly once", path, n)
		}
	}
}

// ReduceToTodoList must reconstruct Verify/VerifyResult from task events so
// downstream consumers (e.g. DiffBranches) can evaluate verification state (R2).
func TestReduceToTodoList_ReconstructsVerify(t *testing.T) {
	payload, _ := json.Marshal(map[string]interface{}{
		"id":          "1",
		"desc":        "build report",
		"status":      "done",
		"verify":      "test -f report.md",
		"verify_mode": "success",
		"verify_result": map[string]interface{}{
			"exit_code": 1,
			"timed_out": false,
		},
	})
	events := []RunEvent{
		{ID: "evt-1", Type: "task_completed", TaskID: "1", Payload: payload},
	}

	todos := ReduceToTodoList(events)
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo, got %d", len(todos))
	}
	if todos[0].Verify != "test -f report.md" {
		t.Errorf("expected Verify reconstructed, got %q", todos[0].Verify)
	}
	if todos[0].VerifyMode != "success" {
		t.Errorf("expected VerifyMode reconstructed, got %q", todos[0].VerifyMode)
	}
	if todos[0].VerifyResult == nil || todos[0].VerifyResult.ExitCode != 1 {
		t.Fatalf("expected VerifyResult exit_code 1, got %+v", todos[0].VerifyResult)
	}
	if isVerifySuccess(todos[0].VerifyResult) {
		t.Errorf("exit_code 1 must not be a verify success")
	}
}

// End-to-end: with verify data present in events, DiffBranches must surface
// verification and artifact differences between branches (R2).
func TestSessionTree_DiffBranches_VerificationAndArtifacts(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer func() { _ = es.Close() }()

	mainPayload, _ := json.Marshal(map[string]interface{}{
		"id":            "1",
		"desc":          "shared task",
		"status":        "done",
		"verify":        "make check",
		"verify_result": map[string]interface{}{"exit_code": 0},
	})
	_ = es.Append(RunEvent{ID: "evt-m1", BranchID: "main", Type: "task_completed", TaskID: "1", Payload: mainPayload})

	exp, err := st.CreateBranch("exp", "evt-m1", es)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	expPayload, _ := json.Marshal(map[string]interface{}{
		"id":            "1",
		"desc":          "shared task",
		"status":        "done",
		"verify":        "make check",
		"verify_result": map[string]interface{}{"exit_code": 2},
	})
	_ = es.Append(RunEvent{ID: "evt-e1", BranchID: exp.ID, Type: "task_completed", TaskID: "1", Payload: expPayload})
	artPayload, _ := json.Marshal(map[string]string{"path": "exp-only.txt"})
	_ = es.Append(RunEvent{ID: "evt-e2", BranchID: exp.ID, Type: "artifact_created", Payload: artPayload})

	diff, err := DiffBranches(tempDir, st, es, "main", exp.ID)
	if err != nil {
		t.Fatalf("DiffBranches failed: %v", err)
	}

	if len(diff.VerifyDiffs) != 1 {
		t.Fatalf("expected 1 verification diff, got %d", len(diff.VerifyDiffs))
	}
	vd := diff.VerifyDiffs[0]
	if vd.DiffType != "status_changed" || !vd.PassedA || vd.PassedB {
		t.Errorf("unexpected verification diff: %+v", vd)
	}

	if len(diff.ArtifactDiffs) != 1 || diff.ArtifactDiffs[0].Path != "exp-only.txt" || diff.ArtifactDiffs[0].DiffType != "only_in_b" {
		t.Errorf("unexpected artifact diffs: %+v", diff.ArtifactDiffs)
	}
}
