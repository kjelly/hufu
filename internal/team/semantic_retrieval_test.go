package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
)

func TestWorkerMemorySemanticOptionsPreserveOffShadowAndLegacyContracts(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "worker"}
	wp3AppendItems(t, repo, contextstore.ContextItem{
		ID: "memory-1", Kind: contextstore.ContextPattern, Content: "semantic deployment guidance",
		Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1,
	})
	stored, err := repo.GetMany(t.Context(), []string{"memory-1"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("load stored item: %v %#v", err, stored)
	}
	vector := &workerSemanticVector{
		results: []contextstore.SearchResult{{Item: stored[0], Score: .9}},
		model: embedding.ModelIdentity{
			ID: "tiny-zh", Revision: "r1", ManifestSHA256: strings.Repeat("a", 64), Dimensions: 2,
		},
		generationID: "generation-1", sourceRevision: 7,
	}
	request := WorkerMemoryRecallRequest{
		WorkerID: "worker", Scope: scope, Query: "semantic deployment guidance",
		Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 5, MaxTokens: 1000},
	}

	off, err := NewWorkerMemoryService(repo, nil).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if off.Semantic != nil {
		t.Fatalf("legacy nil searcher produced semantic identity: %#v", off.Semantic)
	}

	hasher, err := contextstore.NewHMACTraceHasher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	var traces []contextstore.SemanticRetrievalTrace
	shadow, err := NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{
		Semantic: vector, Mode: contextstore.RetrievalShadow, TraceHasher: hasher,
		TraceSink: func(trace contextstore.SemanticRetrievalTrace) { traces = append(traces, trace) },
	}).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if shadow.Semantic != nil {
		t.Fatalf("shadow retrieval entered the official manifest identity: %#v", shadow.Semantic)
	}
	if !reflect.DeepEqual(workerMemoryIDs(shadow.Items), workerMemoryIDs(off.Items)) {
		t.Fatalf("shadow IDs = %v, off IDs = %v", workerMemoryIDs(shadow.Items), workerMemoryIDs(off.Items))
	}
	if len(traces) != 1 || traces[0].Mode != contextstore.RetrievalShadow {
		t.Fatalf("shadow traces = %#v", traces)
	}

	active, err := NewWorkerMemoryService(repo, vector).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if active.Semantic == nil || active.Semantic.Mode != contextstore.RetrievalActive ||
		active.Semantic.ModelID != "tiny-zh" || active.Semantic.GenerationID != "generation-1" ||
		active.Semantic.SourceRevision != 7 || active.Semantic.FallbackReason != contextstore.SemanticFallbackNone ||
		active.Semantic.RetrievalPolicyVersion != SemanticRetrievalPolicyVersion {
		t.Fatalf("active semantic identity = %#v", active.Semantic)
	}
}

func TestSemanticIdentityConditionallyChangesManifestFingerprints(t *testing.T) {
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	policy.PolicyVersion = "memory-policy-v1"
	compiled := CompiledContext{IncludedItems: []ContextItem{{
		ID: "context:memory-1", Kind: "worker_memory", Source: "worker_session",
		TokenCount: 4, DedupKey: "content-hash", BaseScore: .4, FinalScore: .8,
	}}}

	legacy := buildMemoryInjectionManifest(compiled, "run", "task", 1, "worker", "query", policy)
	orderedBinding := "memory-1\x1econtent-hash"
	legacyInput := strings.Join([]string{"run", "task", "worker", policy.PolicyVersion, orderedBinding}, "\x1f")
	wantSum := sha256.Sum256([]byte(legacyInput))
	wantFingerprint := hex.EncodeToString(wantSum[:])
	if legacy == nil || legacy.Semantic != nil || legacy.Fingerprint != wantFingerprint {
		t.Fatalf("legacy fingerprint = %#v, want %s", legacy, wantFingerprint)
	}

	identity := semanticIdentityFixture()
	general := &ContextInjectionManifest{Semantic: identity, Items: []ContextManifestItem{{ID: "memory-1", Included: true}}}
	first := buildMemoryInjectionManifestFromContextManifest(compiled, general, "run", "task", 1, "worker", "query", policy)
	second := buildMemoryInjectionManifestFromContextManifest(compiled, general, "run", "task", 1, "worker", "query", policy)
	if first == nil || first.Semantic == nil || first.Fingerprint == legacy.Fingerprint || first.Fingerprint != second.Fingerprint {
		t.Fatalf("semantic fingerprints: legacy=%q first=%#v second=%#v", legacy.Fingerprint, first, second)
	}
	changedIdentity := *identity
	changedIdentity.GenerationID = "generation-2"
	general.Semantic = &changedIdentity
	changedGeneration := buildMemoryInjectionManifestFromContextManifest(compiled, general, "run", "task", 1, "worker", "query", policy)
	if changedGeneration.Fingerprint == first.Fingerprint {
		t.Fatal("generation identity did not affect memory manifest fingerprint")
	}
	changedContent := compiled
	changedContent.IncludedItems = append([]ContextItem(nil), compiled.IncludedItems...)
	changedContent.IncludedItems[0].DedupKey = "changed-content-hash"
	general.Semantic = identity
	changedBinding := buildMemoryInjectionManifestFromContextManifest(changedContent, general, "run", "task", 1, "worker", "query", policy)
	if changedBinding.Fingerprint == first.Fingerprint {
		t.Fatal("selected semantic item content hash did not affect manifest fingerprint")
	}
}

func TestLegacyManifestJSONRemainsByteIdenticalWithoutSemanticIdentity(t *testing.T) {
	fixtures := []struct {
		name string
		raw  string
		new  func() any
	}{
		{
			name: "memory",
			raw:  `{"retrieval_id":"retrieval-1","run_id":"run","task_id":"task","attempt":1,"agent":"worker","policy_version":"v1","items":[],"fingerprint":"fp","created_at":"2025-01-01T00:00:00Z"}`,
			new:  func() any { return &MemoryInjectionManifest{} },
		},
		{
			name: "context",
			raw:  `{"schema_version":1,"request_id":"request","request_hash":"hash","run_id":"run","attempt":1,"agent":"worker","phase":"EXECUTE","trigger":"task_dispatch","model_called":true,"items":[],"fingerprint":"fp","created_at":"2025-01-01T00:00:00Z"}`,
			new:  func() any { return &ContextInjectionManifest{} },
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			value := fixture.new()
			if err := json.Unmarshal([]byte(fixture.raw), value); err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != fixture.raw {
				t.Fatalf("legacy JSON changed\n got: %s\nwant: %s", got, fixture.raw)
			}
		})
	}
}

func TestSemanticIdentityPropagatesThroughCloneCheckpointEventAndReceipt(t *testing.T) {
	identity := semanticIdentityFixture()
	contextManifest := ContextInjectionManifest{RequestID: "request", Semantic: identity}
	memoryManifest := MemoryInjectionManifest{RetrievalID: "retrieval", Semantic: identity}
	receipt := ExecutionReceipt{
		RunID: "run", TaskID: "task", Attempt: 1, Semantic: identity,
		ContextManifest: &contextManifest, MemoryManifest: &memoryManifest,
	}
	item := &TodoItem{
		ID: "task", Agent: "worker", Status: TaskDone,
		ContextManifests: []ContextInjectionManifest{contextManifest},
		MemoryManifests:  []MemoryInjectionManifest{memoryManifest},
		ExecutionReceipt: &receipt, ExecutionReceipts: []ExecutionReceipt{receipt},
	}

	clone := cloneTodoItem(item)
	clone.ExecutionReceipt.Semantic.GenerationID = "mutated"
	clone.ContextManifests[0].Semantic.GenerationID = "mutated"
	clone.MemoryManifests[0].Semantic.GenerationID = "mutated"
	if got := SemanticRetrievalIdentityForTask(item); got == nil || got.GenerationID != identity.GenerationID {
		t.Fatalf("clone mutated canonical identity: %#v", got)
	}

	workspace := t.TempDir()
	session := NewSession()
	session.Tasks = []*TodoItem{item}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(workspace)
	if loaded == nil || len(loaded.Tasks) != 1 {
		t.Fatalf("loaded checkpoint = %#v", loaded)
	}
	if got := SemanticRetrievalIdentityForTask(loaded.Tasks[0]); !reflect.DeepEqual(got, identity) {
		t.Fatalf("checkpoint identity = %#v, want %#v", got, identity)
	}

	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	reduced := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: item.ID, Payload: payload}})
	if len(reduced) != 1 || !reflect.DeepEqual(SemanticRetrievalIdentityForTask(reduced[0]), identity) {
		t.Fatalf("event-reduced identity = %#v", reduced)
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(receiptJSON), `"semantic_retrieval"`) {
		t.Fatalf("receipt JSON omitted semantic identity: %s", receiptJSON)
	}

	vector := &workerSemanticVector{}
	coordinator := &Coordinator{taskTracker: NewTaskTracker()}
	coordinator.taskTracker.TodoList().Restore(loaded.Tasks)
	coordinator.workerMemorySvc = NewWorkerMemoryServiceWithOptions(nil, WorkerMemoryOptions{Semantic: vector, Mode: contextstore.RetrievalActive})
	resumed, err := coordinator.ResumeInterruptedTasks(t.Context())
	if err != nil || resumed != 0 || vector.calls != 0 {
		t.Fatalf("completed resume = %d/%v, semantic calls=%d", resumed, err, vector.calls)
	}
}

func TestExecutionReceiptSemanticIdentityFailsClosedOnManifestMismatch(t *testing.T) {
	identity := semanticIdentityFixture()
	mismatch := *identity
	mismatch.GenerationID = "other-generation"
	receipt := &ExecutionReceipt{
		RunID: "run", TaskID: "task", Attempt: 1, Semantic: identity,
		ContextManifest: &ContextInjectionManifest{Semantic: &mismatch},
	}
	if _, err := json.Marshal(receipt); err == nil {
		t.Fatal("receipt JSON accepted mismatched semantic identity")
	}
	list := NewTaskTracker().TodoList()
	list.AddBatch([]TodoSpec{{Agent: "worker", Desc: "semantic task"}})
	receipt.TaskID = "1"
	if err := list.SetExecutionReceipt("1", receipt); err == nil {
		t.Fatal("receipt persistence accepted mismatched semantic identity")
	}

	legacy := &ExecutionReceipt{
		RunID: "run", TaskID: "1", Attempt: 1,
		ContextManifest: &ContextInjectionManifest{Semantic: identity},
	}
	if err := list.SetExecutionReceipt("1", legacy); err != nil {
		t.Fatal(err)
	}
	stored := list.Items()[0].ExecutionReceipt
	if stored == nil || !reflect.DeepEqual(stored.Semantic, identity) {
		t.Fatalf("legacy nested identity was not normalized: %#v", stored)
	}
}

func TestCompileAndManifestPropagationKeepsShadowNilAndActiveConsistent(t *testing.T) {
	item := WorkerMemoryItem{ContextItem: contextstore.ContextItem{
		ID: "memory-1", Content: "prior semantic memory", ContentHash: "content-hash",
	}, Tier: "session", BaseScore: .7, FinalScore: .8}
	compile := func(identity *SemanticRetrievalIdentity) CompiledContext {
		compiled, err := CompileWorkerContext(t.Context(), WorkerContextInput{
			Goal: "perform task", WorkerMemory: &WorkerMemoryBundle{Items: []WorkerMemoryItem{item}, Semantic: identity},
			ModelContext: ModelContextSpec{ModelID: "test", ContextWindow: 4000, MaxOutputTokens: 500, SafetyMarginTokens: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		return compiled
	}
	request := validTestContextRequest()
	activeCompiled := compile(semanticIdentityFixture())
	activeManifest, err := BuildContextInjectionManifest(request, activeCompiled, nil, "worker", time.Unix(100, 0), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if activeManifest.Semantic == nil || !reflect.DeepEqual(activeManifest.Semantic, activeCompiled.Semantic) {
		t.Fatalf("active context identity = %#v, compiled = %#v", activeManifest.Semantic, activeCompiled.Semantic)
	}
	shadowCompiled := compile(nil)
	shadowManifest, err := BuildContextInjectionManifest(request, shadowCompiled, nil, "worker", time.Unix(100, 0), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if shadowManifest.Semantic != nil {
		t.Fatalf("shadow/off manifest gained semantic identity: %#v", shadowManifest.Semantic)
	}
}

func TestSemanticReceiptPropagationMatchesDirectAndDAGWorkerPaths(t *testing.T) {
	t.Run("direct", func(t *testing.T) {
		coordinator := newDirectTerminationCoordinator(t, directTerminationAgent{})
		policy := agent.DefaultMemoryLearningPolicy()
		policy.Mode = agent.MemoryLearningObserve
		policy.PolicyVersion = "memory-policy-v1"
		coordinator.session.Config.MemoryLearning = policy
		pool := coordinator.agentPool.(*mockAgentPool)
		pool.resolveDef.MemoryID = "worker"
		pool.resolveDef.Memory = agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 5, MaxTokens: 1000}

		repo, vector := configureSemanticWorkerMemory(t, coordinator, pool.resolveDef)
		t.Cleanup(func() { _ = repo.Close() })
		if _, err := coordinator.RunDirectAgent(t.Context(), "worker", "semantic deployment guidance"); err != nil {
			t.Fatal(err)
		}
		items := coordinator.taskTracker.TodoList().Items()
		if len(items) != 1 {
			t.Fatalf("direct tasks = %#v", items)
		}
		assertSemanticReceiptParity(t, items[0], vector.generationID)
	})

	t.Run("dag", func(t *testing.T) {
		workspace := t.TempDir()
		policy := agent.DefaultMemoryLearningPolicy()
		policy.Mode = agent.MemoryLearningObserve
		policy.PolicyVersion = "memory-policy-v1"
		definition := &agent.AgentDef{
			Name: "worker", Role: "worker", MemoryID: "worker", Generation: agent.GenerationParams{Model: "test"},
			Memory: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 5, MaxTokens: 1000},
		}
		coordinator := &Coordinator{
			session: &TeamSession{
				Workspace: workspace, Config: agent.TeamConfig{Name: "team", MemoryLearning: policy},
				Agents: map[string]*agent.AgentDef{"worker": definition},
			},
			sessionData: NewSession(), sessionTime: time.Now(), taskTracker: NewTaskTracker(),
			reportStatus: func(StatusEvent) {}, taskCache: newDefaultTaskCache(taskCacheDependencies{}),
			executionRunID: "run-semantic-dag", projectDir: workspace,
		}
		repo, vector := configureSemanticWorkerMemory(t, coordinator, definition)
		t.Cleanup(func() {
			time.Sleep(100 * time.Millisecond)
			_ = repo.Close()
		})
		item := coordinator.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "semantic deployment guidance"}})[0]
		coordinator.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
			coordinator.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: TaskResultStatusCompletedWithGaps,
				Summary: "completed semantic task", Source: "submitted",
			})
		}}
		if _, err := coordinator.executeTask(t.Context(), TaskDef{Agent: "worker", Goal: "semantic deployment guidance", Recovery: RecoveryRetry}, item.ID); err != nil {
			t.Fatal(err)
		}
		items := coordinator.taskTracker.TodoList().Items()
		if len(items) != 1 {
			t.Fatalf("DAG tasks = %#v", items)
		}
		assertSemanticReceiptParity(t, items[0], vector.generationID)
	})
}

func configureSemanticWorkerMemory(
	t *testing.T,
	coordinator *Coordinator,
	definition *agent.AgentDef,
) (*contextstore.SQLiteRepository, *workerSemanticVector) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(coordinator.session.Workspace + "/context.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	scope := resolveWorkerScope(coordinator.contextScope(), definition, "main")
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "semantic-memory", Kind: contextstore.ContextPattern, Content: "semantic deployment guidance",
		Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1,
	}); err != nil {
		_ = repo.Close()
		t.Fatal(err)
	}
	stored, err := repo.GetMany(t.Context(), []string{"semantic-memory"})
	if err != nil || len(stored) != 1 {
		_ = repo.Close()
		t.Fatalf("load semantic memory: %v %#v", err, stored)
	}
	vector := &workerSemanticVector{
		results: []contextstore.SearchResult{{Item: stored[0], Score: .95}},
		model: embedding.ModelIdentity{
			ID: "tiny-zh", Revision: "r1", ManifestSHA256: strings.Repeat("a", 64), Dimensions: 2,
		},
		generationID: "generation-path-parity", sourceRevision: 9,
	}
	coordinator.contextRepo = repo
	coordinator.workerMemorySvc = NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{
		Semantic: vector, Mode: contextstore.RetrievalActive,
	})
	return repo, vector
}

func assertSemanticReceiptParity(t *testing.T, item *TodoItem, generationID string) {
	t.Helper()
	if item == nil || item.ExecutionReceipt == nil {
		t.Fatalf("missing execution receipt: %#v", item)
	}
	receipt := item.ExecutionReceipt
	if receipt.Semantic == nil || receipt.ContextManifest == nil || receipt.MemoryManifest == nil {
		t.Fatalf("incomplete semantic receipt: %#v", receipt)
	}
	if receipt.Semantic.GenerationID != generationID ||
		!reflect.DeepEqual(receipt.Semantic, receipt.ContextManifest.Semantic) ||
		!reflect.DeepEqual(receipt.Semantic, receipt.MemoryManifest.Semantic) {
		t.Fatalf("semantic receipt/manifest mismatch: %#v", receipt)
	}
}

type workerSemanticVector struct {
	results        []contextstore.SearchResult
	model          embedding.ModelIdentity
	generationID   string
	sourceRevision int64
	calls          int
}

func (v *workerSemanticVector) SearchVector(_ context.Context, _ contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	v.calls++
	return append([]contextstore.SearchResult(nil), v.results...), nil
}

func (v *workerSemanticVector) SemanticTraceIdentity() (embedding.ModelIdentity, string, int64) {
	return v.model, v.generationID, v.sourceRevision
}

func workerMemoryIDs(items []WorkerMemoryItem) []string {
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids
}

func semanticIdentityFixture() *SemanticRetrievalIdentity {
	return &SemanticRetrievalIdentity{
		Mode: contextstore.RetrievalActive, ModelID: "tiny-zh", ModelRevision: "r1",
		ModelHash: strings.Repeat("a", 64), GenerationID: "generation-1", SourceRevision: 7,
		RetrievalPolicyVersion: SemanticRetrievalPolicyVersion, FallbackReason: contextstore.SemanticFallbackNone,
	}
}
