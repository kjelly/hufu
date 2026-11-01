package team

import (
	"encoding/json/v2"
	"io"
	"strings"
	"testing"
)

func newRecoveredArtifactCoordinator(t *testing.T, configure ...func(*Coordinator)) *Coordinator {
	t.Helper()
	c, _, source := metadataContractRecoveryFixture(t, configure...)
	ctx := WithMetadataOnlyContractRecovery(t.Context())
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "artifact-recovery", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(child.Session)
	c.executionRunID = "resumed-artifact-run"
	if err := c.selectRecoveryRunInputSnapshot(&child.Session.RunInputSnapshots[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRecoveredArtifactScopeRequiresCanonicalOrigin(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Coordinator, []*TodoItem)
	}{
		{name: "accepted"},
		{name: "inputs_not_selected", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { c.runInputSelectionRunID = "" }},
		{name: "different_invocation", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { c.executionRunID = "another-run" }},
		{name: "missing_canonical_store", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { c.eventStore = nil }},
		{name: "changed_result", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { items[0].TypedResult.Summary = "forged" }},
		{name: "changed_verification", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { items[0].VerifyResult.ExitCode = 1 }},
		{name: "changed_receipt", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) {
			item := items[0]
			item.ExecutionReceipt.ProducerID = "forged"
			for i := range item.ExecutionReceipts {
				item.ExecutionReceipts[i].ProducerID = "forged"
			}
		}},
		{name: "changed_source_inputs", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { items[0].RunInputSnapshotHash = "changed" }},
		{name: "source_not_completed", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { items[0].Status = TaskPending }},
		{name: "changed_artifact_attempt", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) { items[1].WorksetBinding.Inputs[0].Attempt++ }},
		{name: "unassigned_producer_artifact", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) {
			items[1].WorksetBinding.Inputs[0] = items[0].TypedResult.Artifacts[1]
		}},
		{name: "changed_expansion_run", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) {
			for _, item := range items[1:] {
				if item.WorksetReceipt != nil {
					item.WorksetReceipt.RunID = "forged"
				}
			}
		}},
		{name: "forged_current_expansion", mutate: func(_ *testing.T, c *Coordinator, items []*TodoItem) {
			for _, item := range items[1:] {
				if item.WorksetReceipt != nil {
					item.WorksetReceipt.RunID = c.executionRunID
				}
			}
		}},
		{name: "different_branch", mutate: func(t *testing.T, c *Coordinator, _ []*TodoItem) {
			tree, err := LoadSessionTree(c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tree.CreateRootBranch("unrelated"); err != nil {
				t.Fatal(err)
			}
			if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
				t.Fatal(err)
			}
			c.eventStore.SetBranchID("unrelated")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newRecoveredArtifactCoordinator(t)
			if test.mutate != nil {
				func() {
					list := c.taskTracker.TodoList()
					list.mu.Lock()
					defer list.mu.Unlock()
					// Corrupt only the loaded projection, without publishing it as
					// a new canonical event through Restore's checkpoint callback.
					test.mutate(t, c, list.items)
				}()
			}
			scope, err := c.buildArtifactAccessScope("2", 1)
			if test.mutate != nil {
				if err == nil {
					t.Fatal("unadmitted recovered artifact was authorized")
				}
				return
			}
			if err != nil || len(scope.AuthorizedRefs) != 1 || scope.RunID != c.executionRunID || scope.AuthorizedRefs[0].RunID != "source-run" {
				t.Fatalf("scope=%+v err=%v", scope, err)
			}
			origins, err := c.runtimeActionRecoveryOrigins()
			if err != nil || len(origins) != 1 || origins["1"] != "source-run" {
				t.Fatalf("native adapter lost canonical recovery origins: origins=%v err=%v", origins, err)
			}
			reader, err := c.openArtifactRef(t.Context(), scope.AuthorizedRefs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil || string(body) != "original assigned input" {
				t.Fatalf("body=%q read=%v close=%v", body, readErr, closeErr)
			}
			if err := c.verifyWorksetSource(t.Context(), c.todoItemByID("2").WorksetReceipt); err != nil {
				t.Fatal(err)
			}
			items := c.taskTracker.TodoList().Items()
			for _, item := range items[1:] {
				id := item.ID
				item.Status = TaskDone
				item.TypedResult = &TaskResult{TaskID: id, Status: TaskResultStatusSuccess}
				item.VerifyResult = &VerificationResult{ExitCode: 0}
			}
			c.taskTracker.TodoList().Restore(items)
			result, err := c.executeWorksetCompleteVerification(t.Context(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "source", WorksetRequireTerminal: true, WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess}})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("verification=%+v err=%v", result, err)
			}
		})
	}
}

func TestRecoveredArtifactActionEnvironmentRejectsInheritedGrants(t *testing.T) {
	for _, granted := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "admitted"}[granted], func(t *testing.T) {
			t.Setenv("HUFU_RECOVERED_TASK_RUNS", `{"forged":"unrelated-run"}`)
			var origins map[string]string
			if granted {
				origins = map[string]string{"1": "source-run"}
			}
			ctx := WithActionEnvironment(t.Context(), ActionEnvironment{RecoveredTaskRuns: origins})
			if granted {
				origins["1"] = "mutated"
				envSnapshot := ActionEnvironmentFromContext(ctx)
				envSnapshot.RecoveredTaskRuns["1"] = "mutated-again"
			}
			seen := 0
			for _, entry := range actionCommandEnvironment(ctx) {
				if value, ok := strings.CutPrefix(entry, "HUFU_RECOVERED_TASK_RUNS="); ok {
					seen++
					var actual map[string]string
					err := json.Unmarshal([]byte(value), &actual)
					valid := len(actual) == 0
					if granted {
						valid = len(actual) == 1 && actual["1"] == "source-run"
					}
					if err != nil || !valid {
						t.Fatalf("runtime authority was inherited or mutated: origins=%v err=%v", actual, err)
					}
				}
			}
			if seen != 1 {
				t.Fatalf("recovery environment has %d competing grant values", seen)
			}
		})
	}
}

func TestRecoveredArtifactResumeRejectsChangedWorksetReceipt(t *testing.T) {
	c := newRecoveredArtifactCoordinator(t)
	list := c.taskTracker.TodoList()
	list.mu.Lock()
	for _, item := range list.items[1:] {
		if item.WorksetReceipt != nil {
			item.WorksetReceipt.RunID = "forged"
		}
	}
	list.mu.Unlock()
	err := c.validateResumeSources(WithSessionResume(t.Context()))
	if err == nil {
		t.Fatalf("altered loaded receipt reached dispatch: %v", err)
	}
}

func TestRecoveredArtifactForkPreservesPendingWorkAndOriginalReceipts(t *testing.T) {
	c := newRecoveredArtifactCoordinator(t)
	parent := c.eventStore.BranchID()
	checkpoint, err := c.eventStore.AppendPersistedContext(t.Context(), RunEvent{Type: string(EventRunStarted), RunID: c.executionRunID, Actor: "coordinator", Payload: mustProjectionJSON(t, map[string]string{"team": c.session.Config.Name})})
	if err != nil {
		t.Fatal(err)
	}
	failed := c.todoItemByID("2")
	failed.Status = TaskError
	appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventTaskFailed), TaskID: failed.ID, RunID: c.executionRunID, Payload: mustProjectionJSON(t, failed)})
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := ForkSessionBranch(t.Context(), c.session.Workspace, tree, c.eventStore, "retry-before-preflight", checkpoint.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := ReadResumeSource(c.session.Workspace, parent, "")
	if err != nil || previous.Session.Tasks[1].Status != TaskError || child.Session.Tasks[1].Status != TaskPending {
		t.Fatalf("fork changed history or retained the later failure: err=%v", err)
	}
	c.eventStore.SetBranchID(branch.ID)
	c.SetSessionData(child.Session)
	c.executionRunID = "retry-invocation"
	if err := c.selectRecoveryRunInputSnapshot(&child.Session.RunInputSnapshots[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.validateResumeSources(WithSessionResume(t.Context())); err != nil {
		t.Fatal(err)
	}
	scope, err := c.buildArtifactAccessScope("2", 1)
	if err != nil || len(scope.AuthorizedRefs) != 1 || scope.AuthorizedRefs[0].RunID != "source-run" {
		t.Fatalf("fork lost original artifact authorization: scope=%+v err=%v", scope, err)
	}
}
