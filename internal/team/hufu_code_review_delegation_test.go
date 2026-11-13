package team

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// This fixture starts at the durable handoff after the supplement producer,
// rather than treating completion of that native action as workflow completion.
// Results are accepted fixture inputs; this test exercises delegation, immutable
// workset expansion, checkpoint restoration and aggregate verification.
func TestHufuCodeReviewSupplementReusesReviewerAfterRestore(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("supplement_items_%d", count), func(t *testing.T) {
			c := newTestCoordinator(t)
			workspace := c.session.Workspace
			c.session = loadHufuCodeReviewTeam(t)
			c.session.Workspace = workspace
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				t.Fatal(err)
			}
			producer := addAcceptedReviewFixtureTask(t, c, "produce-workset")
			addAcceptedReviewFixtureTask(t, c, "verify-targeted-go-tests")
			primary := reviewFixtureWorkset(t, c, store, producer, "review-primary-workset", 2)
			commitReviewFixtureWorkset(t, c, primary)
			addAcceptedReviewFixtureTask(t, c, "review-documentation-workset")
			addAcceptedReviewFixtureTask(t, c, "review-documentation-escalation")
			addAcceptedReviewFixtureTask(t, c, "inventory-review-supplement-requests")
			supplement := addAcceptedReviewFixtureTask(t, c, "prepare-review-supplement")
			reviewFixtureWorkset(t, c, store, supplement, "review-supplement-workset", count)
			if supplement.ID != "8" {
				t.Fatalf("fixture supplement task = %s, want original boundary 8", supplement.ID)
			}

			// Cross the actual checkpoint format and restore into a new coordinator.
			checkpoint := NewSession()
			checkpoint.Tasks = c.taskTracker.TodoList().Items()
			if err := SaveSession(workspace, checkpoint); err != nil {
				t.Fatal(err)
			}
			restored, _, err := LoadSessionReadOnly(workspace)
			if err != nil || restored == nil {
				t.Fatalf("restore checkpoint: %v", err)
			}
			fresh := newTestCoordinator(t)
			fresh.session = c.session
			fresh.taskTracker.TodoList().Restore(restored.Tasks)
			before, err := json.Marshal(fresh.taskTracker.TodoList().Items())
			if err != nil {
				t.Fatal(err)
			}
			// Re-expand from immutable producer artifacts, not an in-memory binding.
			if count > 0 {
				next := expandReviewFixtureContract(t, fresh, "review-supplement-workset")
				if err := fresh.validateDelegationPolicy(next); err != nil {
					t.Fatalf("distinct supplemental workset rejected after restored reviewer success: %v", err)
				}
				children := commitReviewFixtureWorkset(t, fresh, next)
				verification := VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "review-supplement-workset", WorksetRequireTerminal: true, WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess}}
				children[0].VerifyResult.ExitCode = 1
				if result, err := fresh.executeWorksetCompleteVerification(t.Context(), verification); err == nil && result != nil && result.ExitCode == 0 {
					t.Fatal("unverified supplement incorrectly satisfied aggregate verification")
				}
				children[0].VerifyResult.ExitCode = 0
				if result, err := fresh.executeWorksetCompleteVerification(t.Context(), verification); err != nil || result == nil || result.ExitCode != 0 {
					t.Fatalf("supplement aggregate verification: result=%+v err=%v", result, err)
				}
				assertReviewFixtureRedispatchRejected(t, fresh, "review-supplement-workset")
			} else {
				// The coordinator must skip an empty supplemental manifest;
				// expansion must still fail before a new Todo can be created.
				contract := reviewFixtureContract(t, fresh, "review-supplement-workset")
				if _, err := fresh.expandFanOutTasks([]TaskDef{contract}); err == nil {
					t.Fatal("empty supplemental manifest was dispatched")
				}
			}
			assertReviewFixtureRedispatchRejected(t, fresh, "review-primary-workset")
			after, err := json.Marshal(fresh.taskTracker.TodoList().Items()[:8])
			if err != nil || string(after) != string(before) {
				t.Fatalf("original eight accepted results changed: %v", err)
			}

			// Native inventory and audit remain admissible after model reuse.
			for _, requests := range [][]TaskDef{
				{{Agent: "reviewer", ContractID: "inventory-review-handoffs"}},
				{{Agent: "synthesizer", ContractID: "synthesize-review"}, {Agent: "reviewer", ContractID: "verify-review-report"}},
			} {
				bound, _, err := CompileTaskGoalContracts(fresh.session, requests)
				if err != nil {
					t.Fatal(err)
				}
				if err := fresh.validateDelegationPolicy(bound); err != nil {
					t.Fatalf("downstream inventory/synthesis/audit rejected: %v", err)
				}
			}
			// Documentation escalation already ran as critic. Each distinct source
			// still needs an independent critic-review occurrence.
			for _, source := range []string{"3", "4"} {
				bound, _, err := CompileTaskGoalContracts(fresh.session, []TaskDef{{Agent: "critic", ContractID: "critic-review", EvidenceFrom: []string{source}}})
				if err != nil {
					t.Fatal(err)
				}
				if err := fresh.validateDelegationPolicy(bound); err != nil {
					t.Fatal(err)
				}
				if _, err := fresh.bindEvidenceSources(bound); err != nil {
					t.Fatal(err)
				}
				addAcceptedReviewFixtureTask(t, fresh, "critic-review")
			}
			addAcceptedReviewFixtureTask(t, fresh, "synthesize-review")
			for _, id := range []string{"review-documentation-workset", "synthesize-review"} {
				contract := reviewFixtureContract(t, fresh, id)
				if err := fresh.validateDelegationPolicy([]TaskDef{contract}); err == nil || !strings.Contains(err.Error(), "successful terminal results") {
					t.Fatalf("one-shot role %s lost protection: %v", contract.Agent, err)
				}
			}
			finalAudit := reviewFixtureContract(t, fresh, "verify-review-report")
			if err := fresh.validateDelegationPolicy([]TaskDef{finalAudit}); err != nil {
				t.Fatalf("native audit blocked after synthesis: %v", err)
			}
		})
	}
}

func reviewFixtureContract(t *testing.T, c *Coordinator, id string) TaskDef {
	t.Helper()
	i := slices.IndexFunc(c.session.ContractTasks, func(task TaskDef) bool { return task.ID == id })
	if i < 0 {
		t.Fatalf("missing real team contract %q", id)
	}
	return c.session.ContractTasks[i]
}

func addAcceptedReviewFixtureTask(t *testing.T, c *Coordinator, id string) *TodoItem {
	t.Helper()
	contract := reviewFixtureContract(t, c, id)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: contract.Agent, PlanTaskID: id, ContractID: id, Action: contract.Action}})[0]
	item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "accepted fixture"}
	if item.Action != nil {
		item.TypedResult.Source = "runtime"
	}
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "accepted fixture")
	return item
}

func reviewFixtureWorkset(t *testing.T, c *Coordinator, store *FileArtifactStore, producer *TodoItem, id string, count int) []TaskDef {
	t.Helper()
	contract := reviewFixtureContract(t, c, id)
	manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{}}
	for i := range count {
		key := fmt.Sprintf("%s-%d", id, i)
		manifest.Items = append(manifest.Items, WorksetItem{Key: key, Bindings: map[string]string{
			"key": key, "review_revision": "frozen-head", "lens": "general", "reference_candidate_ids": "[]",
			"source_task_id": "3", "source_gap_index": "0", "supplement_kind": "missing_source",
		}})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), PutArtifactRequest{Content: data, Path: id + ".json", Kind: "workset_manifest", Description: contract.FanOut.SourceArtifact.Artifact, RunID: c.executionRunID, TaskID: producer.ID, Attempt: 1, Agent: producer.Agent})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = append(producer.TypedResult.Artifacts, artifact.ArtifactRef)
	if count == 0 {
		return nil
	}
	return expandReviewFixtureContract(t, c, id)
}

func expandReviewFixtureContract(t *testing.T, c *Coordinator, id string) []TaskDef {
	t.Helper()
	contract := reviewFixtureContract(t, c, id)
	bound, _, err := CompileTaskGoalContracts(c.session, []TaskDef{{Agent: contract.Agent, ContractID: id}})
	if err != nil {
		t.Fatal(err)
	}
	children, err := c.expandFanOutTasks(bound)
	if err != nil {
		t.Fatal(err)
	}
	return children
}

func commitReviewFixtureWorkset(t *testing.T, c *Coordinator, children []TaskDef) []*TodoItem {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(len(children))
	receipts, err := buildWorksetReceipts(children, ids, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]*TodoItem, len(children))
	for i, child := range children {
		items[i] = todoItemFromSpec(TodoSpec{Agent: child.Agent, PlanTaskID: child.ID, ContractID: child.ContractID, WorksetBinding: child.WorksetBinding}, ids[i])
		items[i].WorksetReceipt = receipts[child.WorksetBinding.WorksetID]
		items[i].TypedResult = &TaskResult{TaskID: ids[i], Agent: child.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "accepted review"}
		items[i].VerifyResult = &VerificationResult{ExitCode: 0}
	}
	c.taskTracker.TodoList().AddReserved(items)
	for _, item := range items {
		c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "accepted review")
	}
	return items
}

func assertReviewFixtureRedispatchRejected(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	before := c.taskTracker.TodoList().Items()
	children := expandReviewFixtureContract(t, c, id)
	if err := c.validateDelegationPolicy(children); err == nil || !strings.Contains(err.Error(), "committed receipt") {
		t.Fatalf("duplicate %s lost immutable workset protection: %v", id, err)
	}
	if !reflect.DeepEqual(before, c.taskTracker.TodoList().Items()) {
		t.Fatal("rejected workset dispatch changed accepted tasks")
	}
}
