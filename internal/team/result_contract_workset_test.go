package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

const worksetRowsSchema = `{"type":"object","required":["rows"],"properties":{"rows":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}},"x-hufu-workset-inputs":{"items_pointer":"/rows","item_id_pointer":"/id","binding":"target_ids"}}`

func worksetResultFixture(t *testing.T, ids string) (*Coordinator, *TodoItem, *CompiledResultContract) {
	t.Helper()
	c, _, item, compiled := runtimeWorksetInputFixtureWithSchema(t, worksetRowsSchema, map[string]string{"identity": "assigned", "target_ids": ids})
	return c, item, compiled
}

func TestWorksetInputExtensionPreflight(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`null`, `{}`, `{"items_pointer":"/rows","item_id_pointer":"/id","binding":""}`, `{"items_pointer":"rows","item_id_pointer":"/id","binding":"ids"}`, `{"items_pointer":"/rows","item_id_pointer":"/id","binding":"ids","unknown":true}`} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if _, err := compileResultWorksetInputs(map[string]any{"x-hufu-workset-inputs": v}); err == nil {
			t.Fatalf("invalid extension accepted: %s", raw)
		}
	}
	c, _, compiled := worksetResultFixture(t, `[]`)
	if compiled.worksetInputs == nil || len(c.session.ResultContracts) != 1 {
		t.Fatal("extension was not compiled")
	}
}

func TestWorksetInputOrderedCoverage(t *testing.T) {
	t.Parallel()
	spec := &resultWorksetInputsSpec{ItemsPointer: "/rows", ItemIDPointer: "/id", Binding: "target_ids"}
	for _, tc := range []struct {
		name, ids, value string
		valid            bool
	}{
		{"asset inventory", `["asset-a","asset-b"]`, `{"rows":[{"id":"asset-a","state":"checked"},{"id":"asset-b"}]}`, true},
		{"translation segments", `["segment-1"]`, `{"rows":[{"id":"segment-1","text":"translated"}]}`, true},
		{"empty", `[]`, `{"rows":[]}`, true},
		{"foreign", `[]`, `{"rows":[{"id":"other"}]}`, false},
		{"missing", `["a"]`, `{"rows":[]}`, false},
		{"substituted", `["a"]`, `{"rows":[{"id":"b"}]}`, false},
		{"reordered", `["a","b"]`, `{"rows":[{"id":"b"},{"id":"a"}]}`, false},
		{"duplicated", `["a","b"]`, `{"rows":[{"id":"a"},{"id":"a"}]}`, false},
		{"null result", `[]`, `{"rows":null}`, false},
		{"missing result", `[]`, `{}`, false},
		{"null binding", `null`, `{"rows":[]}`, false},
		{"duplicate binding", `["a","a"]`, `{"rows":[{"id":"a"},{"id":"a"}]}`, false},
		{"empty id binding", `[""]`, `{"rows":[{"id":""}]}`, false},
		{"malformed binding", `[`, `{"rows":[]}`, false},
		{"nonstring binding", `[1]`, `{"rows":[{"id":1}]}`, false},
		{"nonstring result", `["a"]`, `{"rows":[{"id":{}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := decodeResultContractJSON([]byte(tc.value))
			if err != nil {
				t.Fatal(err)
			}
			binding := &WorksetBinding{Bindings: map[string]string{"target_ids": tc.ids}}
			err = validateResultWorksetInputs(v, spec, binding)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	if err := validateResultWorksetInputs(map[string]any{}, spec, nil); err == nil {
		t.Fatal("unbound result accepted")
	}
	if err := validateResultWorksetInputs(map[string]any{}, spec, &WorksetBinding{}); err == nil {
		t.Fatal("missing binding accepted")
	}
}

func TestWorksetSubmissionRejectsBeforePublicationAndAllowsCorrection(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, item, compiled := worksetResultFixture(t, `[]`)
			ctx := occurrenceTestContext(c, item.ID, 1)
			for _, valid := range []bool{false, true} {
				value := `{"rows":[{"id":"sibling-only"}]}`
				if valid {
					value = `{"rows":[]}`
				}
				if route == "external" {
					request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: compiled,
						resultWorksetInputIDs: func() ([]string, error) {
							return c.verifiedResultWorksetInputIDs(ctx, item.ID, compiled.worksetInputs)
						}}
					_, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(value)}, nil)
					if (err == nil) != valid {
						t.Fatalf("valid=%v error=%v", valid, err)
					}
				} else {
					response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"checked assigned rows","structured_payload":` + value + `}`})
					if err != nil || response.IsError == valid {
						t.Fatalf("valid=%v response=%#v error=%v", valid, response, err)
					}
					if !valid && (c.GetTaskResult(item.ID) != nil || item.TypedResult != nil) {
						t.Fatal("rejected result was published")
					}
					if !valid && !strings.Contains(response.Content, "requires exactly 0 items") {
						t.Fatalf("missing corrective feedback: %s", response.Content)
					}
					if valid && c.GetTaskResult(item.ID) == nil {
						t.Fatal("corrected result was not accepted")
					}
				}
			}
		})
	}
}

func TestWorksetResultReplayAndDependenciesUseDurableBinding(t *testing.T) {
	t.Parallel()
	c, item, compiled := worksetResultFixture(t, `["asset-a"]`)
	payload, err := validateStructuredResultPayload(compiled, *item.ResultContract, []byte(`{"rows":[{"id":"asset-a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var restored TodoItem
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	todos := c.taskTracker.TodoList()
	items := todos.Items()
	for _, changed := range []bool{false, true} {
		if changed {
			restored.WorksetBinding.Bindings["target_ids"] = `["asset-b"]`
		}
		for i, candidate := range items {
			if candidate.ID == restored.ID {
				items[i] = &restored
			}
		}
		todos.Restore(items)
		todos.SetRunID(c.executionRunID)
		for _, err := range []error{c.validateRequiredEvidenceDelivery(t.Context(), restored.ID, 1), c.validateAcceptedDependencyPayload(&restored)} {
			if (err != nil) != changed {
				t.Fatalf("changed=%v error=%v", changed, err)
			}
		}
	}
}

func TestWorksetBindingIsCheckedAgainAtOccurrenceCommit(t *testing.T) {
	t.Parallel()
	c, item, compiled := worksetResultFixture(t, `[]`)
	ctx := occurrenceTestContext(c, item.ID, 1)
	for _, valid := range []bool{false, true} {
		value := `{"rows":[{"id":"sibling"}]}`
		if valid {
			value = `{"rows":[]}`
		}
		payload, err := validateStructuredResultPayload(compiled, *item.ResultContract, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		err = (coordinatorTaskResultSink{coordinator: c}).Submit(ctx, item.ID, TaskResult{Status: TaskResultStatusSuccess, Summary: "canonical proposal", StructuredPayload: payload})
		if (err == nil) != valid {
			t.Fatalf("valid=%v error=%v", valid, err)
		}
		if !valid && (c.GetTaskResult(item.ID) != nil || item.TypedResult != nil) {
			t.Fatal("commit published out-of-scope rows")
		}
	}
}

func TestWorksetCompletedResultRequiresManifestAfterSQLiteReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, schema, payload string }{
		{"assigned IDs", worksetRowsSchema, `{"rows":[{"id":"asset-a"}]}`},
		{"assigned path", worksetPathResultSchema, `{"findings":[{"location":"dir/source.go:2","summary":"reviewed"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testWorksetCompletedResultManifest(t, tc.schema, tc.payload)
		})
	}
}

func testWorksetCompletedResultManifest(t *testing.T, schema, canonicalPayload string) {
	t.Helper()
	c, producer, item, compiled := runtimeWorksetInputFixtureWithSchema(t, schema,
		map[string]string{"identity": "assigned", "target_ids": `["asset-a"]`}, "dir/source.go")
	runID := c.executionRunID
	snapshot := mustRunInputSnapshot(t, nil, nil)
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "transcript", Path: "work.txt", Content: []byte("reviewed asset-a"), TaskID: item.ID, Agent: item.Agent, Attempt: 1, RunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(compiled, *item.ResultContract, []byte(canonicalPayload))
	if err != nil {
		t.Fatal(err)
	}
	item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Attempt: 1, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	item.Status = TaskDone
	item.RunInputSnapshotID, item.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	item.ExecutionReceipt = &ExecutionReceipt{
		TaskID: item.ID, ProducerID: item.Agent, RunID: runID, Attempt: 1, ExitCode: new(0), TranscriptRef: transcript.ID,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		ResultContractID: compiled.ID, ResultContractSHA256: item.ResultContract.SchemaSHA256,
	}
	item.ExecutionReceipt.bindResultContractPayload(item.TypedResult)
	c.providerManager, err = agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	for _, completed := range []*TodoItem{producer, item} {
		body, err := json.Marshal(completed, collectionDurationFormat)
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []EventType{EventTaskCreated, EventTaskCompleted} {
			if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(kind), BranchID: "main",
				RunID: runID, TaskID: completed.ID, Actor: completed.Agent, Payload: body}); err != nil {
				t.Fatal(err)
			}
		}
	}
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = runID
	c.taskTracker.TodoList().SetRunID(runID)
	restored := c.todoItemByID(item.ID)
	if restored == nil || restored.WorksetReceipt == nil || restored.TypedResult == nil ||
		!receiptMatchesResultPayload(restored.ExecutionReceipt, restored.TypedResult.StructuredPayload) {
		t.Fatalf("SQLite did not restore the accepted workset occurrence: %+v", restored)
	}
	check := func(wantErr bool) {
		t.Helper()
		for _, err := range []error{
			c.validateAcceptedDependencyPayloadWithContext(t.Context(), restored),
			c.validateCompletedResumeEvidence(t.Context(), restored, snapshot),
		} {
			if (err != nil) != wantErr {
				t.Fatalf("expect manifest rejection=%t: %v", wantErr, err)
			}
		}
	}
	check(false)
	worker := scriptWorker(textTurn("reviewed"))
	c.workerAgentOverride = worker
	c.taskTracker.TodoList().advanceIDFloor(2)
	admitDependent := func() *TodoItem {
		t.Helper()
		spec := TodoSpec{Agent: "worker", Goal: "consume accepted workset", EvidenceFrom: []string{restored.ID}}
		ids := c.taskTracker.TodoList().ReserveIDs(1)
		projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
			t.Fatal(err)
		}
		added, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
		if err != nil {
			t.Fatal(err)
		}
		return added[0]
	}
	dependent := admitDependent()
	budgetCtx, budget := WithArtifactReadBudget(t.Context(), 1)
	if _, err := c.executeTask(budgetCtx, taskDefFromTodoItem(dependent), dependent.ID); !errors.Is(err, ErrReadLimit) || !errors.Is(budget.Err(), ErrReadLimit) {
		t.Fatalf("dispatch did not preserve the workset CAS read budget: %v", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	dependent = admitDependent()
	if _, err := c.executeTask(cancelled, taskDefFromTodoItem(dependent), dependent.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled dispatch did not stop: %v", err)
	}
	dependent = admitDependent()
	if err := os.Remove(filepath.Join(store.root, "data", restored.WorksetBinding.SourceArtifactID)); err != nil {
		t.Fatal(err)
	}
	check(true)
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(dependent), dependent.ID); err == nil || !strings.Contains(err.Error(), "stored result bindings") {
		t.Fatalf("dispatch reused a result without its workset CAS: %v", err)
	}
	if got := worker.Calls(); got != 0 {
		t.Fatalf("dependency denial reached the worker: %d calls", got)
	}
}
