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
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestHistoricalNormalizedResultRemainsUsableAsEvidence(t *testing.T) {
	c := newTestCoordinator(t)
	compiled, ref := compiledReviewContract(t, true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	// The 3adbf7e4 payload and receipt recorded the raw and canonical digests
	// and the schema identity, with no provider fingerprint or output record.
	wire := fmt.Sprintf(`{"tasks":[{
		"ID":"source","Status":"done","Agent":"worker",
		"result_contract":{"id":%q,"schema_sha256":%q,"require_structured":true,"normalizer":"normalize-result"},
		"typed_result":{"task_id":"source","attempt":1,"agent":"worker","status":"success","summary":"reviewed","source":"submit_result",
			"structured_payload":{
				"contract":{"id":%q,"schema_sha256":%q,"require_structured":true,"normalizer":"normalize-result"},
				"value":{"findings":[],"verdict":"approve"},
				"sha256":"1229760b12efcb84a7fad2cab503078cd9995618bd0159dc55f67f7d91d30ae1",
				"raw_sha256":"4a76639e5e1ebfa61990bc4c16d05bd6ca98135e0bdcbb3024c5eda926b30bee"}},
		"execution_receipt":{"task_id":"source","run_id":"original","attempt":1,"producer_id":"worker","exit_code":0,
			"model_execution_id":"worker-turn","transcript_ref":"original-transcript","result_validation":"valid",
			"result_contract_id":%q,"result_contract_sha256":%q,
			"result_payload_sha256":"1229760b12efcb84a7fad2cab503078cd9995618bd0159dc55f67f7d91d30ae1",
			"result_raw_payload_sha256":"4a76639e5e1ebfa61990bc4c16d05bd6ca98135e0bdcbb3024c5eda926b30bee"}
	}]}`, ref.ID, ref.SchemaSHA256, ref.ID, ref.SchemaSHA256, ref.ID, ref.SchemaSHA256)
	if err := workspaceDocs(c.session.Workspace).putDocument(runtimestore.DocumentSession, []byte(wire)); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(c.session.Workspace)
	if loaded == nil {
		t.Fatal("historical persisted session could not be read")
	}
	source := todoItemByID(loaded.Tasks, "source")
	if err := c.validateAcceptedDependencyPayload(source); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePersistedResultPayloadReceipt(source, "original"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*TodoItem){
		func(item *TodoItem) { item.ExecutionReceipt.ResultWorkDispatchID = "current-dispatch" },
		func(item *TodoItem) {
			item.ExecutionReceipt.ResultRawPayloadSHA256 = item.TypedResult.StructuredPayload.SHA256
		},
		func(item *TodoItem) {
			item.ResultContract.NormalizerSHA256 = sha256HexBytes([]byte("current provider"))
			item.TypedResult.StructuredPayload.Contract = *item.ResultContract
		},
	} {
		item := todoItemByID(LoadSession(c.session.Workspace).Tasks, "source")
		mutate(item)
		if c.validateAcceptedDependencyPayload(item) == nil || ValidatePersistedResultPayloadReceipt(item, "original") == nil {
			t.Fatal("mismatched or incomplete current evidence accepted as historical evidence")
		}
	}
}

func TestNormalizedDependencyDispatchRequiresOriginalSuccessfulReceipt(t *testing.T) {
	c, _, item := newDirectNormalizationRun(t, echoNormalization, nil)
	c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
	ctx := withTestProtocolRepairInvocationContext(t.Context())
	if _, err := c.executeTask(ctx, taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	source := todoByID(t, c, item.ID)
	if source.Status != TaskDone || source.ExecutionReceipt == nil || source.TypedResult == nil ||
		source.TypedResult.StructuredPayload == nil || source.TypedResult.StructuredPayload.Normalization == nil {
		t.Fatal("worker did not complete with a normalized result and a successful receipt")
	}
	if err := c.validateAcceptedDependencyPayloadWithContext(ctx, source); err != nil {
		t.Fatalf("same-run normalized evidence: %v", err)
	}
	resumed, _ := restartDirectNormalizationRun(t, c, normalizationResumeProjection(t, c), source, echoNormalization)
	recovered := todoByID(t, resumed, source.ID)
	if err := resumed.validateAcceptedDependencyPayloadWithContext(ctx, recovered); err != nil {
		t.Fatalf("recovered input-bound normalized evidence: %v", err)
	}
	bounded, budget := WithArtifactReadBudget(ctx, 1)
	if err := resumed.validateAcceptedDependencyPayloadWithContext(bounded, recovered); !errors.Is(err, ErrReadLimit) || !errors.Is(budget.Err(), ErrReadLimit) {
		t.Fatalf("recovered receipt verification exceeded its read budget: error=%v budget=%v", err, budget.Err())
	}
	for _, test := range []struct {
		name   string
		change func(*ExecutionReceipt)
	}{
		{name: "lost"},
		{name: "wrong run", change: func(r *ExecutionReceipt) { r.RunID = "unrelated-run" }},
		{name: "wrong attempt", change: func(r *ExecutionReceipt) { r.Attempt++ }},
		{name: "wrong producer", change: func(r *ExecutionReceipt) { r.ProducerID = "unrelated-worker" }},
		{name: "wrong frozen inputs", change: func(r *ExecutionReceipt) { r.RunInputSnapshotHash = "unrelated-inputs" }},
		{name: "wrong contract", change: func(r *ExecutionReceipt) { r.ResultContractSHA256 = sha256HexBytes([]byte("schema")) }},
		{name: "wrong raw", change: func(r *ExecutionReceipt) { r.ResultRawPayloadSHA256 = sha256HexBytes([]byte("raw")) }},
		{name: "wrong output", change: func(r *ExecutionReceipt) { r.ResultNormalizerOutputSHA256 = sha256HexBytes([]byte("output")) }},
		{name: "wrong occurrence", change: func(r *ExecutionReceipt) { r.ResultNormalizerOccurrenceSHA256 = sha256HexBytes([]byte("occurrence")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			worker := scriptWorker(failTurn(errors.New("invalid evidence reached model dispatch")))
			c.workerAgentOverride = worker
			list := c.taskTracker.TodoList()
			list.mu.Lock()
			live := todoItemByID(list.items, source.ID)
			current, history := live.ExecutionReceipt, live.ExecutionReceipts
			live.ExecutionReceipt, live.ExecutionReceipts = nil, nil
			if test.change != nil {
				live.ExecutionReceipt = new(cloneExecutionReceipt(current))
				test.change(live.ExecutionReceipt)
				for _, prior := range history {
					receipt := cloneExecutionReceipt(&prior)
					test.change(&receipt)
					live.ExecutionReceipts = append(live.ExecutionReceipts, receipt)
				}
			}
			list.mu.Unlock()
			t.Cleanup(func() {
				list.mu.Lock()
				live.ExecutionReceipt, live.ExecutionReceipts = current, history
				list.mu.Unlock()
			})
			spec := TodoSpec{Agent: "worker", Goal: "consume accepted normalized result", EvidenceFrom: []string{source.ID},
				RunInputSnapshotID: source.RunInputSnapshotID, RunInputSnapshotHash: source.RunInputSnapshotHash}
			ids := list.ReserveIDs(1)
			projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.admitTaskOccurrence(ctx, projection, ids[0], 1); err != nil {
				t.Fatal(err)
			}
			consumers, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, ids)
			if err != nil {
				t.Fatal(err)
			}
			if deps := c.dependencyResultsForTaskWithContext(ctx, ids[0]); len(deps) != 1 || deps[0].TaskID != source.ID {
				t.Fatalf("consumer dependency was lost before dispatch: evidence_from=%v, projected=%d", consumers[0].EvidenceFrom, len(deps))
			}
			if _, err := c.executeTask(ctx, taskDefFromTodoItem(consumers[0]), ids[0]); err == nil ||
				!strings.Contains(err.Error(), "normalized result receipt") || worker.Calls() != 0 {
				t.Fatalf("dispatch did not reject unauthenticated normalized evidence: worker calls=%d error=%v", worker.Calls(), err)
			}
		})
	}
}

func TestLiveNormalizedResultReopensBoundEvidenceBeforeVerification(t *testing.T) {
	for _, transport := range []agent.ResultTransport{agent.ResultTransportTool, agent.ResultTransportRuntime} {
		for _, tc := range []struct {
			name string
			want RepairFailureReason
		}{
			{name: "missing", want: RepairFailureSource},
			{name: "temporary", want: RepairFailureCommit},
		} {
			t.Run(string(transport)+"/"+tc.name, func(t *testing.T) {
				testLiveNormalizedEvidenceDelivery(t, transport, tc.want)
			})
		}
	}
}

func testLiveNormalizedEvidenceDelivery(t *testing.T, transport agent.ResultTransport, want RepairFailureReason) {
	t.Helper()
	const draft = `{"record":{"decision":true}}`
	var collections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var wire map[string]any
		if err := json.UnmarshalRead(request.Body, &wire); err != nil {
			t.Error(err)
			return
		}
		assertCollectorResponseChannel(t, wire)
		collections.Add(1)
		writeNativeRepairStream(t, w, map[string]any{"content": normalizationWire(draft)}, "stop", "test")
	}))
	t.Cleanup(server.Close)
	c, manifest, normalizations := normalizedWorksetDeliveryFixture(t, server.URL+"/v1", transport)
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		world := isolatedAttemptFromContext(ctx)
		if world == nil {
			return nil, errors.New("worker was not given its isolated world")
		}
		if err := os.WriteFile(filepath.Join(world.prepared.Root, "worker-output"), []byte("accepted work"), 0o600); err != nil {
			return nil, err
		}
		if transport == agent.ResultTransportRuntime {
			return textAgentResult("original work completed"), nil
		}
		id, _ := ctx.Value(todoIDKey{}).(string)
		stream, _ := normalizationStreamedSubmission(c, id, normalizationWire(draft), nil)
		return stream.Stream(ctx, call)
	}})
	c.workerAgentOverride = worker
	var restore func()
	c.taskTracker.TodoList().onChange = func() {
		if restore != nil {
			return
		}
		for _, item := range c.taskTracker.TodoList().Items() {
			if item.WorksetBinding != nil && item.TypedResult != nil && item.TypedResult.StructuredPayload != nil {
				restore = faultNormalizedEvidencePath(t, normalizedArtifactData(c, manifest.ID), want == RepairFailureCommit)
				break
			}
		}
	}
	ctx := t.Context()
	if transport == agent.ResultTransportTool {
		ctx = withTestProtocolRepairInvocationContext(ctx)
	}
	_, runErr := c.ExecuteTasks(ctx, []TaskDef{{
		ID: "evaluate", Agent: "worker", Model: "ollama/test", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, Verify: "test -f worker-output && printf 'verified\\n' >> verifier.log",
		FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: manifest.TaskID, Artifact: manifest.ID}, GoalTemplate: "Evaluate {identity}"},
	}})
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 || restore == nil {
		for _, item := range items {
			t.Logf("task %s: status=%s detail=%s output=%s", item.ID, item.Status, item.Detail, item.Output)
		}
		t.Fatalf("work did not reach the accepted evidence boundary: items=%d error=%v", len(items), runErr)
	}
	item := items[1]
	verifier := filepath.Join(c.projectDir, "verifier.log")
	if _, err := os.Stat(verifier); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable evidence reached verification: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.projectDir, "worker-output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable evidence reached apply: %v", err)
	}
	if worker.Calls() != 1 || normalizations.Load() != 1 || (collections.Load() == 1) != (transport == agent.ResultTransportRuntime) {
		t.Fatalf("worker=%d normalizer=%d collector=%d", worker.Calls(), normalizations.Load(), collections.Load())
	}
	assertNormalizedWorksetDeliveryOutcome(t, c, item, verifier, restore, want, runErr)
	if normalizations.Load() != 1 || (collections.Load() == 1) != (transport == agent.ResultTransportRuntime) {
		t.Fatalf("resume repeated normalization or collection: normalizer=%d collector=%d", normalizations.Load(), collections.Load())
	}
}

func normalizedWorksetDeliveryFixture(t *testing.T, endpoint string, transport agent.ResultTransport) (*Coordinator, ArtifactRef, *atomic.Int32) {
	t.Helper()
	compiled, err := CompileResultContractDocument("workset-delivery.json", []byte(runtimeWorksetInputSchema))
	if err != nil {
		t.Fatal(err)
	}
	var normalizations atomic.Int32
	c, producer, _, receipt := resultCollectionIdentityFixture(t, endpoint, true, "", nil, func(c *Coordinator, spec *TodoSpec, _ []string) {
		if err := c.resolveRunInputsForInvocation(t.Context(), "review frozen workset"); err != nil {
			t.Fatal(err)
		}
		snapshot := c.RunInputSnapshot()
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}, func(c *Coordinator) {
		c.session.Agents["worker"].ResultTransport = transport
		c.session.Agents["worker"].WorkerWorkspace = isolatedSpec()
		c.session.Scope = WorkspaceScope{Managed: true, ControlRoot: c.session.Workspace, SubjectRoot: c.projectDir}
		ref := compiled.ref(true)
		ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(_ context.Context, body []byte) ([]byte, error) {
			normalizations.Add(1)
			return body, nil
		}})
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": ref}
	})
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	manifest, err := mustArtifactStore(t, c.session.Workspace).Put(t.Context(), PutArtifactRequest{
		Kind: "workset_manifest", Path: "manifest.json", RunID: receipt.RunID, TaskID: producer.ID, Attempt: 1, Agent: producer.Agent,
		Content: []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"identity":"frozen-value"}}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared := &TaskResult{TaskID: producer.ID, Attempt: 1, Agent: producer.Agent, Status: TaskResultStatusSuccess,
		Summary: "prepared workset", Source: "runtime", Artifacts: []ArtifactRef{manifest.ArtifactRef}}
	if err := c.taskTracker.TodoList().SetTypedResult(producer.ID, prepared); err != nil {
		t.Fatal(err)
	}
	if err := c.setAttemptReceipt(producer.ID, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(t.Context(), producer.ID, TaskInProgress, TaskDone, "prepared", "", nil); err != nil {
		t.Fatal(err)
	}
	return c, manifest.ArtifactRef, &normalizations
}

func assertNormalizedWorksetDeliveryOutcome(t *testing.T, c *Coordinator, item *TodoItem, verifier string, restore func(), want RepairFailureReason, runErr error) {
	t.Helper()
	if item.VerifyResult != nil || runErr == nil {
		t.Fatalf("live evidence failure was not surfaced: status=%s error=%v", item.Status, runErr)
	}
	if want == RepairFailureSource {
		receipt := item.ExecutionReceipt
		if item.Status != TaskBlocked || receipt == nil || receipt.RepairProvenance == nil || receipt.RepairProvenance.FailureReason != want {
			t.Fatalf("permanent evidence failure was not durable: status=%s receipt=%+v error=%v", item.Status, receipt, runErr)
		}
		return
	}
	if !isInterruptedStatus(item.Status) {
		t.Fatalf("temporary evidence read lost continuation: status=%s error=%v", item.Status, runErr)
	}
	restore()
	snapshot, checkpoint := c.RunInputSnapshot(), normalizationResumeProjection(t, c)
	reader := reopenRecoveryCoordinator(t, c)
	reader.SetSessionData(checkpoint)
	if err := reader.selectRecoveryRunInputSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	forbidden := scriptWorker(failTurn(errors.New("unexpected redispatch")))
	reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
	count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	done := todoByID(t, reader, item.ID)
	marker, readErr := os.ReadFile(verifier)
	if err != nil || count != 1 || done.Status != TaskDone || forbidden.Calls() != 0 || readErr != nil || string(marker) != "verified\n" {
		t.Fatalf("normalization continuation did not settle once: status=%s count=%d work=%d verifier=%q error=%v read=%v", done.Status, count, forbidden.Calls(), marker, err, readErr)
	}
	body, err := os.ReadFile(filepath.Join(c.projectDir, "worker-output"))
	if err != nil || string(body) != "accepted work" {
		t.Fatalf("resumed apply lost the original isolated change: output=%q error=%v", body, err)
	}
}
