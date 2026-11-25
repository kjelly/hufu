package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/tools"
)

func requiredReadFixture(t *testing.T, content string) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt, fantasy.AgentTool) {
	t.Helper()
	return requiredReadTaskFixture(t, content, false, nil)
}

func requiredReadTaskFixture(t *testing.T, content string, runtime bool, configure func(*TodoSpec)) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt, fantasy.AgentTool) {
	t.Helper()
	var ref ArtifactRef
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", runtime, func(spec *TodoSpec) {
		ranges := []tools.ArtifactByteRange{}
		if len(content) > 0 {
			ranges = append(ranges, tools.ArtifactByteRange{Start: 0, End: int64(len(content))})
		}
		spec.WorksetBinding = &WorksetBinding{WorksetID: "evidence", ItemKey: "unit", Inputs: []ArtifactRef{ref}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize, Ranges: ranges}}}
		if configure != nil {
			configure(spec)
		}
	}, func(c *Coordinator) {
		store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.Put(t.Context(), PutArtifactRequest{Kind: "source", Path: "source.txt", Content: []byte(content), RunID: c.executionRunID, TaskID: "producer", Attempt: 1, Agent: "producer"})
		if err != nil {
			t.Fatal(err)
		}
		ref = stored.ArtifactRef
	})
	scope := &ArtifactAccessScope{RunID: receipt.RunID, TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{ref}}
	ctx = context.WithValue(ctx, artifactAccessScopeKey, scope)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
	ctx = tools.SetToolsAllowed(ctx, []string{"view"})
	receipt.ArtifactScope = scope
	return c, item, ctx, receipt, tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
}

func deliverRequiredRead(t *testing.T, c *Coordinator, ctx context.Context, view fantasy.AgentTool, callID, input string) {
	t.Helper()
	call := fantasy.ToolCall{ID: callID, Name: "view", Input: input}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	if err := c.captureRequiredArtifactRead(ctx, view, call, response); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredArtifactReadsRequireReturnedBytes(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "empty"}[empty], func(t *testing.T) {
			content := strings.Repeat("A", 600) + "界B needle " + strings.Repeat("z", 600)
			if empty {
				content = ""
			}
			c, item, ctx, receipt, view := requiredReadFixture(t, content)
			var gaps *artifactReadCoverageIncomplete
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) || len(gaps.MissingReads) != 1 {
				t.Fatalf("unread admitted: %v", err)
			}
			ref := item.WorksetBinding.Inputs[0]
			if !empty {
				deliverRequiredRead(t, c, ctx, view, "search", `{"artifact_ref":"`+ref.ID+`","search_text":"needle","context_bytes":0}`)
				if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) {
					t.Fatalf("search scan counted as delivery: %v", err)
				}
				deliverRequiredRead(t, c, ctx, view, "prefix", `{"artifact_ref":"`+ref.ID+`","byte_offset":0,"byte_limit":5}`)
				if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) {
					t.Fatalf("partial bytes admitted: %v", err)
				}
			}
			deliverRequiredRead(t, c, ctx, view, "full", `{"artifact_ref":"`+ref.ID+`","byte_offset":0}`)
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err != nil {
				t.Fatal(err)
			}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "report")
			if err != nil {
				t.Fatal(err)
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			replay := ReduceToTodoList(events)
			if len(replay) != 1 || !reflect.DeepEqual(replay[0].WorksetBinding, item.WorksetBinding) {
				t.Fatal("required reads lost during replay")
			}
			if _, err := c.loadCollectionWork(ctx, replay[0], cp); err != nil {
				t.Fatal(err)
			}
			restored, err := c.restoreCollectionContext(context.WithValue(t.Context(), sessionResumeKey{}, true), cp)
			if err != nil {
				t.Fatal(err)
			}
			restored = context.WithValue(restored, resultCollectionAdmissionKey{}, cp)
			if err := c.validateRequiredArtifactReadSubmission(restored, item.ID); err != nil {
				t.Fatalf("result-only repair lost delivered work: %v", err)
			}
		})
	}
}

func TestRequiredArtifactReadsRejectDifferentExecutionIdentity(t *testing.T) {
	for _, field := range []string{"task", "run", "attempt", "model", "occurrence", "dispatch", "contract"} {
		t.Run(field, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "source")
			deliverRequiredRead(t, c, ctx, view, "read", `{"artifact_ref":"`+item.WorksetBinding.Inputs[0].ID+`"}`)
			metadata, _ := invocationMetadataFromContext(ctx)
			identity, err := submitResultRuntimeIdentityFromContext(ctx, c, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "task":
				metadata.TaskID = "other"
			case "run":
				metadata.RunID = "other"
			case "attempt":
				metadata.Attempt++
			case "model":
				metadata.ModelExecutionID = "other"
			case "occurrence":
				identity.OccurrenceRevision++
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "dispatch":
				identity.DispatchID = "other"
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "contract":
				item.WorksetBinding.ItemKey = "other"
				if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
					t.Fatal(err)
				}
			}
			ctx = withInvocationMetadata(ctx, metadata)
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
				t.Fatal("different identity reused read evidence")
			}
		})
	}
}

func TestRequiredArtifactReadRangeUnion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		observed, want []tools.ArtifactByteRange
	}{
		{name: "overlap", observed: []tools.ArtifactByteRange{{Start: 4, End: 8}, {Start: 0, End: 5}, {Start: 8, End: 10}}},
		{name: "hole", observed: []tools.ArtifactByteRange{{Start: 0, End: 4}, {Start: 6, End: 10}}, want: []tools.ArtifactByteRange{{Start: 4, End: 6}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingArtifactByteRanges([]tools.ArtifactByteRange{{Start: 0, End: 10}}, tc.observed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("missing=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRequiredArtifactReadsValidateAssignedIdentity(t *testing.T) {
	ref := ArtifactRef{ID: "source", SHA256: strings.Repeat("a", 64), ByteSize: 10}
	base := tools.ArtifactReadCoverage{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: 10, Ranges: []tools.ArtifactByteRange{{Start: 0, End: 10}}}
	for _, field := range []string{"valid", "hash", "size", "range", "duplicate", "unassigned"} {
		t.Run(field, func(t *testing.T) {
			required := cloneArtifactReadCoverage([]tools.ArtifactReadCoverage{base})
			switch field {
			case "hash":
				required[0].SHA256 = strings.Repeat("b", 64)
			case "size":
				required[0].ByteSize++
			case "range":
				required[0].Ranges[0].End++
			case "duplicate":
				required = append(required, base)
			case "unassigned":
				required[0].ArtifactID = "other"
			}
			err := validateRequiredArtifactReads([]ArtifactRef{ref}, required)
			if (err == nil) != (field == "valid") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestRequiredArtifactReadObservationRejectsContentTampering(t *testing.T) {
	c, item, ctx, _, view := requiredReadFixture(t, "source")
	call := fantasy.ToolCall{ID: "read", Name: "view", Input: `{"artifact_ref":"` + item.WorksetBinding.Inputs[0].ID + `"}`}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatal(err)
	}
	response.Content = "substituted"
	if err := c.captureRequiredArtifactRead(ctx, view, call, response); err != nil {
		t.Fatal(err)
	}
	if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
		t.Fatal("tampered response counted")
	}
	raw, _ := json.Marshal(map[string]any{"version": 1})
	if err := validateArtifactReadObservationEvent(RunEvent{Payload: raw}); err == nil {
		t.Fatal("malformed durable observation accepted")
	}
}

func TestRequiredArtifactReadsRejectUnsupportedExecution(t *testing.T) {
	for _, kind := range []string{"local", "sidecar", "action", "steps", "closed_sequence", "fanout", "no_view", "no_journal"} {
		t.Run(kind, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "source")
			task := taskDefFromTodoItem(item)
			resolved := ResolvedWorkerTools{Tools: []fantasy.AgentTool{view}, AuthorizedNames: []string{"view"}}
			switch kind {
			case "sidecar":
				task.Sidecar = true
			case "action":
				task.Action = &Action{}
			case "steps":
				task.Execution.Steps = []ExecutionStep{{}}
			case "closed_sequence":
				task.Execution.ToolSequence = []string{"submit_result"}
			case "fanout":
				task.ExecutionTopology = append(task.ExecutionTopology, task.ResolvedExecutionTarget)
			case "no_view":
				resolved.Tools = nil
			case "no_journal":
				store, journal := c.eventStore, c.eventJournal
				t.Cleanup(func() { c.eventStore, c.eventJournal = store, journal })
				c.eventStore, c.eventJournal = nil, nil
			}
			err := c.validateRequiredArtifactReadExecution(ctx, task, item, resolved)
			if (err == nil) != (kind == "local") {
				t.Fatalf("preflight %s: %v", kind, err)
			}
		})
	}
}

func TestRequiredArtifactReadsVerifyPersistedEvidenceAfterDelivery(t *testing.T) {
	for _, kind := range []string{"source", "response"} {
		t.Run(kind, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "original source")
			ref := item.WorksetBinding.Inputs[0]
			budgetCtx, budget := WithArtifactReadBudget(ctx, 1<<20)
			deliverRequiredRead(t, c, budgetCtx, view, "bounded", `{"artifact_ref":"`+ref.ID+`","byte_offset":0}`)
			if budget.Err() != nil {
				t.Fatal(budget.Err())
			}
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err != nil {
				t.Fatal(err)
			}
			if kind == "response" {
				events, err := c.readActiveLineageEvents(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type == string(EventArtifactReadCoverageObserved) {
						var observation artifactReadObservation
						if err := json.Unmarshal(event.Payload, &observation); err != nil {
							t.Fatal(err)
						}
						ref = observation.Output
					}
				}
			}
			store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("tampered evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
				t.Fatal("changed persisted evidence reused prior coverage")
			}
		})
	}
}

func TestRequiredArtifactReadsExcludeWithheldDecisionResponses(t *testing.T) {
	f, view, input := boundReadBudgetFixture(t, 7000, "UNSEEN"+strings.Repeat("\"\\\n", 1500))
	scope, _ := artifactAccessScopeFromContext(f.ctx)
	ref := scope.AuthorizedRefs[len(scope.AuthorizedRefs)-1]
	item := todoByID(t, f.c, f.item.ID)
	item.WorksetBinding = &WorksetBinding{WorksetID: "evidence", ItemKey: "unit", Inputs: []ArtifactRef{ref}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize, Ranges: []tools.ArtifactByteRange{{Start: 0, End: ref.ByteSize}}}}}
	if err := f.c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "withheld", Name: "view", Input: input})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "decision_read_budget_exhausted") {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	events, err := f.c.readActiveLineageEvents(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventArtifactReadCoverageObserved) {
			t.Fatal("withheld source earned delivery coverage")
		}
	}
}

func TestRequiredArtifactReadsSurviveWorkerToCollectorHandoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		want RepairFailureReason
	}{
		{name: "valid"},
		{name: "missing", want: RepairFailureSource},
		{name: "corrupt", want: RepairFailureSource},
		{name: "transient", want: RepairFailureCommit},
		{name: "checkpoint_write", want: RepairFailureCommit},
	} {
		t.Run(tc.name, func(t *testing.T) { testRequiredArtifactCollectionHandoff(t, tc.name, tc.want) })
	}
}

func testRequiredArtifactCollectionHandoff(t *testing.T, mode string, want RepairFailureReason) {
	t.Helper()
	var collections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		if _, typed := request["tool_choice"].(map[string]any); !typed {
			writeNativeRepairStream(t, w, map[string]any{"content": "review finished"}, "stop", "test")
			return
		}
		assertCollectorResponseChannel(t, request)
		collections.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"collection\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", `{"status":"success","summary":"complete read-backed work"}`)
	}))
	t.Cleanup(server.Close)
	c, producer, _, receipt := resultCollectionIdentityFixture(t, server.URL+"/v1", true, "", nil, func(c *Coordinator, spec *TodoSpec, _ []string) {
		if err := c.resolveRunInputsForInvocation(t.Context(), "review the read-backed workset"); err != nil {
			t.Fatal(err)
		}
		snapshot := c.RunInputSnapshot()
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}, func(c *Coordinator) {
		c.session.Agents["coordinator"] = &agent.AgentDef{Name: "coordinator", Role: "coordinator", Generation: c.session.Agents["worker"].Generation}
		c.session.Scope = WorkspaceScope{Managed: true, ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir}
		c.session.Agents["worker"].Tools = "view"
		c.coreTools = []fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))}
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(t.Context(), PutArtifactRequest{Kind: "source", Path: "source.txt", Content: []byte("read before collection"), RunID: c.executionRunID, TaskID: producer.ID, Attempt: 1, Agent: producer.Agent})
	if err != nil {
		t.Fatal(err)
	}
	prepared := &TaskResult{TaskID: producer.ID, Attempt: 1, Agent: producer.Agent, Status: TaskResultStatusSuccess, Summary: "prepared source", Source: "runtime", Artifacts: []ArtifactRef{source.ArtifactRef}}
	if err := c.taskTracker.TodoList().SetTypedResult(producer.ID, prepared); err != nil {
		t.Fatal(err)
	}
	if err := c.setAttemptReceipt(producer.ID, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(t.Context(), producer.ID, TaskInProgress, TaskDone, "prepared", "", nil); err != nil {
		t.Fatal(err)
	}
	input := `{"artifact_ref":"` + source.ID + `","byte_offset":0}`
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		view := c.gatePolicyTools(c.coreTools)[0]
		response, err := view.Run(ctx, fantasy.ToolCall{ID: "work-read", Name: "view", Input: input})
		if err != nil || response.IsError {
			return nil, fmt.Errorf("read failed: %v %s", err, response.Content)
		}
		path := normalizedArtifactData(c, source.ID)
		switch mode {
		case "missing", "transient":
			if err := os.Rename(path, path+".saved"); err != nil {
				return nil, err
			}
			if mode == "transient" {
				if err := os.Symlink(filepath.Base(path), path); err != nil {
					return nil, err
				}
			}
		case "corrupt":
			if err := os.WriteFile(path, []byte("changed after read"), 0o600); err != nil {
				return nil, err
			}
		}
		return textAgentResult("Completed work using the returned source."), nil
	}})
	c.workerAgentOverride = worker
	manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "one", Bindings: map[string]string{"name": "one"}, Inputs: []ArtifactRef{source.ArtifactRef}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: source.ID, SHA256: source.SHA256, ByteSize: source.ByteSize, Ranges: []tools.ArtifactByteRange{{Start: 0, End: source.ByteSize}}}}}}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeWorksetManifest(t, c.session.Workspace, string(data))
	if mode == "checkpoint_write" {
		execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_collection BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' BEGIN SELECT RAISE(ABORT, 'fixture collection write failure'); END`)
	}
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ID: "reviews", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true}, FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "Review {name}"}}})
	if want != "" {
		assertStoppedArtifactCollection(t, c, worker.Calls(), collections.Load(), want)
		if want == RepairFailureCommit {
			switch mode {
			case "transient":
				path := normalizedArtifactData(c, source.ID)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			case "checkpoint_write":
				execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_collection")
			}
			assertResumedArtifactCollection(t, c, mode == "checkpoint_write", &collections)
		}
		return
	}
	if err != nil {
		for _, item := range c.taskTracker.TodoList().Items()[1:] {
			t.Logf("child %s status=%s detail=%s output=%s", item.ID, item.Status, item.Detail, item.Output)
		}
		t.Fatal(err)
	}
	items := c.taskTracker.TodoList().Items()[1:]
	if len(items) != 1 || items[0].Status != TaskDone || items[0].TypedResult == nil || items[0].TypedResult.Source != "collected" || worker.Calls() != 1 || collections.Load() != 1 {
		t.Fatalf("handoff incomplete: items=%+v work=%d collection=%d", items, worker.Calls(), collections.Load())
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replay := ReduceToTodoList(events)
	if len(replay) != 2 || replay[1].TypedResult == nil || replay[1].TypedResult.Source != "collected" {
		t.Fatal("replay lost read-backed collected result")
	}
}

func assertStoppedArtifactCollection(t *testing.T, c *Coordinator, workerCalls int, collections int32, want RepairFailureReason) {
	t.Helper()
	for _, items := range [][]*TodoItem{c.taskTracker.TodoList().Items(), ReduceToTodoList(readJournal(t, c.session.Workspace))} {
		if len(items) != 2 {
			t.Fatalf("expected producer and its child, got %d tasks", len(items))
		}
		item := items[1]
		if want == RepairFailureCommit && item.Status == TaskInProgress && item.TypedResult == nil && workerCalls == 1 && collections == 0 {
			continue
		}
		checkpoint := currentResultCollection(item)
		if item.Status != TaskBlocked || item.TypedResult != nil || item.ExecutionReceipt == nil || checkpoint == nil || checkpoint.State != ResultCollectionBlocked ||
			item.ExecutionReceipt.RepairProvenance == nil || item.ExecutionReceipt.RepairProvenance.FailureReason != want || workerCalls != 1 || collections != 0 {
			t.Fatalf("collection handoff did not durably stop with %q: task=%s receipt=%+v workers=%d collectors=%d", want, item.Status, item.ExecutionReceipt, workerCalls, collections)
		}
	}
}

func assertResumedArtifactCollection(t *testing.T, c *Coordinator, unpublished bool, collections *atomic.Int32) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(c.session.Workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	child := source.Session.Tasks[1]
	seal, err := c.readReturnedCollection(child, 0)
	if err != nil || seal == nil || (currentResultCollection(child) == nil) != unpublished {
		t.Fatalf("returned work is not discoverable after restart: checkpoint=%+v seal=%+v error=%v", currentResultCollection(child), seal, err)
	}
	reader := newRecoveryCoordinator(t, c)
	reader.executionRunID = ""
	reader.providerBoundaryStart = func(context.Context, string) error { return nil }
	reader.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		reader.finishCalled.Store(true)
		return "review finished", nil, nil
	}
	if err := reader.AuthorizeSessionRestore(source); err != nil {
		t.Fatal(err)
	}
	reader.SetSessionData(source.Session)
	forbidden := scriptWorker(failTurn(errors.New("sealed work ran again")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	_, err = reader.Run(WithSessionResume(t.Context()), "resume original read-backed work")
	done := todoByID(t, reader, child.ID)
	if err != nil || done.Status != TaskDone || currentResultCollection(done) == nil || currentResultCollection(done).Work != seal.Work ||
		collections.Load() != 1 || forbidden.Calls() != 0 {
		t.Fatalf("public resume lost sealed work: status=%s collections=%d work=%d error=%v", done.Status, collections.Load(), forbidden.Calls(), err)
	}
}

func TestSubmitResultCanReadMissingAvailableEvidenceOnNextTurn(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "read_then_resubmit", true: "corrupt_source_stops"}[corrupt], func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "authorized review input")
			ref := item.WorksetBinding.Inputs[0]
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			ctx = withTestAuxiliaryInvocationContext(ctx)
			ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
			ctx = tools.SetToolsAllowed(ctx, []string{"view", submitResultToolName})
			const submission = `{"status":"success","summary":"reviewed"}`
			model := &rejectedResultStreamModel{
				finish: fantasy.FinishReasonToolCalls, sibling: submission, pauseAt: 4,
				onCall: func(generation int) {
					if corrupt && generation == 1 {
						if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("altered"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				},
				toolAt: func(i int) string {
					if i == 2 {
						return "view"
					}
					return submitResultToolName
				},
				input: func(i int) string {
					if i == 2 {
						return fmt.Sprintf(`{"artifact_ref":%q}`, ref.ID)
					}
					if corrupt {
						return `{"status":"success","summary":["wrong shape"]}`
					}
					return submission
				},
			}
			available := c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: item.ID}, view})
			worker := fantasy.NewAgent(model, fantasy.WithTools(available...), fantasy.WithMaxRetries(0))
			_, steps, err := c.runAgentWithStatusAndHistory(ctx, worker, item.Agent, "review the assigned source", nil, &taskTiming{}, fantasy.StepCountIs(4))
			result := c.GetTaskResult(item.ID)
			if corrupt {
				if reason := submissionFailureFromSteps(steps); model.calls != 1 || reason != RepairFailureSource ||
					(result != nil && isLocalSubmittedResultSource(result.Source)) {
					t.Fatalf("corrupt evidence result: calls=%d reason=%q result=%+v err=%v steps=%+v", model.calls, reason, result, err, steps)
				}
				return
			}
			if err != nil || model.calls != 3 || result == nil || !isLocalSubmittedResultSource(result.Source) {
				t.Fatalf("view and resubmit did not complete: calls=%d result=%+v err=%v", model.calls, result, err)
			}
		})
	}
}

func TestResultCollectionChecksRequiredSourceBeforeMalformedClaims(t *testing.T) {
	c, item, ctx, receipt, view := requiredReadFixture(t, "authorized review input")
	ref := item.WorksetBinding.Inputs[0]
	deliverRequiredRead(t, c, ctx, view, "assigned-read", fmt.Sprintf(`{"artifact_ref":%q}`, ref.ID))
	checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "review completed")
	if err == nil {
		checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "collector", []byte(`{"tools":[]}`))
	}
	if err == nil {
		checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":["wrong shape"]}`))
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepareCollectionClaims(ctx, checkpoint); localResultRejectionReason(err) != RepairFailureInvalidSchema {
		t.Fatalf("valid evidence and bad claims = %v", err)
	}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("altered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.prepareCollectionClaims(ctx, checkpoint); localResultRejectionReason(err) != RepairFailureSource {
		t.Fatalf("malformed claims hid invalid evidence: %v", err)
	}
}

type artifactReadFaultJournal struct {
	EventJournal
	err error
}

func (j artifactReadFaultJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	return nil, j.err
}

func TestRequiredArtifactReadsClassifyStorageFaultsBeforeClaims(t *testing.T) {
	c, item, ctx, receipt, view := requiredReadFixture(t, "data delivered during work")
	ref := item.WorksetBinding.Inputs[0]
	deliverRequiredRead(t, c, ctx, view, "read", fmt.Sprintf(`{"artifact_ref":%q}`, ref.ID))
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "reviewed")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":false}`))
	}
	if err != nil {
		t.Fatal(err)
	}
	faultRoot := t.TempDir()
	if err := os.Mkdir(runtimestore.Path(faultRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	store, fault := runtimestore.Open(ctx, faultRoot)
	if store != nil {
		_ = store.Close()
	}
	if !runtimestore.IsStorageFault(fault) {
		t.Fatalf("expected actual SQLite open failure, got %v", fault)
	}
	c.SetEventJournal(artifactReadFaultJournal{EventJournal: c.EventJournal(), err: fault})
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":false}`})
	if err != nil || !response.StopTurn || submissionFailureMetadata(response.Metadata) != RepairFailureCommit {
		t.Fatalf("worker storage fault = %+v, %v", response, err)
	}
	if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureCommit || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("collector storage fault = %v", err)
	}
}

func TestRequiredArtifactReadsPreviewBlocksFormatRepairWithUnavailableEvidence(t *testing.T) {
	c, item, ctx, receipt, view := requiredReadFixture(t, "immutable reviewed source")
	ref := item.WorksetBinding.Inputs[0]
	deliverRequiredRead(t, c, ctx, view, "read", fmt.Sprintf(`{"artifact_ref":%q}`, ref.ID))
	compiled, contract := compiledReviewContract(t, true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	item.ResultContract = &contract
	calls := 0
	bindTestNormalizer(t, c, item, &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
		calls++
		return raw, nil
	}})
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "reviewed")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(`{"result":null}`), ExecutionUsage{}, &agent.StructuredResponseIssue{
			Code: "type", Pointer: "/result", Expected: "object", Actual: "null",
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	c.revokeTaskOccurrence(item.ID)
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(store.root, "data", ref.ID)
	preview := func() (ResultCollectionRecoveryAssessment, map[string]error) {
		a := ResultCollectionRecoveryAssessment{Checkpoint: cp, OriginalReason: RepairFailureInvalidSchema, Disposition: RecoveryBlocked}
		checks := make(map[string]error)
		c.assessResultCollectionWork(ctx, c.todoItemByID(item.ID), &a, func(name string, err error) bool {
			checks[name] = err
			return err == nil
		})
		return a, checks
	}
	if a, checks := preview(); a.Disposition != RecoveryRepairClaims || calls != 0 {
		t.Fatalf("intact preview = %+v, checks=%v, actions=%d", a, checks, calls)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if a, checks := preview(); a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 || checks["required_artifact_reads"] == nil || calls != 0 {
		t.Fatalf("missing source preview = %+v, checks=%v, actions=%d", a, checks, calls)
	}
	if err := os.WriteFile(source, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a, checks := preview(); a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 || checks["required_artifact_reads"] == nil || calls != 0 {
		t.Fatalf("corrupt source preview = %+v, checks=%v, actions=%d", a, checks, calls)
	}
	c.artifactStoreRoot = t.TempDir()
	if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
		t.Fatal("nonexistent store admitted")
	}
	if _, err := os.Stat(filepath.Join(c.artifactStoreRoot, logsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read path created artifact storage: %v", err)
	}
}
