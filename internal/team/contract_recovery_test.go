package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func contractRecoveryFixture(t *testing.T, configure ...func(*Coordinator)) (*Coordinator, *versionFixture, *ResumeSource) {
	t.Helper()
	f, _ := derivedForkPreparationFixture(t)
	return contractRecoveryFixtureFromState(t, f, configure...)
}

func contractRecoveryFixtureFromState(t *testing.T, f *versionFixture, configure ...func(*Coordinator)) (*Coordinator, *versionFixture, *ResumeSource) {
	t.Helper()
	c := newTestCoordinator(t)
	c.session.Workspace = f.control
	c.session.Config.Name = "review"
	c.session.Config.Delegation.BindTaskGoalContracts = true
	c.session.WorkspaceVersion = f.version
	if err := c.session.SetCompatibilityWorkspaceScope(f.subject); err != nil {
		t.Fatal(err)
	}
	c.projectDir = f.subject
	c.eventStore = f.events
	c.SetEventJournal(eventStoreJournal{store: f.events})
	c.session.Config.DefaultLLMBackend = "ollama"
	c.session.Agents["worker"].Generation.Model = "ollama/test"
	c.maxConcurrent = 1
	manager, err := agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	old, err := CompileResultContractDocument("result.json", []byte(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	oldRef := old.ref(true)
	for _, id := range []string{"source", "failed", "consumer"} {
		c.session.ContractTasks = append(c.session.ContractTasks, TaskDef{ID: id, Agent: "worker", Phase: PhaseExecute, SideEffect: SideEffectNone, Recovery: RecoveryRetry, ResultContract: &oldRef, ResultContractSpec: &resultContractSpec{Schema: old.ID, RequireStructured: true}, Execution: ExecutionContract{RequiresResult: true}})
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{old.ID: old}
	for _, setup := range configure {
		if setup != nil {
			setup(c)
		}
	}
	if err = c.prepareRecoveryValidation(); err != nil {
		t.Fatal(err)
	}
	oldPolicy := c.ExecutionPolicySnapshot()
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventExecutionPolicySnapshot), Payload: mustProjectionJSON(t, oldPolicy)})
	snapshot, err := ResolveRunInputSnapshot(c.session.RunInputDefinitions, nil, "source-run", "source-inv", "review")
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventRunInputsResolved), RunID: "source-run", Payload: mustProjectionJSON(t, snapshot)})
	c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
	input, err := mustArtifactStore(t, f.control).Put(t.Context(), PutArtifactRequest{Kind: "input", Path: "assigned.txt", Content: []byte("original assigned input"), RunID: "source-run", TaskID: "1", Attempt: 1, Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	manifestRows := []WorksetItem{{Key: "failed", Bindings: map[string]string{"label": "frozen-value"}, Inputs: []ArtifactRef{input.ArtifactRef}}, {Key: "consumer", Bindings: map[string]string{"label": "frozen-value"}, Inputs: []ArtifactRef{input.ArtifactRef}}}
	manifestData, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: manifestRows})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := mustArtifactStore(t, f.control).Put(t.Context(), PutArtifactRequest{Kind: "workset_manifest", Path: "workset.json", Content: manifestData, RunID: "source-run", TaskID: "1", Attempt: 1, Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	workset := &WorksetExpansionReceipt{WorksetID: worksetID(manifest.ID, manifest.SHA256, manifestRows), ItemKeysSHA256: worksetItemKeysDigest(manifestRows), RunID: "source-run", ParentTaskID: "source", SourceArtifactID: manifest.ID, SourceSHA256: manifest.SHA256, SourceArtifact: manifest.ArtifactRef, ItemCount: 2, Children: map[string]string{"failed": "2", "consumer": "3"}}
	var tasks []*TodoItem
	for i, definition := range c.session.ContractTasks {
		id := string(rune('1' + i))
		hash, err := effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
		if err != nil {
			t.Fatal(err)
		}
		definition.ContractHash = hash
		materialized, err := c.materializeTaskActions([]TaskDef{definition})
		if err != nil {
			t.Fatal(err)
		}
		bound := materialized[0]
		payload, err := validateStructuredResultPayload(old, oldRef, []byte(`{"value":"verified"}`))
		if err != nil {
			t.Fatal(err)
		}
		transcript, err := mustArtifactStore(t, f.control).Put(t.Context(), PutArtifactRequest{Kind: "task_transcript", Path: "source-" + id + ".jsonl", Content: []byte("original observations " + id), RunID: "source-run", TaskID: id, Attempt: 1, Agent: "worker"})
		if err != nil {
			t.Fatal(err)
		}
		receipt := &ExecutionReceipt{RunID: "source-run", TaskID: id, Attempt: 1, ModelExecutionID: "original-" + id, ProducerID: "worker", TranscriptRef: transcript.ID, StartedAt: time.Now(), ExitCode: new(0), RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ResultContractID: oldRef.ID, ResultContractSHA256: oldRef.SchemaSHA256, ResultPayloadSHA256: payload.SHA256, ResultValidation: ResultValidationValid}
		item := &TodoItem{ID: id, Agent: "worker", Desc: "original goal", Goal: "original goal", Status: TaskDone, Phase: definition.Phase, ContractID: definition.ID, ContractHash: bound.ContractHash, Action: bound.Action, ActionInputBindings: bound.ActionInputBindings, MaterializedActionPayloadHash: bound.MaterializedActionPayloadHash, BoundInputs: bound.BoundInputs, ContractRevision: effectiveTaskContractRevision, SideEffect: SideEffectNone, Recovery: RecoveryRetry, Execution: definition.Execution, ResultContract: &oldRef, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipt: receipt, ExecutionReceipts: []ExecutionReceipt{*receipt}, TypedResult: &TaskResult{TaskID: id, Status: TaskResultStatusSuccess, Summary: "accepted", StructuredPayload: payload}, VerifyResult: &VerificationResult{ExitCode: 0}}
		item.TypedResult.Attempt, item.TypedResult.Agent = 1, "worker"
		if i == 0 {
			item.TypedResult.Artifacts = []ArtifactRef{input.ArtifactRef, manifest.ArtifactRef}
			if bound.Action != nil {
				outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{"satisfied": true}})
				if err != nil {
					t.Fatal(err)
				}
				item.TypedResult.Source = "runtime"
				item.TypedResult.RuntimeOutputs, item.TypedResult.RuntimeOutputsHash = outputs, digest
				item.TypedResult.RunInputSnapshotID, item.TypedResult.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
				item.TypedResult.MaterializedActionPayloadHash, item.TypedResult.BoundInputs = bound.MaterializedActionPayloadHash, bound.BoundInputs
				item.ExecutionReceipt.ActionInvocationID, item.ExecutionReceipt.RuntimeOutputsHash = "native-source", digest
				item.ExecutionReceipt.MaterializedActionPayloadHash, item.ExecutionReceipt.BoundInputs = bound.MaterializedActionPayloadHash, bound.BoundInputs
				item.ExecutionReceipts = []ExecutionReceipt{*item.ExecutionReceipt}
			}
		} else {
			item.WorksetBinding = &WorksetBinding{WorksetID: workset.WorksetID, ParentTaskID: workset.ParentTaskID, ItemKey: definition.ID, Bindings: map[string]string{"label": "frozen-value"}, Inputs: []ArtifactRef{input.ArtifactRef}, SourceArtifactID: manifest.ID, SourceSHA256: manifest.SHA256, SourceArtifact: manifest.ArtifactRef}
			// Runtime dispatch records one receipt on the first child; all
			// siblings use its committed child map rather than private copies.
			if i == 1 {
				item.WorksetReceipt = cloneWorksetReceipt(workset)
			}
		}
		if i == 1 {
			item.Status = TaskBlocked
			item.TypedResult = nil
			item.ExecutionReceipt = nil
			item.ExecutionReceipts = nil
		}
		if i == 2 {
			item.DependsOn = []string{"2"}
			item.EvidenceFrom = []string{"1", "2"}
		}
		tasks = append(tasks, item)
		appendBranchEvent(t, f.events, RunEvent{Type: string(EventTaskCreated), TaskID: id, RunID: "source-run", Payload: mustProjectionJSON(t, item)})
	}
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "source-run", Payload: []byte(`{"state":"FAILED"}`)})
	c.taskTracker.TodoList().Restore(tasks)
	session := ReduceToSessionData(mustReadEvents(t, f.events))
	if err = SaveSession(f.control, session); err != nil {
		t.Fatal(err)
	}
	next, err := CompileResultContractDocument("result.json", []byte(`{"type":"object","description":"new result representation","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[next.ID] = next
	newRef := next.ref(true)
	for i := range c.session.ContractTasks {
		c.session.ContractTasks[i].ResultContract = &newRef
	}
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, f, source
}

func TestContractRecoveryPreservesParentAndRevalidatesChild(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*Coordinator)
		metadata  bool
	}{
		{name: "unbound"},
		{name: "input_bound_action", configure: configureInputBoundContractRecovery},
		{name: "metadata_only", metadata: true},
		{name: "metadata_inherited_effect", metadata: true, configure: func(c *Coordinator) {
			c.session.ContractTasks[0].SideEffect = ""
			c.session.Agents["worker"].SideEffect = string(SideEffectNone)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var c *Coordinator
			var f *versionFixture
			var source *ResumeSource
			recoveryCtx := t.Context()
			if test.metadata {
				c, f, source = metadataContractRecoveryFixture(t, test.configure)
				recoveryCtx = WithMetadataOnlyContractRecovery(recoveryCtx)
			} else {
				c, f, source = contractRecoveryFixture(t, test.configure)
			}
			before := mustReadEvents(t, f.events)
			plan, err := c.AssessContractRecovery(recoveryCtx, source)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Retained != 1 || plan.Retried != 2 || plan.Tasks[0].Revalidation == nil {
				t.Fatalf("unexpected plan: %+v", plan)
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("preview wrote events")
			}
			if _, err = c.ApplyContractRecovery(recoveryCtx, source, "bad", strings.Repeat("0", 64)); err == nil {
				t.Fatal("stale plan accepted")
			}
			if test.metadata {
				if _, err := c.ApplyContractRecovery(t.Context(), source, "wrong-mode", plan.SHA256); err == nil {
					t.Fatal("metadata plan applied without its explicit mode")
				}
			}
			branch, err := c.ApplyContractRecovery(recoveryCtx, source, "recovered", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := LoadSessionTree(f.control)
			if err != nil {
				t.Fatal(err)
			}
			events := mustReadEvents(t, f.events)
			parent, err := ProjectValidatedEventsForBranch(events, tree, "main")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, parent[:len(before)]) {
				t.Fatal("recovery rewrote parent history")
			}
			child, err := ReadResumeSource(f.control, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			if child.Session.ExecutionPolicySnapshot.ConfigurationHash != plan.Policy.ConfigurationHash || child.Session.WorkflowState != PhaseExecute {
				t.Fatal("child did not admit new policy or reopen workflow")
			}
			if child.metadataOnly != test.metadata {
				t.Fatal("resume source lost recovery mode")
			}
			retained := child.Session.Tasks[0]
			original := source.Session.Tasks[0]
			if !sameCanonicalEvidence(retained.ExecutionReceipt, original.ExecutionReceipt) || !sameCanonicalEvidence(retained.ExecutionReceipts, original.ExecutionReceipts) || string(retained.TypedResult.StructuredPayload.Value) != string(original.TypedResult.StructuredPayload.Value) || !validContractRevalidation(retained, retained.ExecutionReceipt) {
				t.Fatal("revalidation changed native receipts or payload bytes")
			}
			if child.Session.Tasks[1].Status != TaskPending || child.Session.Tasks[2].Status != TaskPending || child.Session.Tasks[2].TypedResult != nil {
				t.Fatal("impacted consumers were reused")
			}
			c.SetSessionData(child.Session)
			c.executionRunID = "recovery-dispatch"
			if err = c.selectRecoveryRunInputSnapshot(&child.Session.RunInputSnapshots[0]); err != nil {
				t.Fatal(err)
			}
			if err = c.AdmitExecutionPolicy(); err != nil {
				t.Fatal(err)
			}
			if err = c.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			c.taskTracker.TodoList().Restore(child.Session.Tasks)
			for _, id := range []string{"2", "3"} {
				scope, err := c.buildArtifactAccessScope(id, 1)
				if err != nil || len(scope.AuthorizedRefs) != 1 || scope.AuthorizedRefs[0].RunID != "source-run" {
					t.Fatalf("recovered workset dispatch lost its original artifact authority: scope=%+v err=%v", scope, err)
				}
			}
			if err := c.verifyWorksetSource(t.Context(), child.Session.Tasks[1].WorksetReceipt); err != nil {
				t.Fatal(err)
			}
			if test.name == "input_bound_action" {
				value, err := c.resolveRuntimeOutputValue(FactRef{Name: "scope", TaskID: "source"}, "scope")
				if err != nil || !sameCanonicalEvidence(value, map[string]any{"satisfied": true}) {
					t.Fatalf("retained native output was not delivered: value=%v err=%v", value, err)
				}
				verification, err := c.executeTaskOutputAssertVerification(t.Context(), VerificationSpec{Type: VerifyTaskOutputAssert, WorksetSourceTask: "source", TaskOutputName: "scope", Assertions: []TaskOutputAssertion{{Pointer: "/satisfied", Op: "equals", Value: true}}})
				if err != nil || verification.ExitCode != 0 {
					t.Fatalf("retained native output failed acceptance: result=%+v err=%v", verification, err)
				}
			}
			if test.name == "metadata_inherited_effect" {
				c.session.Agents["worker"].SideEffect = string(SideEffectExternalWrite)
				if err = c.validateResumeSources(WithSessionResume(t.Context())); err == nil {
					t.Fatal("metadata resume accepted a writing worker default")
				}
				c.session.Agents["worker"].SideEffect = string(SideEffectNone)
			}
			if err = c.validateCompletedResumeEvidence(t.Context(), retained, &source.Session.RunInputSnapshots[0]); err != nil {
				t.Fatal(err)
			}
			lineage, err := ProjectValidatedEventsForBranch(events, tree, branch)
			if err != nil {
				t.Fatal(err)
			}
			if err = CompareCanonicalProjection(child.Session, lineage); err != nil {
				t.Fatal(err)
			}
			if test.metadata {
				entries, err := os.ReadDir(f.version.StateDir)
				if err != nil || len(entries) != 0 {
					t.Fatal("metadata recovery created a workspace version store")
				}
			}
			// Replaying the committed event does not consult historical schema files.
			reloaded := ReduceToSessionData(lineage)
			if !sameCanonicalEvidence(reloaded.Tasks, child.Session.Tasks) {
				t.Fatal("replay changed the recovery result")
			}
		})
	}
}

func TestContractRecoveryRefusesUnsettledOrChangedAuthority(t *testing.T) {
	for _, name := range []string{"model", "scope", "action", "side_effect", "corrupt_payload", "source_head"} {
		t.Run(name, func(t *testing.T) {
			c, f, source := contractRecoveryFixture(t)
			switch name {
			case "model":
				c.session.Agents["worker"].Generation.Model = "ollama/other"
			case "scope":
				c.session.ContractTasks[0].Execution.ForbidArtifacts = true
			case "action":
				c.session.ContractTasks[0].Action = &Action{Type: "changed", Capability: "changed"}
			case "side_effect":
				source.Session.Tasks[1].SideEffect = SideEffectExternalWrite
			case "corrupt_payload":
				source.Session.Tasks[0].TypedResult.StructuredPayload.Value = []byte(`{"value":"tampered"}`)
			case "source_head":
				appendBranchEvent(t, f.events, RunEvent{Type: "unrelated-observation", Payload: []byte(`{}`)})
			}
			before := mustReadEvents(t, f.events)
			plan, err := c.AssessContractRecovery(t.Context(), source)
			if name == "source_head" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.ApplyContractRecovery(t.Context(), source, "stale", plan.SHA256)
			}
			if err == nil {
				t.Fatal("unsafe contract recovery accepted")
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("refusal changed events")
			}
		})
	}
}

func TestContractRecoveryEventRejectsTamperedSourceOrPolicy(t *testing.T) {
	c, f, source := contractRecoveryFixture(t)
	plan, err := c.AssessContractRecovery(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, f.events)
	for _, name := range []string{"same_branch", "source_task", "policy", "hash"} {
		t.Run(name, func(t *testing.T) {
			var changed ContractRecoveryPlan
			if err := json.Unmarshal(mustProjectionJSON(t, plan), &changed); err != nil {
				t.Fatal(err)
			}
			branch := "child"
			switch name {
			case "same_branch":
				branch = "main"
			case "source_task":
				changed.Tasks[0].SourceSHA256 = strings.Repeat("a", 64)
			case "policy":
				changed.Policy.TeamMaxConcurrent++
				changed.Policy.ConfigurationHash, _ = executionPolicyConfigurationHash(changed.Policy)
			case "hash":
				changed.SHA256 = "invalid"
			}
			if name != "hash" {
				changed.SHA256, err = recoveryPlanHash(changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: branch, Payload: mustProjectionJSON(t, changed)}
			if _, err = validateContractRecoveryLineage(event, before, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("tampered recovery event accepted")
			}
		})
	}
}

func TestContractRecoveryInterruptedForkKeepsOriginalPolicy(t *testing.T) {
	c, f, source := contractRecoveryFixture(t)
	plan, err := c.AssessContractRecovery(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	crashAt(t, "fork:event_committed")
	if _, err = c.ApplyContractRecovery(t.Context(), source, "interrupted", plan.SHA256); err == nil {
		t.Fatal("simulated crash was ignored")
	}
	recoverFixture(t, f)
	c.eventStore = f.events
	c.SetEventJournal(eventStoreJournal{store: f.events})
	events := mustReadEvents(t, f.events)
	for _, event := range events {
		if event.Type == string(EventContractRecovery) {
			t.Fatal("partial fork acquired new policy")
		}
	}
	child, err := ReadResumeSource(f.control, "interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.ExecutionPolicySnapshot.ConfigurationHash != source.Session.ExecutionPolicySnapshot.ConfigurationHash {
		t.Fatal("incomplete publication changed policy")
	}
	matches, err := c.executionPolicySnapshotMatchesCurrent(child.Session.ExecutionPolicySnapshot)
	if err == nil && matches {
		t.Fatal("ordinary resume admitted partial publication under changed schema")
	}
}

func TestContractRecoveryRetriesIncompatiblePayloadWithoutRewritingJudgments(t *testing.T) {
	c, _, source := contractRecoveryFixture(t)
	incompatible, err := CompileResultContractDocument("result.json", []byte(`{"type":"object","required":["value"],"properties":{"value":{"const":"different"}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[incompatible.ID] = incompatible
	ref := incompatible.ref(true)
	for i := range c.session.ContractTasks {
		c.session.ContractTasks[i].ResultContract = &ref
	}
	original := string(source.Session.Tasks[0].TypedResult.StructuredPayload.Value)
	plan, err := c.AssessContractRecovery(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Retained != 0 || plan.Retried != 3 || plan.Tasks[0].Revalidation != nil {
		t.Fatal("incompatible result was reused")
	}
	if string(source.Session.Tasks[0].TypedResult.StructuredPayload.Value) != original {
		t.Fatal("assessment rewrote original judgment")
	}
}

func TestContractRecoveryDoesNotReuseProofForNewNativeExecution(t *testing.T) {
	c, _, source := contractRecoveryFixture(t)
	if err := c.prepareRecoveryValidation(); err != nil {
		t.Fatal(err)
	}
	_, _, bindings, err := c.contractRecoveryPolicies(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	item := cloneTodoItem(source.Session.Tasks[0])
	item.ContractRevalidation = &ContractRevalidation{Original: ResultContractRef{ID: "older.json", SchemaSHA256: strings.Repeat("a", 64)}, Accepted: *item.ResultContract, PayloadSHA256: item.TypedResult.StructuredPayload.SHA256, SourceTaskSHA256: strings.Repeat("b", 64)}
	tasks := map[string]TaskDef{}
	for _, task := range c.session.ContractTasks {
		tasks[task.ID] = task
	}
	patch, retry, err := c.assessContractRecoveryTask(t.Context(), item, source.Session, tasks, bindings)
	if err != nil || retry || patch.Revalidation == nil || patch.Revalidation.Original != *item.ResultContract {
		t.Fatalf("new native receipt inherited obsolete proof: %+v retry=%v err=%v", patch, retry, err)
	}
}

func configureInputBoundContractRecovery(c *Coordinator) {
	c.session.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true, Default: []byte(`{"count":2}`)}}
	c.session.ContractTasks[0].Action = &Action{Capability: "inventory", Type: "read", Payload: `{"scope":{"count":1},"limit":10}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}
}

func TestContractRecoveryChecksInputBoundTaskContracts(t *testing.T) {
	c, f, source := contractRecoveryFixture(t, configureInputBoundContractRecovery)
	before := mustReadEvents(t, f.events)
	original := source.Session.Tasks[0]
	bindings := map[string]ExecutionResultContractPolicySnapshot{}
	for _, candidate := range source.Session.ExecutionPolicySnapshot.ResultContracts {
		bindings[candidate.Owner] = candidate
	}
	for _, test := range []struct {
		name   string
		mutate func(*TaskDef, *TodoItem, *SessionData)
	}{
		{name: "unchanged"},
		{name: "payload", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) { item.Action.Payload = `{}` }},
		{name: "action_capability", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) { item.Action.Capability = "other" }},
		{name: "binding", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) { item.ActionInputBindings[0].Input = "other" }},
		{name: "missing_binding", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) { item.ActionInputBindings = nil }},
		{name: "bound_input", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) {
			item.BoundInputs["scope"] = runInputHash([]byte(`{"count":99}`))
		}},
		{name: "payload_hash", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) {
			item.MaterializedActionPayloadHash = runInputHash([]byte(`{}`))
		}},
		{name: "missing_snapshot", mutate: func(_ *TaskDef, _ *TodoItem, session *SessionData) { session.RunInputSnapshots = nil }},
		{name: "snapshot_hash", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) {
			item.RunInputSnapshotHash = runInputHash([]byte("other"))
		}},
		{name: "snapshot_value", mutate: func(_ *TaskDef, _ *TodoItem, session *SessionData) {
			session.RunInputSnapshots[0].Inputs[0].CanonicalValue = []byte(`{"count":99}`)
		}},
		{name: "input_schema", mutate: func(_ *TaskDef, _ *TodoItem, session *SessionData) {
			session.ExecutionPolicySnapshot.RunInputSchemaHash = runInputHash([]byte("other"))
		}},
		{name: "input_policy", mutate: func(_ *TaskDef, _ *TodoItem, session *SessionData) {
			session.ExecutionPolicySnapshot.RunInputPolicyHash = runInputHash([]byte("other"))
		}},
		{name: "contract_hash", mutate: func(_ *TaskDef, item *TodoItem, _ *SessionData) { item.ContractHash = strings.Repeat("0", 64) }},
		{name: "static_payload", mutate: func(definition *TaskDef, _ *TodoItem, _ *SessionData) {
			definition.Action.Payload = `{"scope":{"count":1},"limit":99}`
		}},
		{name: "static_binding", mutate: func(definition *TaskDef, _ *TodoItem, _ *SessionData) { definition.Action.InputBindings = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := cloneTaskDef(c.session.ContractTasks[0])
			item := cloneTodoItem(original)
			session := *source.Session
			session.RunInputSnapshots = CloneRunInputSnapshots(source.Session.RunInputSnapshots)
			session.ExecutionPolicySnapshot = cloneExecutionPolicySnapshot(source.Session.ExecutionPolicySnapshot)
			if test.mutate != nil {
				test.mutate(&definition, item, &session)
			}
			patch, retry, err := c.assessContractRecoveryTask(t.Context(), item, &session, map[string]TaskDef{definition.ID: definition}, bindings)
			if test.mutate != nil {
				if err == nil {
					t.Fatal("changed action identity accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if retry || patch.ContractHash != item.ContractHash || patch.Revalidation == nil {
				t.Fatalf("unchanged materialized action not retained: %+v", patch)
			}
		})
	}
	if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
		t.Fatal("assessment changed source history")
	}
}

func TestContractRecoveryPreservesUnusedBackendEnvironment(t *testing.T) {
	for _, name := range []string{"unused", "reachable"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HUFU_CONTRACT_RECOVERY_TEST", "before")
			c, f, source := contractRecoveryFixture(t, func(c *Coordinator) {
				cfg := defaultExecutionPolicyCodexConfig()
				cfg.InheritEnv = []string{"HUFU_CONTRACT_RECOVERY_TEST"}
				c.session.Config.SubagentProviders = map[string]agent.SubagentProviderConfig{"codex": cfg}
				if name == "reachable" {
					c.session.Agents["worker"].Generation.Model = "codex/worker"
				}
			})
			before := mustReadEvents(t, f.events)
			t.Setenv("HUFU_CONTRACT_RECOVERY_TEST", "after")
			plan, err := c.AssessContractRecovery(t.Context(), source)
			if name == "reachable" {
				if err == nil {
					t.Fatal("reachable backend environment drift accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !sameCanonicalEvidence(plan.Policy.ExecutionWorlds, source.Session.ExecutionPolicySnapshot.ExecutionWorlds) {
					t.Fatal("recovery rewrote frozen backend environment")
				}
				branch, err := c.ApplyContractRecovery(t.Context(), source, "environment-recovery", plan.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				child, err := ReadResumeSource(f.control, branch, "")
				if err != nil {
					t.Fatal(err)
				}
				c.SetSessionData(child.Session)
				if err = c.AdmitExecutionPolicy(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("assessment changed source history")
			}
		})
	}
}

func metadataContractRecoveryFixture(t *testing.T, configure ...func(*Coordinator)) (*Coordinator, *versionFixture, *ResumeSource) {
	t.Helper()
	f := &versionFixture{t: t, control: t.TempDir(), subject: t.TempDir()}
	f.version = WorkspaceVersionContext{Mode: versionstore.ModeOff, WorkspaceID: "metadata_workspace", ProjectID: "metadata_project", TeamName: "review", SubjectRoot: f.subject, ControlRoot: f.control, StateDir: t.TempDir()}
	var err error
	f.events, err = NewEventStore(f.control, "source-run", "review")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.closeStores)
	f.events.SetBranchID("main")
	f.tree, err = LoadSessionTree(f.control)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	f.write("source.txt", "verified source\n")
	return contractRecoveryFixtureFromState(t, f, configure...)
}

func TestContractRecoveryMetadataOnlyRejectsUnsafeSources(t *testing.T) {
	for _, name := range []string{"versioned", "observe", "unmanaged", "dynamic_contracts", "future_write", "source_write", "version_history", "missing_opt_in", "declared_none_mutation"} {
		t.Run(name, func(t *testing.T) {
			c, f, source := metadataContractRecoveryFixture(t)
			ctx := WithMetadataOnlyContractRecovery(t.Context())
			switch name {
			case "versioned":
				c.session.WorkspaceVersion.Mode = versionstore.ModeRequired
			case "observe":
				c.session.WorkspaceVersion.Mode = versionstore.ModeObserve
			case "unmanaged":
				c.session.WorkspaceVersion.WorkspaceID = ""
			case "dynamic_contracts":
				c.session.Config.Delegation.BindTaskGoalContracts = false
			case "declared_none_mutation":
				c.session.ContractTasks[0].Execution.Steps = []ExecutionStep{{ID: "write", Effect: ExecutionEffectMutate}}
			case "future_write":
				c.session.ContractTasks[0].SideEffect = SideEffectExternalWrite
			case "source_write":
				source.Session.Tasks[0].SideEffect = SideEffectExternalWrite
			case "version_history":
				store, err := versionstore.Open(t.Context(), versionstore.Options{StateDir: f.version.StateDir})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = store.EnsureSubjectState(t.Context(), f.subject, versionstore.ModeRequired); err != nil {
					t.Fatal(err)
				}
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing_opt_in":
				ctx = t.Context()
			}
			before := mustReadEvents(t, f.events)
			if name == "source_write" {
				if err := c.validateMetadataContractRecovery(ctx, source.Session); err == nil {
					t.Fatal("source side effects accepted")
				}
			} else if name == "missing_opt_in" {
				plan, err := c.AssessContractRecovery(ctx, source)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = c.ApplyContractRecovery(ctx, source, "unsafe", plan.SHA256); err == nil {
					t.Fatal("unversioned recovery did not require opt-in")
				}
			} else {
				if _, err := c.AssessContractRecovery(ctx, source); err == nil {
					t.Fatal("unsafe metadata recovery accepted")
				}
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("refusal changed source events")
			}
			if b, err := os.ReadFile(filepath.Join(f.subject, "source.txt")); err != nil || string(b) != "verified source\n" {
				t.Fatal("metadata preflight changed project files")
			}
		})
	}
}

func TestContractRecoveryPublishesAfterSiblingBranchObservation(t *testing.T) {
	c, f, _ := metadataContractRecoveryFixture(t, nil)
	tree, err := LoadSessionTree(f.control)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ForkSessionBranch(t.Context(), f.control, tree, f.events, "peer", "main")
	if err != nil {
		t.Fatal(err)
	}
	f.events.SetBranchID(peer.ID)
	appendBranchEvent(t, f.events, RunEvent{Type: "unrelated-observation", Payload: []byte(`{}`)})
	if _, _, err := CheckoutSessionBranch(t.Context(), f.control, tree, f.events, "main"); err != nil {
		t.Fatal(err)
	}
	f.events.SetBranchID("main")
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := source.lineage
	ctx := WithMetadataOnlyContractRecovery(t.Context())
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	// A separate CLI apply reads a fresh source: the preview hash must still
	// reject a global write even when the selected branch did not change.
	f.events.SetBranchID(peer.ID)
	appendBranchEvent(t, f.events, RunEvent{Type: "unrelated-observation", Payload: []byte(`{}`)})
	f.events.SetBranchID("main")
	fresh, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyContractRecovery(ctx, fresh, "stale-after-peer", plan.SHA256); err == nil {
		t.Fatal("fresh source admitted an outdated preview after a sibling write")
	}
	source = fresh
	plan, err = c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "recovered-after-peer", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceHead == source.eventHead || plan.SourceHead != before[len(before)-1].ID {
		t.Fatal("plan used the global journal head instead of the source lineage")
	}
	child, err := ReadResumeSource(f.control, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.ExecutionPolicySnapshot.ConfigurationHash != plan.Policy.ConfigurationHash {
		t.Fatal("child policy not admitted")
	}
	tree, err = LoadSessionTree(f.control)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ProjectValidatedEventsForBranch(mustReadEvents(t, f.events), tree, "main")
	if err != nil || !reflect.DeepEqual(parent, before) {
		t.Fatalf("parent changed: %v", err)
	}
}
