package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/decisionrt"
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func readOnlyWorkRetryFixture(t *testing.T, count, settled int, endpoints ...string) (*Coordinator, *ResumeSource, context.Context, *atomic.Int32) {
	t.Helper()
	return readOnlyWorkRetryOutcomeFixture(t, count, settled, false, endpoints...)
}

func readOnlyWorkRetryOutcomeFixture(t *testing.T, count, settled int, committed bool, endpoints ...string) (*Coordinator, *ResumeSource, context.Context, *atomic.Int32) {
	t.Helper()
	var definition TaskDef
	var snapshot *RunInputSnapshot
	endpoint := "http://127.0.0.1:1/v1"
	if len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	c, item, ctx, receipt, calls := configuredDecisionCollectionFixture(t, endpoint, count, settled, func(spec *TodoSpec) {
		spec.ContractID, spec.ContractHash, spec.ContractRevision = definition.ID, definition.ContractHash, effectiveTaskContractRevision
		spec.Phase, spec.Optional, spec.SideEffect, spec.Recovery = PhaseExecute, true, SideEffectNone, RecoveryRetry
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}, func(c *Coordinator, source *TodoItem) {
		c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
		c.session.Config.Delegation.BindTaskGoalContracts = true
		c.session.WorkspaceVersion = WorkspaceVersionContext{Mode: versionstore.ModeOff, WorkspaceID: "retry_workspace", ProjectID: "retry_project", TeamName: c.session.Config.Name, SubjectRoot: c.projectDir, ControlRoot: c.session.Workspace, StateDir: t.TempDir()}
		store, err := NewEventStore(c.session.Workspace, c.executionRunID, c.session.Config.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		store.SetBranchID("main")
		c.eventStore = store
		c.SetEventJournal(eventStoreJournal{store: store})
		// Initialize the same public branch store used by normal managed execution.
		tree, err := LoadSessionTree(c.session.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if err = SaveSessionTree(c.session.Workspace, tree); err != nil {
			t.Fatal(err)
		}
		for _, spec := range []struct {
			id, schema string
			optional   bool
		}{{"source", "assets.json", false}, {"classify", "decisions.json", true}} {
			compiled := c.session.ResultContracts[spec.schema]
			ref := compiled.ref(true)
			def := TaskDef{ID: spec.id, Agent: "worker", Phase: PhaseExecute, SideEffect: SideEffectNone, Recovery: RecoveryRetry, Optional: spec.optional, ResultContract: &ref, ResultContractSpec: &resultContractSpec{Schema: ref.ID, RequireStructured: true}, Execution: ExecutionContract{RequiresResult: true}}
			def.ContractHash, err = effectiveContractHash(def.ID, def.Agent, def.Execution, def.OutputMode, def.SideEffect, def.Recovery, def.MaxRetries, def.Action, def.FanOut, def.Optional, def)
			if err != nil {
				t.Fatal(err)
			}
			c.session.ContractTasks = append(c.session.ContractTasks, def)
		}
		definition = c.session.ContractTasks[1]
		snapshot, err = ResolveRunInputSnapshot(nil, nil, c.executionRunID, "original-invocation", c.session.Config.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventRunInputsResolved), Actor: "runtime", BranchID: "main", RunID: c.executionRunID, Payload: mustProjectionJSON(t, snapshot)}); err != nil {
			t.Fatal(err)
		}
		c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
		def := c.session.ContractTasks[0]
		source.Agent, source.ContractID, source.ContractHash, source.ContractRevision = "worker", def.ID, def.ContractHash, effectiveTaskContractRevision
		source.Phase, source.SideEffect, source.Recovery, source.Execution = def.Phase, def.SideEffect, def.Recovery, def.Execution
		source.RunInputSnapshotID, source.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
		source.TypedResult.Agent, source.TypedResult.Attempt = "worker", 1
		transcript, err := mustArtifactStore(t, c.session.Workspace).Put(t.Context(), PutArtifactRequest{Kind: "task_transcript", Path: "source.jsonl", Content: []byte("verified original source"), RunID: c.executionRunID, TaskID: source.ID, Agent: source.Agent, Attempt: 1})
		if err != nil {
			t.Fatal(err)
		}
		source.ExecutionReceipt = &ExecutionReceipt{RunID: c.executionRunID, TaskID: source.ID, Attempt: 1, ProducerID: source.Agent, TranscriptRef: transcript.ID, ExitCode: new(0), StartedAt: time.Now(), FinishedAt: time.Now(), RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ResultContractID: source.ResultContract.ID, ResultContractSHA256: source.ResultContract.SchemaSHA256, ResultPayloadSHA256: source.TypedResult.StructuredPayload.SHA256, ResultValidation: ResultValidationValid}
		source.ExecutionReceipts = []ExecutionReceipt{*source.ExecutionReceipt}
		c.taskTracker.TodoList().Restore([]*TodoItem{source})
	})
	receipt.RunInputSnapshotID, receipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "Work returned without all required decisions")
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"blocked","summary":"Required decision missing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		cp, err = c.prepareCollectionClaims(ctx, cp)
		if err != nil {
			t.Fatal(err)
		}
		if err = c.commitPreparedCollection(ctx, cp); err != nil {
			t.Fatal(err)
		}
		outcome := *c.todoItemByID(item.ID).ExecutionReceipt
		outcome.ExitCode, outcome.HandoffState, outcome.SubmittedResult = new(1), ResultHandoffSubmitted, c.GetTaskResult(item.ID)
		if err = c.setAttemptReceipt(item.ID, &outcome); err != nil {
			t.Fatal(err)
		}
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "required decision missing", NeedsHuman, FailureExecution, TaskBlocked, "")
	} else {
		cp, err = c.blockResultCollection(ctx, cp, RepairFailureSource)
		if err != nil {
			t.Fatal(err)
		}
		c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureSource, Cause: errors.New("required decision missing")})
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "required decision missing", NeedsHuman, FailureProtocol, TaskBlocked, "")
	}
	failure := ExecutionError{Phase: PhaseExecute, Category: "OPTIONAL_TASK_BLOCKED", Component: item.Agent, Source: item.Agent}
	state := struct {
		State        Phase                 `json:"state"`
		PhaseResults map[Phase]PhaseResult `json:"phase_results"`
	}{PhaseFailed, map[Phase]PhaseResult{PhaseExecute: {Status: PhaseStatusBlocked, Errors: []ExecutionError{failure}}}}
	if _, err = c.EventJournal().Append(ctx, RunEvent{Type: string(EventWorkflowStateChanged), Actor: "runtime", BranchID: "main", RunID: receipt.RunID, Payload: mustProjectionJSON(t, state)}); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveSession(c.session.Workspace, ReduceToSessionData(events)); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(c.session.Workspace, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, source, WithReadOnlyWorkRetry(WithMetadataOnlyContractRecovery(t.Context()), item.ID), calls
}

func TestContractRecoveryReadOnlyWorkRetryRejectsUnsafeTargets(t *testing.T) {
	c, source, ctx, calls := readOnlyWorkRetryFixture(t, 2, 1)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	original := todoItemByID(source.Session.Tasks, plan.WorkRetry.TaskID)
	for _, name := range []string{"writing", "mutating_step", "unfinished", "failed_work", "accepted", "uncertain", "fanout", "isolated", "policy_denial"} {
		t.Run(name, func(t *testing.T) {
			item := cloneTodoItem(original)
			switch name {
			case "writing":
				item.SideEffect = SideEffectExternalWrite
			case "mutating_step":
				item.Execution.Steps = []ExecutionStep{{ID: "write", Effect: ExecutionEffectMutate}}
			case "unfinished":
				item.ExecutionReceipt.FinishedAt = time.Time{}
			case "failed_work":
				item.ExecutionReceipt.ExitCode = new(1)
			case "accepted":
				item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess}
			case "uncertain":
				item.RecoveryState = RecoveryStateUnknown
			case "fanout":
				item.ExecutionReceipt.ResultCollection.FanoutLeaf = true
			case "isolated":
				item.ExecutionReceipt.ExecutionWorldID = "other-world"
			case "policy_denial":
				item.ExecutionReceipt.ToolDispositions = append(item.ExecutionReceipt.ToolDispositions, ToolExecutionDisposition{})
			}
			if err := validateWorkRetryTarget(item, item.ExecutionReceipt.ResultCollection); err == nil {
				t.Fatal("unsafe work retry accepted")
			}
		})
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	compiled := c.session.ResultContracts[original.ResultContract.ID]
	plans, err := c.fixedCollectionPlans(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"interrupted", "abstained", "error"} {
		t.Run(name, func(t *testing.T) {
			changed := append([]RunEvent(nil), events...)
			if name == "interrupted" {
				for _, event := range events {
					if event.Type != string(EventDecisionPrimitiveStarted) {
						continue
					}
					var payload decisionPrimitivePayload
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					binding := plan.WorkRetry.MissingDecisions[0]
					binding.Context = payload.Binding.Context
					payload.Binding = &binding
					event.Payload = mustProjectionJSON(t, payload)
					if err := validateDecisionPrimitiveEvent(event); err != nil {
						t.Fatal(err)
					}
					changed = append(changed, event)
					break
				}
			} else {
				for i, event := range changed {
					if event.Type != string(EventDecisionPrimitiveSettled) {
						continue
					}
					var payload decisionPrimitivePayload
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if name == "abstained" {
						payload.Result.Status = decisionrt.StatusAbstained
						payload.Receipt.Status = decisionrt.StatusAbstained
					} else {
						payload.ErrorCode = "decision_backend_failed"
						payload.Result, payload.Receipt = nil, nil
					}
					changed[i].Payload = mustProjectionJSON(t, payload)
					if err := validateDecisionPrimitiveEvent(changed[i]); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := workRetryMissingDecisions(changed, source.BranchID, original, compiled, plans, nil); err == nil {
				t.Fatal("unsettled decision retry accepted")
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatal("recovery validation repeated inference")
	}
}

func TestContractRecoveryReadOnlyWorkRetryInvalidatesOnlyConsumers(t *testing.T) {
	c, source, ctx, _ := readOnlyWorkRetryFixture(t, 1, 0)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	tasks := append([]*TodoItem(nil), source.Session.Tasks...)
	for i, id := range []string{"consumer", "transitive"} {
		item := cloneTodoItem(todoItemByID(tasks, "accepted-source"))
		item.ID = id
		if i == 0 {
			item.DependsOn = []string{plan.WorkRetry.TaskID}
		} else {
			item.EvidenceFrom = []string{"consumer"}
		}
		tasks = append(tasks, item)
		plan.Tasks = append(plan.Tasks, ContractRecoveryTask{ID: id, Action: "retry", ResultContract: item.ResultContract, ContractHash: item.ContractHash})
	}
	if err := validateWorkRetryScope(tasks, plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"retain_consumer", "retry_independent", "change_contract"} {
		t.Run(name, func(t *testing.T) {
			var changed ContractRecoveryPlan
			if err := json.Unmarshal(mustProjectionJSON(t, plan), &changed); err != nil {
				t.Fatal(err)
			}
			for i := range changed.Tasks {
				patch := &changed.Tasks[i]
				if name == "retain_consumer" && patch.ID == "transitive" {
					patch.Action = "retain"
				}
				if name == "retry_independent" && patch.ID == "accepted-source" {
					patch.Action = "retry"
				}
				if name == "change_contract" && patch.ID == plan.WorkRetry.TaskID {
					patch.ContractHash = strings.Repeat("a", 64)
				}
			}
			if err := validateWorkRetryScope(tasks, &changed); err == nil {
				t.Fatal("changed retry scope accepted")
			}
		})
	}
}

func TestContractRecoveryReadOnlyWorkRetry(t *testing.T) {
	c, source, ctx, calls := readOnlyWorkRetryFixture(t, 2, 1)
	before, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorkRetry == nil || len(plan.WorkRetry.MissingDecisions) != 1 || plan.Retained != 1 || plan.Retried != 1 || plan.SourcePolicy != plan.Policy.ConfigurationHash || calls.Load() != 1 {
		t.Fatalf("unexpected preview: %+v calls=%d", plan, calls.Load())
	}
	after, _ := c.EventJournal().ReadEvents(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("preview mutated source")
	}
	if _, err = c.ApplyContractRecovery(ctx, source, "stale", strings.Repeat("0", 64)); err == nil {
		t.Fatal("stale plan accepted")
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "retry-work", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ReadResumeSource(c.session.Workspace, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !sameCanonicalEvidence(parent.Session.Tasks, source.Session.Tasks) {
		t.Fatal("parent results or receipts changed")
	}
	old := todoItemByID(source.Session.Tasks, plan.WorkRetry.TaskID)
	fresh := todoItemByID(child.Session.Tasks, old.ID)
	retained := todoItemByID(child.Session.Tasks, "accepted-source")
	if fresh.Status != TaskPending || fresh.ExecutionReceipt != nil || fresh.TypedResult != nil || fresh.OccurrenceRevision <= old.OccurrenceRevision || len(fresh.ExecutionReceipts) == 0 || !sameCanonicalEvidence(fresh.ExecutionReceipts[len(fresh.ExecutionReceipts)-1], *old.ExecutionReceipt) {
		t.Fatal("retry did not retain original receipt or open new occurrence")
	}
	if !sameCanonicalEvidence(retained, todoItemByID(source.Session.Tasks, retained.ID)) {
		t.Fatal("unrelated accepted source changed")
	}
	if child.Session.WorkflowState != PhaseExecute {
		t.Fatal("workflow gates were not reopened")
	}
	// Replay checks independently reconstruct the missing slot from pinned schema.
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ReplayTodoList(events); err != nil {
		t.Fatal(err)
	}
	validEvent := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: branch, Payload: mustProjectionJSON(t, plan)}
	if _, err := validateContractRecoveryLineage(validEvent, before, source.Session.ExecutionPolicySnapshot); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing_proof", "slot", "schema", "checkpoint", "checks", "scope"} {
		t.Run(name, func(t *testing.T) {
			raw := mustProjectionJSON(t, plan)
			var tampered ContractRecoveryPlan
			if err := json.Unmarshal(raw, &tampered); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing_proof":
				tampered.WorkRetry = nil
			case "slot":
				tampered.WorkRetry.MissingDecisions[0].Slot = strings.Repeat("a", 64)
			case "schema":
				tampered.WorkRetry.Schema = []byte(`{"type":"object"}`)
			case "checkpoint":
				tampered.WorkRetry.CheckpointSHA256 = strings.Repeat("b", 64)
			case "checks":
				tampered.WorkRetry.Checks = nil
			case "scope":
				for i := range tampered.Tasks {
					if tampered.Tasks[i].ID == "accepted-source" {
						tampered.Tasks[i].Action = "retry"
					}
				}
				tampered.Retained--
				tampered.Retried++
			}
			tampered.SHA256, _ = recoveryPlanHash(tampered)
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: branch, Payload: mustProjectionJSON(t, tampered)}
			if _, err := validateContractRecoveryLineage(event, before, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("forged work retry proof accepted")
			}
		})
	}
}

func TestContractRecoveryReadOnlyWorkRetryResumesWithFreshDecisions(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed_%t", committed), func(t *testing.T) { testReadOnlyWorkRetryContinuation(t, committed) })
	}
}

func testReadOnlyWorkRetryContinuation(t *testing.T, committed bool) {
	t.Helper()
	var response string
	var collectorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
			return
		}
		collectorCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"recovered\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", response)
	}))
	t.Cleanup(server.Close)
	c, source, ctx, originalCalls := readOnlyWorkRetryOutcomeFixture(t, 2, 1, committed, server.URL+"/v1")
	if committed {
		assessment, err := c.AssessResultCollectionRecovery(t.Context(), "1")
		if err != nil || assessment.Disposition != RecoveryBlocked {
			t.Fatal("ordinary result repair gained authority over blocked work", err)
		}
	}
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "resume-work", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ReadResumeSource(c.session.Workspace, source.BranchID, "")
	if err != nil || !sameCanonicalEvidence(parent.Session.Tasks, source.Session.Tasks) {
		t.Fatal("work retry changed original results or outcome receipts", err)
	}
	c = reopenRecoveryCoordinator(t, c)
	c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
	c.SetSessionData(child.Session)
	c.executionRunID = "new-invocation-work-retry"
	if err = c.selectRecoveryRunInputSnapshot(&child.Session.RunInputSnapshots[0]); err != nil {
		t.Fatal(err)
	}
	if err = c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	if err = c.validateResumeSources(WithSessionResume(t.Context())); err != nil {
		t.Fatal(err)
	}
	var decisions atomic.Int32
	c.decisionPrimitives, err = catalog.New(c.session.Config.DecisionPrimitives, catalog.WithSidecarGenerator(func(string) (sidecarbackend.Generator, error) { return countingGenerator{calls: &decisions}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	c.decisionPrimitiveGate = make(chan struct{}, 1)
	id := plan.WorkRetry.TaskID
	plans, err := c.fixedCollectionPlans(id)
	if err != nil {
		t.Fatal(err)
	}
	response = `{"status":"success","summary":"New assessment retains the original source limitation","structured_payload":` + fixedCollectionSubmission(t, plans[0], func(rows map[string]any) {
		for _, row := range rows {
			delete(row.(map[string]any), "state")
		}
	}) + `}`
	artifact := todoItemByID(child.Session.Tasks, "accepted-source").TypedResult.Artifacts[0]
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
		raw, _ := json.Marshal(map[string]any{"artifact_ref": artifact.ID, "byte_limit": 1024})
		call := fantasy.ToolCall{ID: "fresh-read", Name: "view", Input: string(raw)}
		result, err := view.Run(ctx, call)
		if err != nil || result.IsError {
			t.Fatalf("fresh authorized read: %v %s", err, result.Content)
		}
		if err = c.captureDecisionArtifactRead(ctx, view, call, &result); err != nil {
			t.Fatal(err)
		}
		for pass := range 2 {
			for i, row := range plans[0].rows {
				raw, _ = json.Marshal(map[string]any{"collection": "/decisions", "row_key": row.Key, "group": "assessment"})
				result, err = (&decisionPrimitiveTool{coordinator: c}).Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("fresh-decision-%d-%d", pass, i), Name: decisionPrimitiveToolName, Input: string(raw)})
				if err != nil || result.IsError {
					t.Fatalf("fresh decision: %v %s", err, result.Content)
				}
			}
		}
		return textAgentResult("New assessment with all required decisions; original limitation retained."), nil
	}})
	c.workerAgentOverride = worker
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	if err = c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}, RequireNoUnresolvedTasks: true}); err != nil {
		t.Fatal(err)
	}
	if result, err := c.runAcceptance(t.Context()); err == nil || result.IsPassed() {
		t.Fatal("branch creation satisfied acceptance before work")
	}
	if _, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
		t.Fatal(err)
	}
	completed := c.todoItemByID(id)
	if completed.Status != TaskDone || completed.TypedResult == nil || worker.Calls() != 1 || decisions.Load() != 2 || originalCalls.Load() != 1 || collectorCalls.Load() != 1 {
		t.Fatalf("retry status=%s worker=%d decisions=%d collector=%d", completed.Status, worker.Calls(), decisions.Load(), collectorCalls.Load())
	}
	if completed.ExecutionReceipt.RunID != c.executionRunID || len(completed.TypedResult.StructuredPayload.CollectionBindings[0].Decisions) != 2 {
		t.Fatal("fresh receipt or decision provenance lost")
	}
	if result, err := c.runAcceptance(t.Context()); err != nil || !result.IsPassed() {
		t.Fatalf("recovered acceptance: %+v %v", result, err)
	}
	if err = c.SetAcceptanceSpec(AcceptanceSpec{RequireNoUnresolvedTasks: true, RequiredArtifacts: []string{"unproduced-deliverable.txt"}}); err != nil {
		t.Fatal(err)
	}
	if result, err := c.runAcceptance(t.Context()); err == nil || result.IsPassed() {
		t.Fatal("successful task bypassed acceptance evidence")
	}
	events, err := c.readActiveLineageEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCanonicalEvidence(todoItemByID(replayed, id).TypedResult, completed.TypedResult) {
		t.Fatal("replay changed new decision result")
	}
}
