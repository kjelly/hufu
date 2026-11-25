package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/execution"
)

func collectionFrozenPathProducer(t *testing.T, c *Coordinator, snapshot *RunInputSnapshot, input *ArtifactRef, paths []string, lineInventory string) *WorksetBinding {
	t.Helper()
	definition := TaskDef{ID: "path-source", Agent: "worker", Phase: PhasePrepare, SideEffect: SideEffectNone, Recovery: RecoveryManual}
	hash, err := effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
	if err != nil {
		t.Fatal(err)
	}
	c.session.ContractTasks = append(c.session.ContractTasks, definition)
	c.initEventStore()
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventRunInputsResolved), RunID: c.executionRunID, Payload: mustProjectionJSON(t, snapshot)})
	target := execution.ExecutionTarget{Backend: execution.OllamaBackendName, Model: "test"}
	spec := TodoSpec{Agent: "worker", Model: "ollama/test", ExecutionTarget: target, ExecutionTopology: []execution.ExecutionTarget{target}, PlanTaskID: definition.ID, ContractID: definition.ID, ContractHash: hash, ContractRevision: effectiveTaskContractRevision, Phase: definition.Phase, SideEffect: SideEffectNone, Recovery: RecoveryManual, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
		t.Fatal(err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatal(err)
	}
	item := items[0]
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "produce immutable scope", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, item.Agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordAssistantOutput("Produced the immutable assigned scope."); err != nil {
		t.Fatal(err)
	}
	sealed, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if lineInventory == "" {
		lineInventory = "frozen assigned path evidence"
	}
	assigned, err := store.Put(t.Context(), PutArtifactRequest{Kind: "source", Path: "assigned-source.json", Content: []byte(lineInventory), RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
	if err != nil {
		t.Fatal(err)
	}
	*input = assigned.ArtifactRef
	manifest, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "sample", Bindings: map[string]string{"identity": "frozen-sample"}, TouchedPaths: paths, Inputs: []ArtifactRef{*input}}}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(t.Context(), PutArtifactRequest{Kind: "workset_manifest", Path: "paths.json", Content: manifest, RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: item.Agent})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: item.ID, Attempt: 1, Agent: item.Agent, Status: TaskResultStatusSuccess, Source: "runtime", Summary: "prepared", Artifacts: []ArtifactRef{*input, source.ArtifactRef}}
	c.storeSubmittedTaskResult(item.ID, result)
	receipt := &ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, OccurrenceAttempt: 1, ProducerID: item.Agent, ModelExecutionID: "path-producer", TranscriptRef: sealed.ID, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: new(0), RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskInProgress, TaskDone, "prepared", "prepared", map[string]any{"execution_receipt": receipt}); err != nil {
		t.Fatal(err)
	}
	expanded, err := c.expandFanOutTasks(t.Context(), []TaskDef{{ID: "research", Agent: "worker", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: definition.ID, Artifact: source.ID}, GoalTemplate: "Review the immutable source."}}})
	if err != nil || len(expanded) != 1 {
		t.Fatalf("expand assigned paths: %v", err)
	}
	return expanded[0].WorksetBinding
}

func TestAcceptedWorksetPathRepairRetainsWorkAcrossRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"restart", "cancelled", "assigned_path_out_of_bounds"} {
		t.Run(scenario, func(t *testing.T) {
			location := "dir/soruce.go:7"
			repaired := "dir/source.go:7"
			fixture := contractCollectionFixtureOptions{Accepted: true, ValidAccepted: true, ArtifactKeys: true, FrozenPaths: []string{"dir/source.go"}}
			if scenario == "assigned_path_out_of_bounds" {
				location, repaired = "dir/short.go:67", "dir/source.go:67"
				fixture.FrozenPaths = append(fixture.FrozenPaths, "dir/short.go")
				fixture.FrozenLineSpans = worksetLineInventory
			}
			response := strings.Replace(researchCollectionResponse(t, 2), `"structured_payload":{`, `"structured_payload":{"locations":["`+location+`"],`, 1)
			c, source, id := contractCollectionFixture(t, response, SideEffectNone, fixture)
			original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
			before := mustReadEvents(t, c.eventStore)
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			var proof *AcceptedResultRepair
			for _, task := range plan.Tasks {
				if task.ID == id && task.Collection != nil {
					proof = task.Collection.AcceptedResult
				}
			}
			if plan.Repaired != 1 || proof == nil || !reflect.DeepEqual(proof.PathPointers, []string{"/locations/0"}) {
				t.Fatalf("path repair plan: %+v", plan)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "path-reference-repair", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			if err := c.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			_, compiled, err := c.boundResultContract(id)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := c.verifiedWorksetPathSelection(t.Context(), id, compiled.worksetPaths)
			if err != nil {
				t.Fatal(err)
			}
			var key string
			for candidate, path := range binding.paths {
				if path == "dir/source.go" {
					key = candidate
				}
			}
			if scenario == "assigned_path_out_of_bounds" {
				wrong := WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{{Pointer: "/locations/0", Reference: "dir/short.go"}})
				a, err := c.AssessResultCollectionRecovery(wrong, id)
				if err != nil || a.Disposition != RecoveryBlocked {
					t.Fatalf("the wrong assigned file remained selectable: %+v, %v", a, err)
				}
			}
			selected := WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{{Pointer: "/locations/0", Reference: key}})
			assessment, err := c.AssessResultCollectionRecovery(selected, id)
			if err != nil || assessment.Disposition != RecoverySelectReferences || assessment.AdditionalTurns != 0 {
				t.Fatalf("path selection: %+v, %v", assessment, err)
			}
			if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(selected, id)); err != nil {
				t.Fatal(err)
			}
			granted, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, granted)
			worker := scriptWorker(textTurn("must never repeat the old work"))
			c.workerAgentOverride = worker
			runCtx, cancel := context.WithCancel(WithSessionResume(t.Context()))
			t.Cleanup(cancel)
			if scenario == "cancelled" {
				cancel()
			}
			_, runErr := c.executeTask(runCtx, taskDefFromTodoItem(c.todoItemByID(id)), id)
			if scenario == "cancelled" {
				if runErr == nil || c.todoItemByID(id).TypedResult != nil || worker.Calls() != 0 {
					t.Fatal("cancelled selection published or reran work")
				}
				saved, err := ReadResumeSource(c.session.Workspace, branch, "")
				if err != nil {
					t.Fatal(err)
				}
				c = reopenContractCollection(t, c, saved)
				c.executionRunID = "cancelled-path-selection"
				c.runInputSelectionRunID = c.executionRunID
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(selected, id)); err != nil {
					t.Fatal(err)
				}
				c.workerAgentOverride = worker
				_, runErr = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id)
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			item := c.todoItemByID(id)
			if worker.Calls() != 0 || item.Status != TaskDone || currentResultCollection(item).TurnsUsed != currentResultCollection(original).TurnsUsed || currentResultCollection(item).Work != currentResultCollection(original).Work {
				t.Fatal("path selection replayed work or changed the spent budget")
			}
			if !strings.Contains(string(item.TypedResult.StructuredPayload.Value), `"locations":["`+repaired+`"]`) {
				t.Fatal("exact path selection was not preserved")
			}
			got, err := acceptedResultSemanticHashWithPaths(item.TypedResult, proof.Pointers, proof.PathPointers)
			if err != nil || got != proof.SemanticSHA256 {
				t.Fatal("accepted path repair changed line or semantic claims")
			}
			events := mustReadEvents(t, c.eventStore)
			if !reflect.DeepEqual(before, events[:len(before)]) {
				t.Fatal("repair rewrote historical events")
			}
			replayed, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil || todoItemByID(replayed.Session.Tasks, id).Status != TaskDone {
				t.Fatalf("repaired path could not replay: %v", err)
			}

		})
	}
}

func TestAcceptedWorksetPathRepairRejectsInvalidAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"foreign_path", "changed_line", "foreign_pointer", "missing_namespace_proof", "legacy_revision", "mutable_proof"} {
		t.Run(scenario, func(t *testing.T) {
			response := strings.Replace(researchCollectionResponse(t, 2), `"structured_payload":{`, `"structured_payload":{"locations":["dir/soruce.go:7"],`, 1)
			c, source, id := contractCollectionFixture(t, response, SideEffectNone, contractCollectionFixtureOptions{Accepted: true, ValidAccepted: true, ArtifactKeys: true, FrozenPaths: []string{"dir/source.go"}})
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "path-authority", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			_, compiled, err := c.boundResultContract(id)
			if err != nil {
				t.Fatal(err)
			}
			choices, err := c.verifiedWorksetPathSelection(t.Context(), id, compiled.worksetPaths)
			if err != nil {
				t.Fatal(err)
			}
			var key string
			for candidate := range choices.paths {
				key = candidate
			}
			selection := AcceptedReferenceSelection{Pointer: "/locations/0", Reference: key}
			switch scenario {
			case "foreign_path":
				selection.Reference = "dir/foreign.go"
			case "changed_line":
				selection.Reference = key + ":8"
			case "foreign_pointer":
				selection.Pointer = "/findings/0"
			}
			ctx = WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{selection})
			a, err := c.AssessResultCollectionRecovery(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "foreign_path" || scenario == "changed_line" || scenario == "foreign_pointer" {
				if a.Disposition != RecoveryBlocked || currentResultCollection(c.todoItemByID(id)).TurnsUsed != a.TurnsUsed {
					t.Fatal("invalid selection granted authority or changed the budget")
				}
				return
			}
			if a.Disposition != RecoverySelectReferences {
				t.Fatalf("valid baseline is not selectable: %+v", a)
			}
			switch scenario {
			case "missing_namespace_proof":
				checks := a.Checks[:0]
				for _, check := range a.Checks {
					if check.Code != "workset_paths" {
						checks = append(checks, check)
					}
				}
				a.Checks = checks
			case "legacy_revision":
				a.ValidatorRevision = 12
			case "mutable_proof":
				acceptedRepairProof(a.Checkpoint).PathPointers[0] = "/findings/0"
				if acceptedRepairProof(currentResultCollection(c.todoItemByID(id))).PathPointers[0] != "/locations/0" {
					t.Fatal("returned assessment mutated the durable repair proof")
				}
			}
			a.CheckpointSHA256, _ = collectionDigest(a.Checkpoint)
			a.Fingerprint, _ = recoveryAssessmentFingerprint(a)
			if validateRecoveryAssessment(a) == nil {
				t.Fatal("invalid path proof admitted")
			}
		})
	}
}

func retainedPathCollectionFixture(t *testing.T) (*Coordinator, *ResumeSource, string, *TodoItem, []RunEvent) {
	t.Helper()
	response := strings.Replace(researchCollectionResponse(t, 2), `"structured_payload":{`, `"structured_payload":{"locations":["repository/dir/source.go:7","repository/dir/source.go:9"],`, 1)
	c, source, id := contractCollectionFixture(t, response, SideEffectNone, contractCollectionFixtureOptions{
		Accepted: true, ValidAccepted: true, ArtifactKeys: true, FrozenPaths: []string{"dir/source.go"}, PendingPeer: true,
	})
	original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
	before := mustReadEvents(t, c.eventStore)
	ctx := WithMetadataOnlyContractRecovery(t.Context())
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Retried != 1 || plan.Repaired != 0 {
		t.Fatalf("retention plan: retried=%d repaired=%d", plan.Retried, plan.Repaired)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "retain-original-collection", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c = reopenContractCollection(t, c, retained)
	retained, err = ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	return c, retained, id, original, before
}

func TestAcceptedWorksetPathRepairOfRetainedAncestor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	c, retained, id, original, before := retainedPathCollectionFixture(t)
	ctx := contractCollectionContext(t, id)
	plan, err := c.AssessContractRecovery(ctx, retained)
	if err != nil {
		t.Fatalf("accepted ancestor collection cannot be repaired: %v", err)
	}
	branch, err := c.ApplyContractRecovery(ctx, retained, "repair-retained-collection", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c = reopenContractCollection(t, c, child)
	selected := WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{
		{Pointer: "/locations/0", Reference: "dir/source.go"}, {Pointer: "/locations/1", Reference: "dir/source.go"},
	})
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(selected, id)); err != nil {
		t.Fatal(err)
	}
	worker := scriptWorker(textTurn("must not repeat accepted work"))
	c.workerAgentOverride = worker
	if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
		t.Fatal(err)
	}
	item := c.todoItemByID(id)
	if worker.Calls() != 0 || item.Status != TaskDone || currentResultCollection(item).Work != currentResultCollection(original).Work || currentResultCollection(item).TurnsUsed != currentResultCollection(original).TurnsUsed {
		t.Fatal("repair replayed work or changed its sealed work or spent budget")
	}
	if !strings.Contains(string(item.TypedResult.StructuredPayload.Value), `"locations":["dir/source.go:7","dir/source.go:9"]`) {
		t.Fatal("ancestor repair did not preserve original source lines")
	}
	if !reflect.DeepEqual(before, mustReadEvents(t, c.eventStore)[:len(before)]) {
		t.Fatal("ancestor repair rewrote original evidence")
	}
	resumed, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c = reopenContractCollection(t, c, resumed)
	if err := c.validateResumeSources(WithSessionResume(t.Context())); err != nil {
		t.Fatalf("ordinary resume rejected the repaired retained collection: %v", err)
	}
	if hasCommittedWorkflowCollectionRepair(c.todoItemByID(id), branch) {
		t.Fatal("an accepted result reference repair must not prove recovery from an unrelated workflow failure")
	}
}

func TestAcceptedWorksetPathRepairRejectsForgedAncestor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"event", "event_hash", "task_digest", "contract_hash", "checkpoint", "claims"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id, _, _ := retainedPathCollectionFixture(t)
			plan, err := c.AssessContractRecovery(contractCollectionContext(t, id), source)
			if err != nil {
				t.Fatal(err)
			}
			var patch *ContractRecoveryTask
			for i := range plan.Tasks {
				if plan.Tasks[i].ID == id {
					patch = &plan.Tasks[i]
				}
			}
			if patch == nil || patch.Collection == nil || patch.Collection.SourceOrigin == nil {
				t.Fatal("ancestor source proof is missing")
			}
			p := patch.Collection
			switch scenario {
			case "event":
				p.SourceOrigin.EventID = "foreign-completion"
			case "event_hash":
				p.SourceOrigin.EventHash = strings.Repeat("a", 64)
			case "task_digest":
				p.SourceOrigin.TaskSHA256 = strings.Repeat("a", 64)
			case "contract_hash":
				p.SourceOrigin.ContractHash = strings.Repeat("a", 64)
			case "checkpoint":
				p.Original.PolicySHA256 = strings.Repeat("a", 64)
			case "claims":
				changed := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
				changed.TypedResult.Summary = "replace an accepted judgment"
				if _, err := originalCollectionRepairItem(changed, p); !errors.Is(err, errResultCollectionStale) {
					t.Fatalf("changed retained claims were not rejected: %v", err)
				}
				return
			}
			plan.SHA256, err = recoveryPlanHash(*plan)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: "invalid-origin-recovery", Type: string(EventContractRecovery), BranchID: "forged-child", Payload: mustProjectionJSON(t, plan)}
			if _, err := validateContractRecoveryLineage(event, source.lineage, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("forged ancestor proof passed lineage admission")
			}
			rows, err := ReplayTodoList(append(cloneRunEvents(source.lineage), event))
			if err == nil && todoItemByID(rows, id).Status != TaskDone {
				t.Fatal("replay applied a forged ancestor repair")
			}
		})
	}
}

func TestAcceptedWorksetPathRepairRequiresAncestorGrantProof(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	c, source, id, _, _ := retainedPathCollectionFixture(t)
	ctx := contractCollectionContext(t, id)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "ancestor-grant", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	source, err = ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c = reopenContractCollection(t, c, source)
	selected := WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{
		{Pointer: "/locations/0", Reference: "dir/source.go"}, {Pointer: "/locations/1", Reference: "dir/source.go"},
	})
	for _, scenario := range []string{"missing_check", "old_revision", "mutable_proof"} {
		t.Run(scenario, func(t *testing.T) {
			a, err := c.AssessResultCollectionRecovery(selected, id)
			if err != nil || a.Disposition != RecoverySelectReferences {
				t.Fatalf("valid ancestor selection failed: %v, %+v", err, a)
			}
			switch scenario {
			case "missing_check":
				for i, check := range a.Checks {
					if check.Code == "accepted_origin" {
						a.Checks = append(a.Checks[:i], a.Checks[i+1:]...)
						break
					}
				}
			case "old_revision":
				a.ValidatorRevision = 13
			case "mutable_proof":
				a.Checkpoint.ContractUpgrade.SourceOrigin.ContractHash = strings.Repeat("a", 64)
				if currentResultCollection(c.todoItemByID(id)).ContractUpgrade.SourceOrigin.ContractHash == strings.Repeat("a", 64) {
					t.Fatal("assessment mutated the durable origin proof")
				}
			}
			a.CheckpointSHA256, _ = collectionDigest(a.Checkpoint)
			a.Fingerprint, _ = recoveryAssessmentFingerprint(a)
			if err := c.validateResultRepairGrant(selected, c.todoItemByID(id), TaskProtocolIncomplete, c.collectionResultRepairGrant(a)); err == nil {
				t.Fatal("unverified ancestor grant was accepted")
			}
		})
	}
}

func TestAcceptedWorksetPathRepairRejectsChangedAncestorProducer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"retried_producer", "planned_producer_retry"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id, _, _ := retainedPathCollectionFixture(t)
			producerID := c.todoItemByID(id).WorksetBinding.SourceArtifact.TaskID
			ctx := contractCollectionContext(t, id)
			if scenario == "retried_producer" {
				if err := c.CommitTaskResetForRetry(t.Context(), producerID, "producer no longer accepted"); err != nil {
					t.Fatal(err)
				}
				var err error
				source, err = ReadResumeSource(c.session.Workspace, source.BranchID, "")
				if err != nil {
					t.Fatal(err)
				}
				before := mustReadEvents(t, c.eventStore)
				if _, err := c.AssessContractRecovery(ctx, source); err == nil {
					t.Fatal("ancestor evidence concealed a retried producer")
				}
				if !reflect.DeepEqual(before, mustReadEvents(t, c.eventStore)) {
					t.Fatal("rejected ancestor assessment changed durable state")
				}
				return
			}
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			for i := range plan.Tasks {
				if plan.Tasks[i].ID == producerID {
					plan.Tasks[i].Action, plan.Tasks[i].Revalidation = "retry", nil
					plan.Retried++
					plan.Retained--
				}
			}
			plan.SHA256, _ = recoveryPlanHash(*plan)
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: "producer-retry-recovery", Type: string(EventContractRecovery), BranchID: "unsafe-child", Payload: mustProjectionJSON(t, plan)}
			if _, err := validateContractRecoveryLineage(event, source.lineage, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("result repair admitted a planned producer retry")
			}
			replayed, err := ReplayTodoList(append(cloneRunEvents(source.lineage), event))
			if err == nil && (todoItemByID(replayed, id).Status != TaskDone || todoItemByID(replayed, producerID).Status != TaskDone) {
				t.Fatal("rejected producer retry partially changed the replay")
			}
		})
	}
}
