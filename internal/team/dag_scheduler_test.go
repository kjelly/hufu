package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
)

func TestProviderSemaphore(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"local":  {MaxConcurrent: 1},
		"ollama": {MaxConcurrent: 2},
		"remote": {},
	})
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{
		providerManager: manager,
		session:         &TeamSession{Config: agent.TeamConfig{}},
	}

	sem := coord.providerSemaphore("local/model")
	if sem == nil {
		t.Fatal("expected a semaphore for a provider with max-concurrent configured")
	}
	if cap(sem) != 1 {
		t.Errorf("cap = %d, want 1", cap(sem))
	}
	for _, modelID := range []string{"ollama/model", "model"} {
		if got := coord.providerSemaphore(modelID); got != sem {
			t.Errorf("providerSemaphore(%q) returned a different channel", modelID)
		}
	}
	if coord.providerSemaphore("local/model") != sem {
		t.Error("providerSemaphore must return the same channel on repeated calls (shared limiter)")
	}
	if coord.providerSemaphore("remote/model") != nil {
		t.Error("expected nil semaphore for a provider with no max-concurrent configured")
	}
	if coord.providerSemaphore("") != nil {
		t.Error("expected nil semaphore for an empty provider name")
	}

	nilSessionCoord := &Coordinator{}
	if nilSessionCoord.providerSemaphore("ollama/model") != nil {
		t.Error("expected nil semaphore when the coordinator has no session")
	}
}

func TestAcquireSemNilChannelAlwaysAvailable(t *testing.T) {
	slot, err := acquireSem(context.Background(), nil)
	if err != nil {
		t.Fatalf("acquireSem(nil) error = %v", err)
	}
	slot.release() // must not block or panic on a nil channel
	slot.release() // and must be safe to call twice
}

func TestExecutionBackendSemaphoreUsesCanonicalBackendBucket(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM, caps: execution.BackendCapabilities{DirectLanguageModel: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{
		Providers:         map[string]config.ProviderConfig{"local": {MaxConcurrent: 1}},
		SubagentProviders: map[string]agent.SubagentProviderConfig{"codex": {MaxConcurrent: 4}},
	}}}
	c.SetExecutionRegistry(registry)

	local := execution.ExecutionTarget{Backend: "local", Model: "same-model"}
	codex := execution.ExecutionTarget{Backend: "codex", Model: "same-model"}
	localSem, err := c.executionBackendSemaphore(local)
	if err != nil {
		t.Fatalf("local policy: %v", err)
	}
	codexSem, err := c.executionBackendSemaphore(codex)
	if err != nil {
		t.Fatalf("codex policy: %v", err)
	}
	if localSem == codexSem {
		t.Fatal("unrelated canonical backends shared a concurrency bucket")
	}
	if cap(localSem) != 1 || cap(codexSem) != codexDefaultMaxConcurrent {
		t.Fatalf("backend capacities local=%d codex=%d, want 1 and %d", cap(localSem), cap(codexSem), codexDefaultMaxConcurrent)
	}
	if again, err := c.executionBackendSemaphore(codex); err != nil || again != codexSem {
		t.Fatalf("codex backend bucket was not stable: sem=%p err=%v", again, err)
	}
	policy, err := c.ResolveBackendExecutionPolicy(codex)
	if err != nil || policy.Backend != "codex" || policy.MaxConcurrent != codexDefaultMaxConcurrent {
		t.Fatalf("codex backend policy = %#v, err=%v, want hard cap %d", policy, err, codexDefaultMaxConcurrent)
	}
}

func TestCodexBackendDefaultsToSerializedConcurrency(t *testing.T) {
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeExecutionBackend{name: codexSubagentProviderName, kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{}}}
	c.SetExecutionRegistry(registry)

	target := execution.ExecutionTarget{Backend: codexSubagentProviderName, Model: "gpt-5-codex"}
	policy, err := c.ResolveBackendExecutionPolicy(target)
	if err != nil {
		t.Fatalf("ResolveBackendExecutionPolicy: %v", err)
	}
	if policy.MaxConcurrent != codexDefaultMaxConcurrent {
		t.Fatalf("Codex default policy = %#v, want max-concurrent=%d", policy, codexDefaultMaxConcurrent)
	}
	sem, err := c.executionBackendSemaphore(target)
	if err != nil {
		t.Fatalf("executionBackendSemaphore: %v", err)
	}
	if cap(sem) != codexDefaultMaxConcurrent {
		t.Fatalf("Codex semaphore capacity = %d, want %d", cap(sem), codexDefaultMaxConcurrent)
	}
}

func TestAcquireSemLimitsConcurrency(t *testing.T) {
	ch := make(chan struct{}, 1)

	slot1, err := acquireSem(context.Background(), ch)
	if err != nil {
		t.Fatalf("first acquireSem error = %v", err)
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := acquireSem(timeoutCtx, ch); err == nil {
		t.Error("expected acquireSem to block (and time out) while the channel is full")
	}

	slot1.release()

	slot2, err := acquireSem(context.Background(), ch)
	if err != nil {
		t.Fatalf("acquireSem after release error = %v", err)
	}
	slot2.release()
}

func TestProviderInvocationLimiterUsesFinalModelProvider(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"local":  {MaxConcurrent: 1},
		"remote": {MaxConcurrent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		providerManager: manager,
		session:         &TeamSession{Config: agent.TeamConfig{}},
	}
	limiter := c.providerAdmission().(agent.InvocationLimiter)

	localRelease, err := limiter.AcquireProviderInvocation(t.Context(), "local/initial")
	if err != nil {
		t.Fatalf("acquire initial local invocation: %v", err)
	}
	defer localRelease()

	fallbackAcquired := make(chan struct{})
	fallbackRelease := make(chan func())
	go func() {
		release, acquireErr := limiter.AcquireProviderInvocation(t.Context(), "local/fallback")
		if acquireErr != nil {
			return
		}
		fallbackAcquired <- struct{}{}
		fallbackRelease <- release
	}()

	select {
	case <-fallbackAcquired:
		t.Fatal("final-model fallback invocation bypassed the local provider limit")
	case <-time.After(25 * time.Millisecond):
	}

	localRelease()
	select {
	case <-fallbackAcquired:
	case <-time.After(time.Second):
		t.Fatal("final-model fallback invocation did not acquire after the old slot was released")
	}
	(<-fallbackRelease)()
}

func TestProviderInvocationLimiterIsSharedByExtraModelClones(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"local": {MaxConcurrent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := &Coordinator{
		providerManager: manager,
		session:         &TeamSession{Config: agent.TeamConfig{}},
	}
	clone := cloneCoordinator(parent, parent.session)
	parentLimiter := parent.providerAdmission().(agent.InvocationLimiter)
	cloneLimiter := clone.providerAdmission().(agent.InvocationLimiter)

	parentRelease, err := parentLimiter.AcquireProviderInvocation(t.Context(), "local/main")
	if err != nil {
		t.Fatalf("acquire parent invocation: %v", err)
	}
	defer parentRelease()

	cloneAcquired := make(chan struct{})
	cloneRelease := make(chan func())
	go func() {
		release, acquireErr := cloneLimiter.AcquireProviderInvocation(t.Context(), "ollama/extra")
		if acquireErr != nil {
			return
		}
		cloneAcquired <- struct{}{}
		cloneRelease <- release
	}()

	select {
	case <-cloneAcquired:
		t.Fatal("extra-model clone bypassed the shared local provider limit")
	case <-time.After(25 * time.Millisecond):
	}

	parentRelease()
	select {
	case <-cloneAcquired:
	case <-time.After(time.Second):
		t.Fatal("extra-model clone did not acquire after the parent released the provider slot")
	}
	(<-cloneRelease)()
}

func TestDAGSchedulerBudgetAdmissionSkipsQueuedWorkers(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "budget-admission"},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		taskTracker:   NewTaskTracker(),
		reportStatus:  func(StatusEvent) {},
		maxConcurrent: 1,
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "fanout child one"},
		{Agent: "worker", Desc: "fanout child two"},
	})
	tasks := []TaskDef{
		{Agent: "worker", Goal: "fanout child one"},
		{Agent: "worker", Goal: "fanout child two"},
	}
	var providerCalls int
	c.workerAgentOverride = &countingEmptyAgent{calls: &providerCalls}
	c.SetBudget(0, 1)
	c.tokensUsed.Store(1)

	scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)
	results, err := scheduler.run(context.Background())
	if err != nil {
		t.Fatalf("budgeted DAG run: %v", err)
	}
	if providerCalls != 0 {
		t.Fatalf("queued budget-exhausted workers invoked provider %d time(s)", providerCalls)
	}
	if len(results) != len(tasks) {
		t.Fatalf("results = %d, want %d", len(results), len(tasks))
	}
	for i, result := range results {
		if !isBudgetAdmissionError(result.err) {
			t.Fatalf("result[%d] error = %v, want budget admission error", i, result.err)
		}
		if items[i].Status != TaskError {
			t.Fatalf("queued item[%d] status = %s, want terminal error", i, items[i].Status)
		}
		if !strings.Contains(items[i].Detail, "source=budget_exceeded") {
			t.Fatalf("queued item[%d] detail = %q, want budget source", i, items[i].Detail)
		}
	}
}

func TestDAGSchedulerLaunchesOnlySlotOwnersInInputOrder(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "deterministic-serial-dispatch"},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		taskTracker:   NewTaskTracker(),
		reportStatus:  func(StatusEvent) {},
		maxConcurrent: 1,
		taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "serial child one"},
		{Agent: "worker", Desc: "serial child two"},
		{Agent: "worker", Desc: "serial child three"},
	})
	tasks := []TaskDef{
		{Agent: "worker", Goal: "serial child one"},
		{Agent: "worker", Goal: "serial child two"},
		{Agent: "worker", Goal: "serial child three"},
	}
	var providerCalls int
	var heldID string
	holding := &budgetHoldingAgent{started: make(chan struct{}), release: make(chan struct{}), calls: &providerCalls, heldID: &heldID}
	c.workerAgentOverride = holding
	scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)

	scheduler.launchReady(t.Context())
	select {
	case <-holding.started:
	case <-time.After(time.Second):
		t.Fatal("first task did not start")
	}
	if heldID != items[0].ID {
		t.Fatalf("first dispatched task = %q, want input-order task %q", heldID, items[0].ID)
	}
	if scheduler.inProgress != 1 || scheduler.states[0] != TaskInProgress || scheduler.states[1] != TaskPending || scheduler.states[2] != TaskPending {
		t.Fatalf("scheduler states = %v in_progress=%d, want [in_progress pending pending] / 1", scheduler.states, scheduler.inProgress)
	}
	if items[1].Status != TaskPending || items[2].Status != TaskPending {
		t.Fatalf("queued durable statuses = [%s %s], want pending/pending", items[1].Status, items[2].Status)
	}
	if len(scheduler.sem) != 1 {
		t.Fatalf("reserved team slots = %d, want 1", len(scheduler.sem))
	}

	close(holding.release)
	select {
	case <-scheduler.eventCh:
	case <-time.After(time.Second):
		t.Fatal("launched task did not stop")
	}
}

func TestDAGSchedulerCancellationDoesNotLaunchDependentFromBufferedEvent(t *testing.T) {
	for range 100 {
		c := &Coordinator{
			session: &TeamSession{
				Workspace: t.TempDir(),
				Config:    agent.TeamConfig{Name: "cancel-buffered-event"},
				Agents: map[string]*agent.AgentDef{
					"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
				},
			},
			taskTracker:   NewTaskTracker(),
			reportStatus:  func(StatusEvent) {},
			maxConcurrent: 1,
			taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
		}
		items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
			{Agent: "worker", Desc: "completed root"},
			{Agent: "worker", Desc: "dependent must not start"},
		})
		tasks := []TaskDef{
			{Agent: "worker", Goal: "completed root"},
			{Agent: "worker", Goal: "dependent must not start", DependsOn: []int{0}},
		}
		var calls int
		c.workerAgentOverride = &countingEmptyAgent{calls: &calls}
		scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)
		scheduler.states[0] = TaskInProgress
		scheduler.inProgress = 1
		scheduler.eventCh <- agentTaskResult{agentName: "worker", todoID: items[0].ID, idx: 0}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := scheduler.run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled scheduler error = %v, want context.Canceled", err)
		}
		if calls != 0 {
			t.Fatalf("cancelled scheduler launched dependent worker %d time(s)", calls)
		}
		if scheduler.inProgress != 0 || scheduler.states[1] != TaskPending {
			t.Fatalf("cancelled scheduler state = states=%v in_progress=%d, want [in_progress pending] / 0", scheduler.states, scheduler.inProgress)
		}
	}
}

func TestDAGSchedulerPreCancelledContextStopsBeforeStranding(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "pre-cancelled-scheduler"},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		taskTracker:   NewTaskTracker(),
		reportStatus:  func(StatusEvent) {},
		maxConcurrent: 1,
		taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "must not start"}})
	tasks := []TaskDef{{Agent: "worker", Goal: "must not start"}}
	var calls int
	c.workerAgentOverride = &countingEmptyAgent{calls: &calls}
	scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	results, err := scheduler.run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled scheduler error = %v, want context.Canceled", err)
	}
	if results != nil {
		t.Fatalf("pre-cancelled scheduler results = %#v, want nil", results)
	}
	if calls != 0 {
		t.Fatalf("pre-cancelled scheduler invoked worker %d time(s)", calls)
	}
	if scheduler.states[0] != TaskPending || items[0].Status != TaskPending {
		t.Fatalf("pre-cancelled scheduler stranded task: state=%s item=%s", scheduler.states[0], items[0].Status)
	}
}

type budgetHoldingAgent struct {
	started chan struct{}
	release chan struct{}
	calls   *int
	heldID  *string
}

func (a *budgetHoldingAgent) run(ctx context.Context) (*fantasy.AgentResult, error) {
	*a.calls++
	if a.heldID != nil && *a.heldID == "" {
		*a.heldID, _ = ctx.Value(todoIDKey{}).(string)
	}
	select {
	case <-a.started:
	default:
		close(a.started)
	}
	select {
	case <-a.release:
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "completed"}}}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *budgetHoldingAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a *budgetHoldingAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func TestDAGSchedulerBudgetExpiryWhileQueuedTaskWaitsForPermit(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "budget-queue-expiry"},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		taskTracker:   NewTaskTracker(),
		reportStatus:  func(StatusEvent) {},
		maxConcurrent: 1,
		sessionTime:   time.Now(),
		budgetLedger:  budgetLedger{maxWallClock: 20 * time.Millisecond},
		taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "holds permit"},
		{Agent: "worker", Desc: "waits for permit"},
	})
	tasks := []TaskDef{
		{Agent: "worker", Goal: "holds permit"},
		{Agent: "worker", Goal: "waits for permit"},
	}
	var providerCalls int
	var heldID string
	holding := &budgetHoldingAgent{started: make(chan struct{}), release: make(chan struct{}), calls: &providerCalls, heldID: &heldID}
	c.workerAgentOverride = holding
	scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)
	resultsCh := make(chan []agentTaskResult, 1)
	go func() {
		results, _ := scheduler.run(context.Background())
		resultsCh <- results
	}()
	select {
	case <-holding.started:
	case <-time.After(time.Second):
		t.Fatal("first task did not acquire the permit")
	}
	time.Sleep(50 * time.Millisecond)
	if exceeded, _ := c.budgetExceeded(); !exceeded {
		t.Fatal("run budget did not expire while first task held the permit")
	}
	close(holding.release)
	select {
	case results := <-resultsCh:
		if len(results) != 2 {
			t.Fatalf("results = %d, want 2", len(results))
		}
		if providerCalls != 1 {
			t.Fatalf("provider calls = %d, want only the permit holder", providerCalls)
		}
		queued := -1
		for i, result := range results {
			if isBudgetAdmissionError(result.err) {
				if queued != -1 {
					t.Fatal("more than one task was classified as queued budget admission")
				}
				queued = i
			}
		}
		if queued == -1 {
			t.Fatalf("no queued result received budget admission error: %#v", results)
		}
		if items[queued].ID == heldID {
			t.Fatal("permit holder was classified as the queued task")
		}
		if items[queued].Status != TaskError || strings.TrimSpace(items[queued].Detail) == "" {
			t.Fatalf("queued task status/detail = %s / %q, want terminal truthful error", items[queued].Status, items[queued].Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler did not drain after permit release")
	}
}
