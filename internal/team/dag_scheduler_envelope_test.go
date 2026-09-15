package team

import (
	"testing"
)

func mustNewDAGScheduler(t *testing.T, c *Coordinator, tasks []TaskDef, items []*TodoItem, duplicates map[int]bool) *dagScheduler {
	t.Helper()
	if len(items) == 0 {
		items = make([]*TodoItem, len(tasks))
	}
	preparedItems := make([]*TodoItem, len(tasks))
	runtimeItems := make([]*TodoItem, len(tasks))
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = string(rune('a' + i))
		if i < len(items) && items[i] != nil && items[i].ID != "" {
			ids[i] = items[i].ID
		}
	}
	envelopes := make([]TaskExecutionEnvelope, len(tasks))
	for i := range tasks {
		var original *TodoItem
		if i < len(items) {
			original = items[i]
		}
		item := testTodoItemFromTask(tasks[i], ids[i], ids)
		if original != nil {
			runtimeItems[i] = original
		} else {
			runtimeItems[i] = item
		}
		if item.ID == "" {
			item.ID = ids[i]
		}
		if item.ResourceScopeSnapshot == nil {
			claims, err := normalizeResourceClaims(authoredResourceClaims(tasks[i]))
			if err != nil {
				t.Fatalf("normalize test resource claims: %v", err)
			}
			mode := ResourceRead
			if isMutationSideEffect(item.SideEffect) {
				mode = ResourceExclusive
			}
			root, err := NewWorkspacePathResourceClaim(".", mode)
			if err != nil {
				t.Fatal(err)
			}
			claims, err = normalizeResourceClaims(append(claims, root))
			if err != nil {
				t.Fatal(err)
			}
			item.ResourceScopeSnapshot = &TaskResourceScopeSnapshot{Version: taskResourceScopeSnapshotVersion, Claims: claims, Source: resourceScopeSourceRuntimeFallback}
			item.ResourceScopeSnapshot.Digest = taskResourceScopeDigest(item.ResourceScopeSnapshot)
		}
		projection, err := newTaskOccurrenceProjection(item)
		if err != nil {
			t.Fatalf("project test task occurrence: %v", err)
		}
		occurrenceDigest, err := decisionOccurrenceInputDigest(projection)
		if err != nil {
			t.Fatalf("digest test task occurrence: %v", err)
		}
		preparedItems[i] = item
		envelopes[i] = TaskExecutionEnvelope{
			OccurrenceDigest: occurrenceDigest, LogicalToolsetDigest: "test-logical-toolset",
			ResourceScopeDigest: item.ResourceScopeSnapshot.Digest,
			ResourceScope:       EffectiveTaskResourceScope{Claims: append([]ResourceClaim(nil), item.ResourceScopeSnapshot.Claims...), Source: item.ResourceScopeSnapshot.Source},
		}
	}
	scheduler, err := newDAGScheduler(c, tasks, preparedItems, envelopes, duplicates)
	if err != nil {
		t.Fatalf("new DAG scheduler: %v", err)
	}
	scheduler.todoItems = runtimeItems
	return scheduler
}

func testTodoItemFromTask(task TaskDef, id string, ids []string) *TodoItem {
	item := &TodoItem{
		ID: id, PlanTaskID: task.ID, PlanFirst: task.PlanFirst, PlanID: task.PlanID, Phase: task.Phase,
		Action: cloneActionPtr(task.Action), ActionInputBindings: append([]ActionInputBinding(nil), task.ActionInputBindings...),
		RunInputSnapshotID: task.RunInputSnapshotID, RunInputSnapshotHash: task.RunInputSnapshotHash,
		MaterializedActionPayloadHash: task.MaterializedActionPayloadHash, BoundInputs: cloneStringMap(task.BoundInputs),
		InvariantVerification: task.InvariantVerification, ContractID: task.ContractID, ContractHash: task.ContractHash,
		ContractRevision: task.ContractRevision, Agent: task.Agent, Goal: task.Goal, Desc: task.Goal, Constraints: task.Constraints,
		Model: task.Model, ModelTopology: cloneModelTopology(task.ModelTopology), ExecutionTarget: task.ResolvedExecutionTarget,
		ExecutionTopology: cloneExecutionTopology(task.ExecutionTopology), Sidecar: task.Sidecar, Summarize: task.Summarize,
		OutputMode: task.OutputMode, ContextFiles: append([]string(nil), task.ContextFiles...), Requires: append([]string(nil), task.Requires...),
		Verify: task.Verify, VerifyMode: task.VerifyMode, VerifySpec: cloneVerificationSpecPtr(task.VerifySpec), MaxRetries: task.MaxRetries,
		OnFailureClasses: append([]TaskFailureClass(nil), task.OnFailureClasses...), Escalate: task.Escalate,
		AdversarialVerify: task.AdversarialVerify, SideEffect: task.SideEffect, Recovery: task.Recovery, ReconcileTool: task.ReconcileTool,
		Execution: cloneExecutionContract(task.Execution), Optional: task.Optional, ResourceClaims: append([]string(nil), task.ResourceClaims...),
		Resources: append([]ResourceClaim(nil), task.Resources...), Kind: task.Kind, Advances: append([]string(nil), task.Advances...),
		ExpectedStateChange: task.ExpectedStateChange, RecoveryHypothesis: cloneRecoveryHypothesis(task.RecoveryHypothesis),
		WorksetBinding: cloneWorksetBinding(task.WorksetBinding), DecisionProfile: task.DecisionProfile,
		DecisionOptions: append([]DecisionOption(nil), task.DecisionOptions...), DecisionAssumptions: cloneDecisionAssumptions(task.DecisionAssumptions),
		DecisionFacts: cloneDecisionFacts(task.DecisionFacts), DecisionArtifacts: append([]ArtifactRef(nil), task.DecisionArtifacts...),
		DecisionBaseRates: cloneBaseRateEvidence(task.DecisionBaseRates), DecisionProvenance: cloneEvidenceProvenance(task.DecisionProvenance),
		SubagentProvider: task.SubagentProvider,
	}
	for _, dep := range task.DependsOn {
		if dep >= 0 && dep < len(ids) {
			item.DependsOn = append(item.DependsOn, ids[dep])
		}
	}
	if task.OnFailure != nil && *task.OnFailure >= 0 && *task.OnFailure < len(ids) {
		item.OnFailure = ids[*task.OnFailure]
	}
	return item
}
