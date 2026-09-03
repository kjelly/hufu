package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
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

	scheduler := newDAGScheduler(c, tasks, items, nil)
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
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		maxConcurrent:   1,
		sessionTime:     time.Now(),
		maxWallClock:    20 * time.Millisecond,
		taskResultCache: make(map[string][]cachedTaskEntry),
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
	scheduler := newDAGScheduler(c, tasks, items, nil)
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
