package team

import (
	"cmp"
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

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

type contractCollectionFixtureOptions struct {
	Accepted         bool
	Verbatim         bool
	ValidAccepted    bool
	Dependents       bool
	StringEnvelope   bool
	PriorGrant       bool
	Endpoint         string
	ArtifactKeys     bool
	LongOutput       bool
	Exhausted        bool
	FrozenPaths      []string
	FrozenLineSpans  string
	PendingPeer      bool
	PreserveContract bool
}

func contractCollectionFixture(t *testing.T, response string, effect SideEffectClass, options ...contractCollectionFixtureOptions) (*Coordinator, *ResumeSource, string) {
	t.Helper()
	var fixture contractCollectionFixtureOptions
	if len(options) > 0 {
		fixture = options[0]
	}
	if fixture.LongOutput {
		var claims map[string]any
		if err := json.Unmarshal([]byte(response), &claims); err != nil {
			t.Fatal(err)
		}
		claims["summary"] = strings.Repeat("complete original findings; preserve all judgments. ", 120)
		raw, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		response = string(raw)
	}
	oldDocument := `{"type":"object","required":["findings"],"properties":{"findings":{"type":"array","maxItems":12,"items":{"type":"string"}}},"additionalProperties":false}`
	if fixture.ArtifactKeys {
		oldDocument = strings.TrimSuffix(oldDocument, "}") + `,"x-hufu-evidence-keys":{"pointers":["/findings/*"],"runtime_owned_files_read":true}}`
		if fixture.Accepted {
			oldDocument = strings.Replace(oldDocument, `"runtime_owned_files_read":true`, `"runtime_owned_files_read":false`, 1)
		}
	}
	if fixture.FrozenPaths != nil {
		var document map[string]any
		if err := json.Unmarshal([]byte(oldDocument), &document); err != nil {
			t.Fatal(err)
		}
		document["properties"].(map[string]any)["locations"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		document["required"] = append(document["required"].([]any), "locations")
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		oldDocument = string(raw)
		if fixture.FrozenLineSpans != "" {
			oldDocument = strings.TrimSuffix(oldDocument, "}") + `,"x-hufu-workset-paths":{"pointers":["/locations/*"],"format":"path_line"}}`
		}
	}
	old, err := CompileResultContractDocument("research.json", []byte(oldDocument))
	if err != nil {
		t.Fatal(err)
	}
	ref := old.ref(true)
	definition := TaskDef{ID: "research", Agent: "worker", Phase: PhasePrepare, SideEffect: effect, Recovery: RecoveryManual,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/structured_payload/value/findings", Op: "min_items", Value: 14}}}, ResultContract: &ref, ResultContractSpec: &resultContractSpec{Schema: ref.ID, RequireStructured: true}}
	if fixture.Accepted || fixture.PreserveContract {
		definition.VerifySpec.TaskResultAssertions[0].Value = 2
	}
	if fixture.Verbatim {
		definition.OutputMode = TaskOutputModeVerbatim
	}
	hash, err := effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ResolveRunInputSnapshot(nil, nil, "run-test", "inv-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	var observedSource ArtifactRef
	var frozenBinding *WorksetBinding
	c, item, ctx, receipt := resultCollectionIdentityFixture(t, cmp.Or(fixture.Endpoint, "http://127.0.0.1:1/v1"), fixture.Endpoint != "", "", func(spec *TodoSpec) {
		spec.ContractID, spec.ContractHash, spec.Phase = definition.ID, hash, definition.Phase
		spec.OutputMode = definition.OutputMode
		spec.VerifySpec = definition.VerifySpec
		spec.SideEffect, spec.Recovery, spec.ResultContract = effect, RecoveryManual, &ref
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
		if fixture.ArtifactKeys {
			spec.WorksetBinding = &WorksetBinding{WorksetID: "evidence", ItemKey: "sample", Inputs: []ArtifactRef{observedSource}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: observedSource.ID, SHA256: observedSource.SHA256, ByteSize: observedSource.ByteSize, Ranges: []tools.ArtifactByteRange{{Start: 0, End: observedSource.ByteSize}}}}}
		}
		if frozenBinding != nil {
			spec.WorksetBinding = frozenBinding
		}
	}, func(c *Coordinator, spec *TodoSpec, ids []string) {
		if frozenBinding == nil {
			return
		}
		receipts, err := buildWorksetReceipts([]TaskDef{{WorksetBinding: frozenBinding}}, ids, c.executionRunID)
		if err != nil {
			t.Fatal(err)
		}
		spec.WorksetReceipt = receipts[frozenBinding.WorksetID]
	}, func(c *Coordinator) {
		if fixture.ArtifactKeys {
			c.session.Agents["worker"].Tools = "view"
			observedSource = contractCollectionArtifact(t, c)
		}
		c.session.Config.Delegation.BindTaskGoalContracts = true
		c.session.ContractTasks = []TaskDef{definition}
		c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: old}
		c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
		c.session.WorkspaceVersion = WorkspaceVersionContext{Mode: versionstore.ModeOff, WorkspaceID: "metadata", ProjectID: "project", TeamName: "test", ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir, StateDir: t.TempDir()}
		if fixture.FrozenPaths != nil {
			frozenBinding = collectionFrozenPathProducer(t, c, snapshot, &observedSource, fixture.FrozenPaths, fixture.FrozenLineSpans)
		}
	})
	if fixture.FrozenPaths == nil {
		appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventRunInputsResolved), RunID: "run-test", Payload: mustProjectionJSON(t, snapshot)})
	}
	receipt.RunInputSnapshotID, receipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	if fixture.ArtifactKeys {
		ctx, response = observeContractCollectionKeys(t, c, item, ctx, &receipt, response, observedSource)
		if fixture.Accepted {
			var body map[string]any
			if err := json.Unmarshal([]byte(response), &body); err != nil {
				t.Fatal(err)
			}
			if !fixture.ValidAccepted {
				body["structured_payload"].(map[string]any)["findings"].([]any)[0] = "sha256-" + strings.Repeat("0", 64)
			}
			body["files_read"] = []FileRef{{Path: observedSource.ID}}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			response = string(raw)
		}
	}
	if fixture.StringEnvelope {
		response = fmt.Sprintf(`{"result":%q}`, response)
	}
	acceptedResponse := response
	if fixture.Accepted && (fixture.PriorGrant || fixture.Exhausted) {
		response = strings.Replace(response, `"status":"success"`, `"status":42`, 1)
	}
	workOutput, completedOutput := "complete original research with every finding", "complete original research"
	if fixture.LongOutput {
		workOutput = strings.Repeat(workOutput+"\n", 200)
		completedOutput = workOutput
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, workOutput)
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.recordCollectionResponse(ctx, cp, []byte(response))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.PriorGrant {
		cp, err = c.recordCollectionFormatFailure(ctx, cp, rejectLocalResult(RepairFailureInvalidSchema, errors.New("original canonical schema rejects findings count")))
		if err != nil {
			t.Fatal(err)
		}
		cp, err = c.reserveResultCollection(ctx, cp, "second-original-collector", []byte(`{"tools":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(response))
		if err != nil {
			t.Fatal(err)
		}
	}
	if fixture.Accepted {
		if fixture.PriorGrant {
			cp, err = c.blockResultCollection(ctx, cp, RepairFailureInvalidSchema)
			if err != nil {
				t.Fatal(err)
			}
			c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New("original format rejection")})
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "original format rejection", NeedsHuman, FailureProtocol, TaskBlocked, "complete original research")
			c.executionRunID = "earlier-result-repair"
			if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
				t.Fatal(err)
			}
			cp = currentResultCollection(c.todoItemByID(item.ID))
			ctx, err = c.restoreCollectionContext(WithSessionResume(t.Context()), cp)
			if err == nil {
				cp, err = c.reserveResultCollection(ctx, cp, "earlier-granted-collector", []byte(`{"tools":[]}`))
			}
			if err == nil {
				cp, err = c.recordCollectionResponse(ctx, cp, []byte(acceptedResponse))
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if fixture.Exhausted {
			for cp.TurnsUsed < cp.TurnLimit {
				raw, readErr := c.readCollectionBlob(ctx, cp, "response", cp.Response)
				if readErr != nil {
					t.Fatal(readErr)
				}
				_, rejection := c.decodeCollectionClaims(item.ID, raw)
				if rejection == nil {
					t.Fatal("exhausted fixture requires an actual rejected response")
				}
				cp, err = c.recordCollectionFormatFailure(ctx, cp, rejection)
				if err != nil {
					t.Fatal(err)
				}
				cp, err = c.reserveResultCollection(ctx, cp, "spent-"+newTaskDispatchID(), []byte(`{"tools":[]}`))
				if err == nil {
					cp, err = c.recordCollectionResponse(ctx, cp, []byte(acceptedResponse))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		cp, err = c.prepareCollectionClaims(ctx, cp)
		if err == nil {
			err = c.commitPreparedCollection(ctx, cp)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.finishProtocolRepair(ctx, c.todoItemByID(item.ID), definition, item.Agent, "ollama/test", c.GetTaskResult(item.ID), completedOutput); err != nil {
			t.Fatal(err)
		}
	} else {
		cp, err = c.blockResultCollection(ctx, cp, RepairFailureInvalidSchema)
		if err != nil {
			t.Fatal(err)
		}
		c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New("original schema rejected claims")})
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "original schema rejected claims", NeedsHuman, FailureProtocol, TaskBlocked, "complete original research")
	}
	if fixture.PriorGrant && !fixture.Accepted {
		c.executionRunID = "earlier-result-repair"
		if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
			t.Fatal(err)
		}
		cp = currentResultCollection(c.todoItemByID(item.ID))
		repairCtx, err := c.restoreCollectionContext(WithSessionResume(t.Context()), cp)
		if err != nil {
			t.Fatal(err)
		}
		cp, err = c.blockResultCollection(repairCtx, cp, RepairFailureInvalidSchema)
		if err != nil {
			t.Fatal(err)
		}
		c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New("same rejected claims")})
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "same rejected claims", NeedsHuman, FailureProtocol, TaskBlocked, "complete original research")
	}
	if fixture.Dependents {
		acceptedRepairDependents(t, c, item.ID, definition, snapshot, observedSource)
	}
	if fixture.PendingPeer {
		pending := TodoSpec{Agent: item.Agent, Model: item.Model, ExecutionTarget: item.ExecutionTarget,
			ContractID: definition.ID, ContractHash: item.ContractHash, Phase: item.Phase, SideEffect: SideEffectNone,
			Recovery: RecoveryManual, Execution: definition.Execution, ResultContract: definition.ResultContract,
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
		if frozenBinding != nil {
			producer := c.todoItemByID(frozenBinding.SourceArtifact.TaskID)
			pending.ContractID, pending.ContractHash = producer.ContractID, producer.ContractHash
			pending.Execution, pending.ResultContract = producer.Execution, producer.ResultContract
		}
		ids := c.taskTracker.TodoList().ReserveIDs(1)
		projection, err := taskOccurrenceProjectionFromSpec(pending, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{pending}, ids); err != nil {
			t.Fatal(err)
		}
	}
	appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "run-test", Payload: []byte(`{"state":"PREPARE"}`)})
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	nextDocument := strings.Replace(oldDocument, `"maxItems":12,`, "", 1)
	if fixture.Accepted {
		nextDocument = strings.Replace(oldDocument, `"runtime_owned_files_read":false`, `"runtime_owned_files_read":true`, 1)
	}
	if fixture.FrozenLineSpans != "" {
		nextDocument = withWorksetLineSpans(nextDocument)
	} else if fixture.FrozenPaths != nil {
		nextDocument = strings.TrimSuffix(nextDocument, "}") + `,"x-hufu-workset-paths":{"pointers":["/locations/*"],"format":"path_line"}}`
	}
	next, err := CompileResultContractDocument("research.json", []byte(nextDocument))
	if err != nil {
		t.Fatal(err)
	}
	if !fixture.PreserveContract {
		c.session.ResultContracts[ref.ID] = next
		nextRef := next.ref(true)
		c.session.ContractTasks[0].ResultContract = &nextRef
	}
	source, err := ReadResumeSource(c.session.Workspace, "main", "run-test")
	if err != nil {
		t.Fatal(err)
	}
	return c, source, item.ID
}

func researchCollectionResponse(t *testing.T, count int) string {
	t.Helper()
	findings := make([]string, count)
	for i := range count {
		findings[i] = strings.Repeat("finding ", i+1)
	}
	raw, err := json.Marshal(map[string]any{"status": "success", "summary": "complete original findings", "structured_payload": map[string]any{"findings": findings}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func contractCollectionContext(t *testing.T, id string) context.Context {
	t.Helper()
	return WithContractResultRepair(WithMetadataOnlyContractRecovery(t.Context()), id)
}

func TestContractRecoveryRepairsCompletedCollectionWithoutWorkerReplay(t *testing.T) {
	for _, scenario := range []struct {
		effect     SideEffectClass
		priorGrant bool
	}{{SideEffectNone, false}, {SideEffectBrowserInteraction, false}, {SideEffectBrowserInteraction, true}} {
		t.Run(fmt.Sprintf("%s/prior_grant=%t", scenario.effect, scenario.priorGrant), func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), scenario.effect, contractCollectionFixtureOptions{PriorGrant: scenario.priorGrant})
			original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Repaired != 1 || plan.Retried != 0 || plan.Tasks[0].Action != "repair_result" || !reflect.DeepEqual(plan.ReopenedVerification, []string{id}) {
				t.Fatalf("plan=%+v", plan)
			}
			before := mustReadEvents(t, c.eventStore)
			branch, err := c.ApplyContractRecovery(ctx, source, "repaired-contract", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := ReadResumeSource(c.session.Workspace, "main", "run-test")
			if err != nil {
				t.Fatal(err)
			}
			if err := compareSingleTaskProjection(parent.Session.Tasks[0], original); err != nil {
				t.Fatalf("parent changed: %v", err)
			}
			events := mustReadEvents(t, c.eventStore)
			if !reflect.DeepEqual(before, events[:len(before)]) {
				t.Fatal("historical events changed")
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			migrated := child.Session.Tasks[0]
			cp := currentResultCollection(migrated)
			if cp.Work != original.ExecutionReceipt.ResultCollection.Work || cp.TurnsUsed != original.ExecutionReceipt.ResultCollection.TurnsUsed || cp.TurnLimit != original.ExecutionReceipt.ResultCollection.TurnLimit {
				t.Fatal("publication changed work or collector budget")
			}
			restarted := reopenContractCollection(t, c, child)
			if err := restarted.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			a, err := restarted.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != RecoveryCommitLocal {
				t.Fatalf("assessment=%+v err=%v", a, err)
			}
			if err := restarted.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
				t.Fatal(err)
			}
			worker := scriptWorker(textTurn("must not repeat original work"))
			restarted.workerAgentOverride = worker
			if _, err := restarted.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restarted.todoItemByID(id)), id); err != nil {
				t.Fatal(err)
			}
			item := restarted.todoItemByID(id)
			if worker.Calls() != 0 || item.Status != TaskDone || currentResultCollection(item).TurnsUsed != cp.TurnsUsed {
				t.Fatalf("worker calls=%d status=%s", worker.Calls(), item.Status)
			}
			var payload struct {
				Findings []string `json:"findings"`
			}
			if err := json.Unmarshal(item.TypedResult.StructuredPayload.Value, &payload); err != nil || len(payload.Findings) != 14 {
				t.Fatal("findings were lost")
			}
			if item.VerifyResult == nil || item.VerifyResult.ExitCode != 0 {
				t.Fatal("objective verification was not rerun")
			}
			if err := restarted.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}, RequireNoUnresolvedTasks: true}); err != nil {
				t.Fatal(err)
			}
			if accepted, err := restarted.runAcceptance(t.Context()); err != nil || !accepted.IsPassed() {
				t.Fatalf("child acceptance failed: %+v %v", accepted, err)
			}
			if err := restarted.validateCompletedResumeEvidence(t.Context(), item, &child.Session.RunInputSnapshots[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadResumeSource(restarted.session.Workspace, branch, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContractRecoveryRejectsUnsafeCollectionUpgrade(t *testing.T) {
	for _, scenario := range []string{"missing_selection", "wrong_task", "partial_effects", "unknown_effects", "unrelated_external_effect", "model_drift", "changed_source", "stale_plan"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), SideEffectBrowserInteraction)
			ctx := contractCollectionContext(t, id)
			switch scenario {
			case "missing_selection":
				ctx = WithMetadataOnlyContractRecovery(t.Context())
			case "wrong_task":
				ctx = contractCollectionContext(t, "missing")
			case "partial_effects", "unknown_effects":
				source.Session.Tasks[0].RecoveryState = RecoveryStatePartial
				if scenario == "unknown_effects" {
					source.Session.Tasks[0].RecoveryState = RecoveryStateUnknown
				}
				source.sessionSHA256, _ = collectionDigest(source.Session)
			case "unrelated_external_effect":
				c.session.ContractTasks[0].SideEffect = SideEffectExternalWrite
			case "model_drift":
				c.session.Agents["worker"].Generation.Model = "ollama/different"
			case "changed_source":
				source.Session.Tasks[0].RunInputSnapshotHash = strings.Repeat("b", 64)
			case "stale_plan":
				plan, err := c.AssessContractRecovery(ctx, source)
				if err != nil {
					t.Fatal(err)
				}
				appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventWorkflowStateChanged), Payload: []byte(`{"state":"PREPARE"}`)})
				if _, err := c.ApplyContractRecovery(ctx, source, "stale", plan.SHA256); err == nil {
					t.Fatal("stale plan applied")
				}
				return
			}
			if _, err := c.AssessContractRecovery(ctx, source); err == nil {
				t.Fatal("unsafe result-only upgrade accepted")
			}
		})
	}
}

func reopenContractCollection(t *testing.T, c *Coordinator, child *ResumeSource) *Coordinator {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newTestCoordinator(t)
	restarted.session, restarted.projectDir, restarted.providerManager = c.session, c.projectDir, c.providerManager
	restarted.maxConcurrent = c.maxConcurrent
	restarted.providerBoundaryStarted = c.providerBoundaryStarted
	if c.modelProfileRuntime != nil {
		restarted.modelProfileRuntime = NewModelProfileRuntime(c.providerManager, false)
	}
	restarted.recoveryBranch = child.BranchID
	if err := restarted.prepareRecoveryValidation(); err != nil {
		t.Fatal(err)
	}
	restarted.initEventStore()
	restarted.taskTracker.TodoList().Restore(child.Session.Tasks)
	restarted.SetSessionData(child.Session)
	restarted.executionRunID = "child-result-repair"
	restarted.runInputSelectionRunID = restarted.executionRunID
	return restarted
}

func TestContractRecoveryBoundsCollectorRepairAfterUpgrade(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			var calls atomic.Int32
			invalid := `{"status":"success","summary":"complete research","structured_payload":{"findings":"wrong type"}}`
			accepted := researchCollectionResponse(t, 14)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				if calls.Add(1) > 2 {
					t.Error("collector exceeded bounded repair grant")
					http.Error(w, "extra turn", 400)
					return
				}
				claims := invalid
				if valid {
					claims = accepted
				}
				envelope := fmt.Sprintf(`{"result":%s}`, claims)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", envelope)
			}))
			t.Cleanup(server.Close)
			c, source, id := contractCollectionFixture(t, invalid, SideEffectBrowserInteraction, contractCollectionFixtureOptions{Endpoint: server.URL + "/v1"})
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "bounded", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != RecoveryRepairClaims || a.AdditionalTurns != 2 {
				t.Fatalf("assessment=%+v err=%v", a, err)
			}
			worker := scriptWorker(textTurn("never replay"))
			c.workerAgentOverride = worker
			if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
				t.Fatal(err)
			}
			_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id)
			item := c.todoItemByID(id)
			if worker.Calls() != 0 || valid && (err != nil || item.Status != TaskDone || calls.Load() != 1) || !valid && (err == nil || item.Status != TaskBlocked || calls.Load() != 2) {
				t.Fatalf("valid=%t calls=%d worker=%d status=%s err=%v", valid, calls.Load(), worker.Calls(), item.Status, err)
			}
			if currentResultCollection(item).TurnsUsed != a.TurnsUsed+int(calls.Load()) {
				t.Fatal("consumed collection turns were refunded")
			}
			if _, err := ReadResumeSource(c.session.Workspace, branch, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContractRecoveryRejectsForgedCollectionProof(t *testing.T) {
	for _, field := range []string{"work", "inputs", "policy", "consumed_turns", "source_digest"} {
		t.Run(field, func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), SideEffectBrowserInteraction)
			plan, err := c.AssessContractRecovery(contractCollectionContext(t, id), source)
			if err != nil {
				t.Fatal(err)
			}
			proof := plan.Tasks[0].Collection
			switch field {
			case "work":
				proof.Original.Work.SHA256 = strings.Repeat("a", 64)
			case "inputs":
				proof.Original.InputsSHA256 = strings.Repeat("a", 64)
			case "policy":
				proof.AcceptedPolicy = strings.Repeat("a", 64)
			case "consumed_turns":
				proof.Original.TurnsUsed++
			case "source_digest":
				proof.SourceTaskSHA256 = strings.Repeat("a", 64)
			}
			plan.SHA256, _ = recoveryPlanHash(*plan)
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: "forged-child", Payload: mustProjectionJSON(t, plan)}
			if _, err := validateContractRecoveryLineage(event, mustReadEvents(t, c.eventStore), source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("forged collection proof accepted")
			}
		})
	}
}

func TestContractRecoveryRevalidatesOriginalToolObservations(t *testing.T) {
	for _, changedRule := range []bool{false, true} {
		t.Run(fmt.Sprint(changedRule), func(t *testing.T) {
			c := newTestCoordinator(t)
			old := evidenceTestContract(t)
			original := old.ref(true)
			schema := strings.Replace(evidenceTestSchema, `"type":"object","required":["findings"]`, `"type":"object","description":"updated representation","required":["findings"]`, 1)
			if changedRule {
				schema = strings.Replace(schema, `"tool":"read_document"`, `"tool":"different_document"`, 1)
			}
			next, err := CompileResultContractDocument(old.ID, []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			accepted := next.ref(true)
			definition := TaskDef{ID: "source", Agent: "worker", SideEffect: SideEffectBrowserInteraction, Recovery: RecoveryManual, ResultContract: &original, ResultContractSpec: &resultContractSpec{Schema: original.ID, RequireStructured: true}}
			hash, err := effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := ResolveRunInputSnapshot(nil, nil, "original-run", "original-invocation", "test")
			if err != nil {
				t.Fatal(err)
			}
			transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, "1", "original-run", 1, "worker")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			if err := transcript.RecordToolCall("observed", "read_document", `{"key":"document-a"}`); err != nil {
				t.Fatal(err)
			}
			if err := transcript.RecordToolResult("observed", "read_document", `{"text":"the exact words in a document"}`, false); err != nil {
				t.Fatal(err)
			}
			ref, err := sealVerbatimTaskTranscript(transcript)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := validateStructuredResultPayload(old, original, []byte(evidenceTestPayload), transcript.evidenceRecords())
			if err != nil {
				t.Fatal(err)
			}
			receipt := &ExecutionReceipt{RunID: "original-run", TaskID: "1", Attempt: 1, ProducerID: "worker", ModelExecutionID: "original-model", TranscriptRef: ref.ID, ExitCode: new(0), RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ResultContractID: original.ID, ResultContractSHA256: original.SchemaSHA256, ResultPayloadSHA256: payload.SHA256, ResultValidation: ResultValidationValid}
			item := &TodoItem{ID: "1", Agent: "worker", Status: TaskDone, ContractID: definition.ID, ContractHash: hash, SideEffect: definition.SideEffect, Recovery: definition.Recovery, ResultContract: &original, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipt: receipt, ExecutionReceipts: []ExecutionReceipt{*receipt}, TypedResult: &TaskResult{TaskID: "1", Agent: "worker", Status: TaskResultStatusSuccess, Source: "submitted", Summary: "accepted observation", StructuredPayload: payload}}
			session := &SessionData{Tasks: []*TodoItem{item}, RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID}
			definition.ResultContract = &accepted
			c.session.ResultContracts = map[string]*CompiledResultContract{accepted.ID: next}
			patch, retry, err := c.assessContractRecoveryTask(t.Context(), item, session, map[string]TaskDef{definition.ID: definition}, map[string]ExecutionResultContractPolicySnapshot{"contract:source": {Owner: "contract:source", ID: original.ID, SchemaSHA256: original.SchemaSHA256, RequireStructured: true}})
			if err != nil || retry != changedRule {
				t.Fatalf("retry=%t changed_rule=%t err=%v", retry, changedRule, err)
			}
			if !changedRule && (patch.Revalidation == nil || patch.Revalidation.PayloadSHA256 != payload.SHA256) {
				t.Fatal("verified observation changed during revalidation")
			}
		})
	}
}
