package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/belief"
)

func writeBeliefModel(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile("../belief/testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBeliefTeamLoadingAndResourceBoundary(t *testing.T) {
	for _, test := range []struct {
		name, model, experiment, extra string
		wantError                      bool
	}{
		{name: "valid", model: "model.json", experiment: "comparison"},
		{name: "parent path", model: "../model.json", experiment: "comparison", wantError: true},
		{name: "unknown experiment", model: "model.json", experiment: "other", wantError: true},
		{name: "active unsupported", model: "model.json", experiment: "comparison", extra: "  mode: active\n", wantError: true},
		{name: "missing experiment", model: "model.json", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBeliefModel(t, dir)
			writeLoadTeamAgent(t, dir, "worker.md", "worker")
			manifest := "name: belief-test\nbelief:\n  model-file: " + test.model + "\n  experiment: " + test.experiment + "\n" + test.extra
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
			if (err != nil) != test.wantError {
				t.Fatalf("LoadTeam error=%v wantError=%t", err, test.wantError)
			}
			if err == nil && (session.Belief == nil || !session.Belief.model.Synthetic || session.Belief.snapshot.ExperimentID != "comparison") {
				t.Fatalf("compiled belief=%+v", session.Belief)
			}
		})
	}
	dir := t.TempDir()
	outside := t.TempDir()
	writeBeliefModel(t, outside)
	if err := os.Symlink(filepath.Join(outside, "model.json"), filepath.Join(dir, "model.json")); err != nil {
		t.Fatal(err)
	}
	session := &TeamSession{Dir: dir, Config: agent.TeamConfig{Belief: &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison"}}}
	if err := compileTeamBelief(session); err == nil {
		t.Fatal("accepted escaping symlink")
	}
}

func TestBeliefPolicySnapshotAndResumeDrift(t *testing.T) {
	dir, workspace := t.TempDir(), t.TempDir()
	writeBeliefModel(t, dir)
	start := func() *Coordinator {
		t.Helper()
		session := &TeamSession{Dir: dir, Workspace: workspace, Config: agent.TeamConfig{Name: "belief-policy", GoalMode: "exploratory", Belief: &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison"}}}
		c, err := newCoordinator(coordinatorParams{Session: session, MaxConcurrent: 1}, RuntimeServices{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if saved := LoadSession(workspace); saved != nil {
			c.SetSessionData(saved)
		}
		c.initEventStore()
		return c
	}
	first := start()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	snapshot := first.ExecutionPolicySnapshot()
	if snapshot.Belief == nil || snapshot.Belief.ModelDigest != first.session.Belief.snapshot.ModelDigest {
		t.Fatal("execution policy did not pin the belief model")
	}
	snapshot.Belief.ModelDigest = "mutated-copy"
	if first.ExecutionPolicySnapshot().Belief.ModelDigest == "mutated-copy" {
		t.Fatal("snapshot leaked a mutable belief binding")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		t.Fatal(err)
	}
	model.Version = "changed"
	data, err = json.Marshal(model)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	changed := start()
	if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("model change did not block resume: %v", err)
	}
}

type failBeliefAdmission struct {
	EventJournal
	fail bool
}

func (j *failBeliefAdmission) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == EventBeliefObservationAdmitted && j.fail {
		j.fail = false
		return RunEvent{}, errors.New("injected belief append failure")
	}
	return j.EventJournal.Append(ctx, event)
}

func runBeliefAction(t *testing.T, failAdmission bool, configure ...func(*TeamSession)) (*Coordinator, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "executions")
	command := []string{"/bin/sh", "-c", `printf x >> "$1"; printf '%s' '{"outputs":{"ok":true}}'`, "belief-action", marker}
	c, session := newCommandActionCoordinator(t, command, "run-belief-action")
	writeBeliefModel(t, session.Dir)
	session.Config.Belief = &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison"}
	for _, config := range configure {
		config(session)
	}
	if err := compileTeamBelief(session); err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	t.Cleanup(func() { _ = c.eventStore.Close() })
	c.refreshBeliefs(nil)
	if failAdmission {
		c.SetEventJournal(&failBeliefAdmission{EventJournal: c.EventJournal(), fail: true})
	}
	snapshot := bindBeliefTestRevision(t, c)
	task := TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Verify: "true", Action: &Action{Capability: "structured-actions", Type: "apply"}, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	spec := TodoSpec{PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, Agent: task.Agent, Desc: task.Goal, Action: task.Action, Verify: task.Verify, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	if _, err := c.admitTaskOccurrence(t.Context(), todoItemFromSpec(spec, ids[0]), ids[0], 1); err != nil {
		t.Fatal(err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeRuntimeAction(t.Context(), task, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if c.todoItemByID(items[0].ID).Status != TaskDone {
		t.Fatal("belief bookkeeping changed verified execution status")
	}
	return c, marker
}

func bindBeliefTestRevision(t *testing.T, c *Coordinator) *RunInputSnapshot {
	t.Helper()
	c.session.RunInputDefinitions = []RunInputDefinition{{Name: "revision", Schema: RunInputSchema{Type: "string"}, Required: true}}
	c.SetRunInputAssignments([]RunInputAssignment{{Name: "revision", RawValue: []byte(`"fixed-synthetic-revision"`), Source: RunInputSourceCLI}})
	if err := c.resolveRunInputsForInvocation(t.Context(), "synthetic fixture inputs"); err != nil {
		t.Fatal(err)
	}
	snapshot := c.RunInputSnapshot()
	if snapshot == nil {
		t.Fatal("typed input admission produced no snapshot")
	}
	return snapshot
}

func assertBeliefActionState(t *testing.T, c *Coordinator, marker string) string {
	t.Helper()
	views := c.BeliefStates()
	if len(views) != 1 || len(views[0].Epochs) != 1 {
		t.Fatalf("runtime beliefs=%+v", views)
	}
	epoch := views[0].Epochs[0]
	if epoch.State == nil || epoch.State.Status != belief.Estimated || !epoch.State.Synthetic || !epoch.Closed || (*epoch.State.Distribution)[0].Units != 142857143 {
		t.Fatalf("belief epoch=%+v", epoch)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("action was replayed: data=%q error=%v", data, err)
	}
	return epoch.State.Digest
}

func TestBeliefRuntimeActionAndExpendableCache(t *testing.T) {
	c, marker := runBeliefAction(t, false)
	digest := assertBeliefActionState(t, c, marker)
	before, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	c.beliefRuntime = nil // Discarding the snapshot must require no source execution.
	c.refreshBeliefs(nil)
	if assertBeliefActionState(t, c, marker) != digest {
		t.Fatal("reconstruction changed the state digest")
	}
	after, err := c.eventStore.ReadEvents()
	if err != nil || len(after) != len(before) {
		t.Fatalf("reconstruction appended duplicate bookkeeping: error=%v", err)
	}
	copyView := c.BeliefStates()
	(*copyView[0].Epochs[0].State.Distribution)[0].Units = 0
	if assertBeliefActionState(t, c, marker) != digest {
		t.Fatal("caller mutated canonical belief state")
	}
}

func TestBeliefRuntimeRepairsAdmissionWithoutRepeatingAction(t *testing.T) {
	c, marker := runBeliefAction(t, true)
	views := c.BeliefStates()
	if len(views) != 1 || len(views[0].Diagnostics) == 0 {
		t.Fatalf("failed admission was invisible: %+v", views)
	}
	c.refreshBeliefs(nil)
	assertBeliefActionState(t, c, marker)
}

func TestBeliefRuntimeMissingArtifactNeverReloadsTeamModel(t *testing.T) {
	c, marker := runBeliefAction(t, false)
	assertBeliefActionState(t, c, marker)
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != EventBeliefModelBound {
			continue
		}
		var payload beliefEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(c.session.Workspace, "logs", "artifacts", "data", payload.ModelRef.ID)); err != nil {
			t.Fatal(err)
		}
	}
	c.beliefRuntime = nil
	c.refreshBeliefs(nil)
	views := c.BeliefStates()
	if len(views) != 1 || views[0].Models[0].Unavailable != "model_unavailable" || views[0].Epochs[0].State != nil {
		t.Fatalf("missing artifact reconstructed from mutable team config: %+v", views)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatal("missing artifact replayed action")
	}
}

func TestBeliefRuntimeWorkerVerificationUsesFrozenEventBinding(t *testing.T) {
	worker := &staleVerifyWorkerAgent{}
	c, _ := newWP05RetryTestCoordinator(t, worker)
	c.session.Dir = t.TempDir()
	writeBeliefModel(t, c.session.Dir)
	c.session.Config.Belief = &agent.BeliefConfig{ModelFile: "model.json", Experiment: "comparison"}
	if err := compileTeamBelief(c.session); err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	t.Cleanup(func() { _ = c.eventStore.Close() })
	c.refreshBeliefs(nil)
	snapshot := bindBeliefTestRevision(t, c)
	spec := TodoSpec{Agent: "worker", Desc: "do work", Verify: "true", RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	if _, err := c.admitTaskOccurrence(t.Context(), todoItemFromSpec(spec, ids[0]), ids[0], 1); err != nil {
		t.Fatal(err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(items[0]), items[0].ID); err != nil {
		t.Fatal(err)
	}
	views := c.BeliefStates()
	if len(views) != 1 || len(views[0].Epochs) != 1 || views[0].Epochs[0].State == nil || views[0].Epochs[0].State.Status != belief.Estimated || worker.calls != 1 {
		t.Fatalf("worker verification did not enter belief state: %+v calls=%d", views, worker.calls)
	}
	digest := views[0].Epochs[0].State.Digest
	c.beliefRuntime = nil
	c.refreshBeliefs(nil)
	if c.BeliefStates()[0].Epochs[0].State.Digest != digest || worker.calls != 1 {
		t.Fatal("worker replay changed the posterior or ran the worker again")
	}
}

func TestBeliefRuntimeRepairsForkGapOnlyOnSelectedBranch(t *testing.T) {
	f := newBeliefFixture(t)
	if err := f.journal.BindVerificationModel(t.Context(), f.model, "comparison"); err != nil {
		t.Fatal(err)
	}
	if err := f.journal.OpenEpoch(t.Context(), f.epoch); err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	f.tree.Branches["child"] = &SessionBranch{ID: "child", Name: "child", ParentID: "main", ForkEventID: events[len(events)-1].ID}
	f.tree.ActiveBranch = "child"
	if err := SaveSessionTree(f.workspace, f.tree); err != nil {
		t.Fatal(err)
	}
	f.store.SetBranchID("child")
	digest, err := belief.ModelDigest(f.model)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: f.workspace, Belief: &CompiledBelief{model: f.model, snapshot: ExecutionBeliefSnapshot{ModelDigest: digest, ReducerVersion: belief.ReducerVersion, ExperimentID: "comparison"}}}, eventStore: f.store}
	c.SetEventJournal(eventStoreJournal{f.store})
	c.refreshBeliefs(nil)
	views := c.BeliefStates()
	if len(views) != 1 || len(views[0].Epochs) != 2 {
		t.Fatalf("fork repair=%+v", views)
	}
	estimated := 0
	for _, epoch := range views[0].Epochs {
		if epoch.State != nil && epoch.State.Status == belief.Estimated {
			estimated++
			if epoch.Epoch.Scope.BranchID != "child" {
				t.Fatal("child repair wrote an ancestor epoch")
			}
		}
	}
	if estimated != 1 || f.state(t).State.Status != belief.Prior {
		t.Fatal("fork repair changed main's belief distribution")
	}
}

func TestBeliefEventVerificationDoesNotBindOtherReceipts(t *testing.T) {
	base := ExecutionReceipt{RunID: "run-1", TaskID: "task-1", Attempt: 1, OccurrenceAttempt: 1, ModelExecutionID: "current"}
	previous := base
	previous.ModelExecutionID = "previous"
	verification := &VerificationResult{ExitCode: 0, Fingerprint: "verification-1"}
	for _, attempt := range []int{1, 2} {
		payload, err := json.Marshal(map[string]any{"execution_receipt": base, "execution_receipts": []ExecutionReceipt{previous, base}, "verify_result": verification, "attempt": attempt, "run_input_snapshot_hash": "fixed-input"}, jsonv1.FormatDurationAsNano(true))
		if err != nil {
			t.Fatal(err)
		}
		receipts, err := beliefSourceReceipts(RunEvent{RunID: "run-1", TaskID: "task-1", Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if len(receipts) != 2 || receipts[0].VerifyResult != nil || receipts[0].RunInputSnapshotHash != "" {
			t.Fatal("event-wide result was copied onto a different execution")
		}
		if (receipts[1].VerifyResult != nil) != (attempt == 1) || (receipts[1].RunInputSnapshotHash != "") != (attempt == 1) {
			t.Fatal("event result escaped its exact current occurrence binding")
		}
	}
}

func TestBeliefVerifierWithoutReceiptCannotBecomeObservation(t *testing.T) {
	f := newBeliefFixture(t)
	payload := []byte(`{"id":"task-1","status":"error","verify_result":{"exit_code":1,"fingerprint":"verification-1"}}`)
	event := RunEvent{Type: "task_failed", RunID: f.scope.RunID, TaskID: f.scope.TaskID, Payload: payload}
	if err := admitBeliefSource(t.Context(), f.journal, f.model, "comparison", event, nil); err == nil {
		t.Fatal("unbound verifier result was admitted")
	}
}

func TestBeliefRuntimeRestartPreservesCompletedExecution(t *testing.T) {
	first, marker := runBeliefAction(t, false)
	digest := assertBeliefActionState(t, first, marker)
	first.saveCheckpoint()
	if err := first.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	saved := LoadSession(first.session.Workspace)
	if saved == nil {
		t.Fatal("missing durable checkpoint")
	}
	restarted := &Coordinator{session: first.session, taskTracker: NewTaskTracker(), executionRunID: "run-after-restart", reportStatus: func(StatusEvent) {}}
	restarted.SetSessionData(saved)
	restarted.initEventStore()
	if restarted.eventStore == nil {
		t.Fatal("restart could not open canonical event store")
	}
	t.Cleanup(func() { _ = restarted.eventStore.Close() })
	if restarted.sessionData.RecoveryRequired {
		t.Fatalf("belief bookkeeping caused execution projection drift: %s", restarted.sessionData.RecoveryReason)
	}
	restarted.refreshBeliefs(nil)
	if _, err := restarted.ResumeInterruptedTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range restarted.BeliefStates() {
		if view.RunID == "run-belief-action" {
			found = len(view.Epochs) == 1 && view.Epochs[0].State != nil && view.Epochs[0].State.Digest == digest
		}
	}
	if !found {
		t.Fatal("restart lost the historical belief digest")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatal("restart repeated a completed action")
	}
	for _, item := range restarted.taskTracker.TodoList().Items() {
		if item.Status != TaskDone {
			t.Fatalf("restart changed completed execution status: %s", item.Status)
		}
	}
}
