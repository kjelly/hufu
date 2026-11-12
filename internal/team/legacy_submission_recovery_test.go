package team

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

const legacyRejectedClaims = `{"status":"completed_with_gaps","summary":"original answer","structured_payload":{"answer":[{"text":"Evidence remains inconclusive.","extra":""}]}}`
const legacyRepairedClaims = `{"status":"completed_with_gaps","summary":"original answer","structured_payload":{"answer":[{"text":"Evidence remains inconclusive."}]}}`

type legacySubmissionFixtureConfig struct {
	Source  bool
	Receipt func(*ExecutionReceipt)
}

func legacySubmissionFixture(t *testing.T, endpoint string, options ...legacySubmissionFixtureConfig) (*Coordinator, *ResumeSource, string) {
	t.Helper()
	var config legacySubmissionFixtureConfig
	if len(options) > 0 {
		config = options[0]
	}
	compiled, err := CompileResultContractDocument("answer.json", []byte(`{"type":"object","additionalProperties":false,"required":["answer"],"properties":{"answer":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"required":["text"],"properties":{"text":{"type":"string"}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	definition := TaskDef{ID: "answer", Agent: "worker", Phase: PhaseVerify, SideEffect: SideEffectNone, Recovery: RecoveryRetry,
		Execution: ExecutionContract{RequiresResult: true, ToolSequence: []string{submitResultToolName}}, ResultContract: &ref, ResultContractSpec: &resultContractSpec{Schema: ref.ID, RequireStructured: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/structured_payload/value/answer", Op: "min_items", Value: 1}}}}
	hash, err := effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ResolveRunInputSnapshot(nil, nil, "run-test", "inv-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, false, func(spec *TodoSpec) {
		spec.ContractID, spec.ContractHash, spec.Phase = definition.ID, hash, definition.Phase
		spec.SideEffect = SideEffectNone
		spec.Recovery = RecoveryRetry
		spec.Execution = cloneExecutionContract(definition.Execution)
		spec.VerifySpec = definition.VerifySpec
		spec.ResultContract = &ref
		if config.Source {
			spec.EvidenceFrom = []string{"2"}
		}
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}, func(c *Coordinator) {
		c.session.Config.Delegation.BindTaskGoalContracts = true
		c.session.Config.CollectorResponseChannel = agent.CollectorResponseTextJSON
		c.session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		c.session.Agents["worker"].Tools = submitResultToolName
		c.session.ContractTasks = []TaskDef{definition}
		if config.Source {
			d := definition
			d.ID = "source"
			d.Phase = PhasePrepare
			d.SideEffect = SideEffectBrowserInteraction
			d.Execution = ExecutionContract{RequiresResult: true}
			c.session.ContractTasks = append(c.session.ContractTasks, d)
		}
		c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: compiled}
		c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
		c.session.WorkspaceVersion = WorkspaceVersionContext{Mode: versionstore.ModeOff, WorkspaceID: "metadata", ProjectID: "project", TeamName: "test", ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir, StateDir: t.TempDir()}
	})
	appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventRunInputsResolved), RunID: "run-test", Payload: mustProjectionJSON(t, snapshot)})
	if config.Source {
		d := c.session.ContractTasks[1]
		sourceHash, err := contractRecoveryTaskHash(d, &TodoItem{}, &SessionData{})
		if err != nil {
			t.Fatal(err)
		}
		original := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "accepted source", Goal: "accepted source", ContractID: d.ID, ContractHash: sourceHash, Phase: d.Phase, SideEffect: SideEffectBrowserInteraction, Execution: d.Execution, ResultContract: &ref, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}})[0]
		sourceTranscript, err := newTaskTranscriptForAttempt(c.session.Workspace, original.ID, "run-test", 1, "worker")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sourceTranscript.Close() })
		if err := sourceTranscript.RecordAssistantOutput("Accepted source with an explicit evidence gap."); err != nil {
			t.Fatal(err)
		}
		sealed, err := sealVerbatimTaskTranscript(sourceTranscript)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := validateStructuredResultPayload(compiled, ref, []byte(`{"answer":[{"text":"Accepted source with an explicit evidence gap."}]}`))
		if err != nil {
			t.Fatal(err)
		}
		sourceReceipt := cloneExecutionReceipt(&receipt)
		sourceReceipt.TaskID = original.ID
		sourceReceipt.ModelExecutionID = "source-work"
		sourceReceipt.TranscriptRef = sealed.ID
		sourceReceipt.RunInputSnapshotID, sourceReceipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
		sourceReceipt.ResultContractID, sourceReceipt.ResultContractSHA256, sourceReceipt.ResultPayloadSHA256, sourceReceipt.ResultValidation = ref.ID, ref.SchemaSHA256, payload.SHA256, ResultValidationValid
		originals := c.taskTracker.TodoList().Items()
		originals[1].Status = TaskDone
		// Timing exists only in the live projection, never canonical replay.
		originals[1].ModelTime = time.Second
		originals[1].ExecutionReceipt = &sourceReceipt
		originals[1].ExecutionReceipts = []ExecutionReceipt{sourceReceipt}
		originals[1].TypedResult = &TaskResult{TaskID: original.ID, Agent: "worker", Attempt: 1, Source: "submitted", Status: TaskResultStatusCompletedWithGaps, Summary: "Accepted source", StructuredPayload: payload}
		c.taskTracker.TodoList().Restore(originals)
		if err := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); err != nil {
			t.Fatal(err)
		}
	}
	if config.Source {
		task := taskDefFromTodoItem(c.todoItemByID(item.ID))
		request := c.newTaskContextRequest(task, item.ID, receipt.Attempt, ContextTriggerTaskDispatch, item.Agent, "worker", nil)
		request.ModelExecutionID = receipt.ModelExecutionID
		request.AssignRequestID()
		content := FormatDependencyResults(c.dependencyResultsForTask(item.ID))
		compiled := CompiledContext{IncludedItems: []ContextItem{{ID: "dependency_results", Kind: "dependency_result", Content: content, Required: true}}}
		manifest, err := BuildContextInjectionManifest(request, compiled, nil, item.Agent, time.Now(), c.session.Config.MemoryLearning)
		if err != nil {
			t.Fatal(err)
		}
		// Reproduce a legacy schema-2 native dispatch, which predates the
		// selected-view call marker used by newer manifest versions.
		manifest.ModelCalled, manifest.Outcome = true, "model_call"
		manifest.Fingerprint = contextManifestFingerprint(manifest)
		appendBranchEvent(t, c.eventStore, RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: "task_context_manifest", RunID: receipt.RunID, TaskID: item.ID, Payload: mustProjectionJSON(t, map[string]any{"id": item.ID, "context_manifests": []ContextInjectionManifest{manifest}})})
		receipt.ContextManifest = cloneContextInjectionManifest(&manifest)
	}
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{ID: "rejected", Name: submitResultToolName, Input: legacyRejectedClaims})
	if err != nil || !response.IsError || c.todoItemByID(item.ID).ResultCandidate == nil {
		t.Fatalf("candidate: %+v %v", response, err)
	}
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, "run-test", 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	for _, record := range []struct {
		id, input, output string
		failed            bool
	}{
		{"rejected", legacyRejectedClaims, response.Content, true},
		{"correction", legacyRepairedClaims, "closed sequence", true},
		{"blocked", `{"status":"blocked","summary":"submission unavailable"}`, "submitted", false},
	} {
		if err := transcript.RecordToolCall(record.id, submitResultToolName, record.input); err != nil {
			t.Fatal(err)
		}
		if err := transcript.RecordToolResult(record.id, submitResultToolName, record.output, record.failed); err != nil {
			t.Fatal(err)
		}
	}
	transcriptRef, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	receipt.TranscriptRef = transcriptRef.ID
	receipt.RunInputSnapshotID, receipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	receipt.ExitCode = new(1)
	receipt.StepBudget = &StepBudgetUsage{Used: 3, Limit: 30}
	receipt.SubmittedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusBlocked, Source: "submitted", Summary: "submission unavailable"}
	if config.Receipt != nil {
		config.Receipt(&receipt)
	}
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		t.Fatal(err)
	}
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "schema rejection closed the sequence", RetryNone, FailureExecution, TaskBlocked, "original work returned")
	items := c.taskTracker.TodoList().Items()
	items[0].FailureEvent.FailedStepID = "rejected"
	c.taskTracker.TodoList().Restore(items)
	if err := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "run-test", Payload: []byte(`{"state":"VERIFY"}`)})
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(c.session.Workspace, "main", "run-test")
	if err != nil {
		t.Fatal(err)
	}
	c.session.Agents["worker"].ResultTransport = agent.ResultTransportRuntime
	c.session.ContractTasks[0].Execution.ToolSequence = nil
	c.providerBoundaryStarted = true
	c.modelProfileRuntime = NewModelProfileRuntime(c.providerManager, false)
	return c, source, item.ID
}

func TestContractRecoveryRepairsLegacySubmissionWithoutWorkerReplay(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		valid, source bool
	}{{"empty_sources", true, false}, {"retained_browser_source", true, true}, {"bounded_failure", false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			valid := scenario.valid
			var calls atomic.Int32
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
				if exposed, _ := request["tools"].([]any); len(exposed) != 0 {
					t.Error("legacy collection exposed execution tools")
				}
				n := calls.Add(1)
				if n > 2 {
					t.Error("unbounded legacy repair")
					http.Error(w, "extra call", 400)
					return
				}
				if scenario.source && !strings.Contains(fmt.Sprint(request["messages"]), "Accepted source with an explicit evidence gap.") {
					t.Error("full accepted source lost from conversion capsule")
				}
				if !strings.Contains(fmt.Sprint(request["messages"]), "Evidence remains inconclusive.") {
					t.Error("original candidate lost")
				}
				body := legacyRejectedClaims
				if valid && n == 2 {
					body = legacyRepairedClaims
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
			}))
			t.Cleanup(server.Close)
			c, source, id := legacySubmissionFixture(t, server.URL+"/v1", legacySubmissionFixtureConfig{Source: scenario.source})
			before := mustReadEvents(t, c.eventStore)
			original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Repaired != 1 || plan.Retried != 0 || plan.Retained != len(source.Session.Tasks)-1 || plan.Tasks[0].LegacySubmission == nil || calls.Load() != 0 {
				t.Fatalf("invalid plan: %+v", plan)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "legacy-repair", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			events := mustReadEvents(t, c.eventStore)
			if !reflect.DeepEqual(before, events[:len(before)]) {
				t.Fatal("historical events changed")
			}
			parent, err := ReadResumeSource(c.session.Workspace, "main", "")
			if err != nil || !sameCanonicalEvidence(parent.Session.Tasks[0], original) {
				t.Fatalf("parent changed: %v", err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.source && !sameCanonicalEvidence(child.Session.Tasks[1], source.Session.Tasks[1]) {
				t.Fatal("accepted source or original receipt changed")
			}
			cp := currentResultCollection(child.Session.Tasks[0])
			if cp == nil || cp.TurnsUsed != 3 || cp.TurnLimit != 3 || calls.Load() != 0 {
				t.Fatal("publication granted turns or erased legacy budget")
			}
			restarted := reopenContractCollection(t, c, child)
			if err := restarted.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			a, err := restarted.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition == RecoveryBlocked {
				t.Fatalf("assessment %+v: %v", a, err)
			}
			if err := restarted.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
				t.Fatal(err)
			}
			worker := scriptWorker(textTurn("must not execute original work"))
			restarted.workerAgentOverride = worker
			_, err = restarted.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restarted.todoItemByID(id)), id)
			if (err == nil) != valid || worker.Calls() != 0 || calls.Load() != 2 {
				t.Fatalf("valid=%t worker=%d calls=%d err=%v", valid, worker.Calls(), calls.Load(), err)
			}
			item := restarted.todoItemByID(id)
			if currentResultCollection(item).TurnsUsed != 5 {
				t.Fatal("repair lost historical calls")
			}
			if valid {
				if err := restarted.validateCompletedResumeEvidence(t.Context(), item, contractRecoveryInputSnapshot(item, child.Session)); err != nil {
					t.Fatal(err)
				}
				work, err := restarted.loadCollectionWork(t.Context(), item, currentResultCollection(item))
				if err != nil || work.Receipt.ExitCode == nil || *work.Receipt.ExitCode != 1 || !sameCanonicalEvidence(work.Receipt, original.ExecutionReceipt) {
					t.Fatalf("original failed receipt changed: %v", err)
				}
				if item.Status != TaskDone || item.VerifyResult == nil || item.VerifyResult.ExitCode != 0 || item.TypedResult.Status != TaskResultStatusCompletedWithGaps {
					t.Fatalf("incomplete repaired task: %+v", item)
				}
				if err := restarted.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}, RequiredWorkersAcceptGaps: true, RequireNoUnresolvedTasks: true}); err != nil {
					t.Fatal(err)
				}
				if result, err := restarted.runAcceptance(t.Context()); err != nil || !result.IsPassed() {
					t.Fatalf("acceptance: %+v %v", result, err)
				}
			} else if item.Status != TaskBlocked || item.TypedResult != nil {
				t.Fatal("invalid answer was accepted")
			}
			replayed, err := ReplayTodoList(mustReadEvents(t, restarted.eventStore))
			if err != nil || !sameCanonicalEvidence(replayed[0].ExecutionReceipt, item.ExecutionReceipt) {
				t.Fatalf("replay changed recovery receipt: %v", err)
			}
		})
	}
}

func TestContractRecoveryRejectsUnsafeLegacySubmissions(t *testing.T) {
	for _, scenario := range []string{"successful_work_receipt", "unrelated_policy_denial", "candidate_missing", "generation_drift", "scope_drift", "schema_drift", "response_channel_drift", "missing_dependency_dispatch", "changed_dependency_dispatch", "changed_source_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			mutate := func(r *ExecutionReceipt) {}
			if scenario == "missing_dependency_dispatch" {
				mutate = func(r *ExecutionReceipt) { r.ContextManifest = nil }
			}
			if scenario == "changed_dependency_dispatch" {
				mutate = func(r *ExecutionReceipt) { r.ContextManifest.Items[0].ContentHash = strings.Repeat("a", 32) }
			}
			if scenario == "successful_work_receipt" {
				mutate = func(r *ExecutionReceipt) { r.ExitCode = new(0) }
			}
			if scenario == "unrelated_policy_denial" {
				mutate = func(r *ExecutionReceipt) {
					r.ToolDispositions = []ToolExecutionDisposition{{ToolName: "bash", Kind: ToolExecutionPolicyDenied, ReasonCode: "guard_denied"}}
				}
			}
			c, source, id := legacySubmissionFixture(t, "http://127.0.0.1:1/v1", legacySubmissionFixtureConfig{Receipt: mutate, Source: strings.Contains(scenario, "dependency_dispatch") || scenario == "changed_source_receipt"})
			switch scenario {
			case "changed_source_receipt":
				items := c.taskTracker.TodoList().Items()
				items[1].ExecutionReceipt.StepBudget = &StepBudgetUsage{Used: 1, Limit: 2}
				c.taskTracker.TodoList().Restore(items)
				if err := c.emitTaskEventsFromCheckpoint(items); err != nil {
					t.Fatal(err)
				}
				var err error
				source, err = ReadResumeSource(c.session.Workspace, "main", "run-test")
				if err != nil {
					t.Fatal(err)
				}
			case "candidate_missing":
				original := todoItemByID(source.Session.Tasks, id)
				if err := os.Remove(filepath.Join(mustArtifactStore(t, c.session.Workspace).root, "data", original.ResultCandidate.ArtifactID)); err != nil {
					t.Fatal(err)
				}
			case "generation_drift":
				c.session.Agents["worker"].Generation.MaxTokens = "7"
			case "scope_drift":
				c.session.ContractTasks[0].Execution.RequiresEvidence = true
			case "schema_drift":
				compiled, err := CompileResultContractDocument("answer.json", []byte(`{"type":"object","properties":{"different":{"type":"string"}}}`))
				if err != nil {
					t.Fatal(err)
				}
				c.session.ResultContracts[compiled.ID] = compiled
				ref := compiled.ref(true)
				c.session.ContractTasks[0].ResultContract = &ref
			case "response_channel_drift":
				c.session.Config.CollectorResponseChannel = agent.CollectorResponseFunction
			}
			if _, err := c.AssessContractRecovery(contractCollectionContext(t, id), source); err == nil {
				t.Fatal("unsafe legacy source or authority change was admitted")
			}
		})
	}
}

func TestContractRecoveryRejectsForgedLegacySubmissionProof(t *testing.T) {
	for _, scenario := range []string{"candidate", "receipt", "attempts", "execution", "source_digest", "input_digest", "evidence_digest", "policy"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id := legacySubmissionFixture(t, "http://127.0.0.1:1/v1")
			plan, err := c.AssessContractRecovery(contractCollectionContext(t, id), source)
			if err != nil {
				t.Fatal(err)
			}
			proof := plan.Tasks[0].LegacySubmission
			switch scenario {
			case "candidate":
				proof.Candidate.SHA256 = strings.Repeat("a", 64)
			case "receipt":
				proof.ReceiptSHA256 = strings.Repeat("a", 64)
			case "attempts":
				proof.AttemptsUsed = 1
			case "execution":
				proof.Execution.RequiresEvidence = true
			case "source_digest":
				proof.SourceTaskSHA256 = strings.Repeat("a", 64)
			case "evidence_digest":
				proof.EvidenceSHA256 = strings.Repeat("a", 64)
			case "input_digest":
				proof.OriginalTaskInputSHA256 = strings.Repeat("a", 64)
			case "policy":
				plan.Policy.ResultCollection.TurnLimit = 10
			}
			plan.SHA256, err = recoveryPlanHash(*plan)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: "forged-child", Payload: mustProjectionJSON(t, plan)}
			if _, err := validateContractRecoveryLineage(event, mustReadEvents(t, c.eventStore), source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("forged legacy proof replayed")
			}
		})
	}
}
