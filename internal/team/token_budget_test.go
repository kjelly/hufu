package team

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestTokenBudgetChargesRawUsageOnceAcrossAttempts(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 100)

	first, err := c.reserveTokenStep(40)
	if err != nil {
		t.Fatal(err)
	}
	c.commitTokenStep(&first, 37)
	second, err := c.reserveTokenStep(40)
	if err != nil {
		t.Fatal(err)
	}
	c.commitTokenStep(&second, 23)

	if got := c.TokensUsed(); got != 60 {
		t.Fatalf("raw usage charged %d, want 60", got)
	}
	if exceeded, _ := c.budgetExceeded(); exceeded {
		t.Fatal("budget exceeded before the reserved capacity was consumed")
	}
}

func TestTokenBudgetConcurrentStepReservationsBoundOvershoot(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 80)
	const workers = 4
	start := make(chan struct{})
	var wg sync.WaitGroup
	reservations := make(chan tokenStepReservation, workers)
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, err := c.reserveTokenStep(20)
			if err != nil {
				errs <- err
				return
			}
			reservations <- reservation
		}()
	}
	close(start)
	wg.Wait()
	close(reservations)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := len(reservations); got != workers {
		t.Fatalf("admitted %d concurrent steps, want %d", got, workers)
	}
	for reservation := range reservations {
		// The reservation is deliberately smaller than the reported step usage;
		// the only possible overshoot is the bounded in-flight delta.
		c.commitTokenStep(&reservation, 30)
	}
	if got := c.TokensUsed(); got != workers*30 {
		t.Fatalf("charged usage %d, want %d", got, workers*30)
	}
	if exceeded, reason := c.budgetExceeded(); !exceeded || reason == "" {
		t.Fatalf("budget status = (%v, %q), want exhausted", exceeded, reason)
	}
	if _, err := c.reserveTokenStep(1); err == nil {
		t.Fatal("admitted a new step after the hard token cap")
	}
}

func TestTokenBudgetWorksetCapacityAndLowBudgetFailure(t *testing.T) {
	const items = 52
	const perItem = int64(30)

	sufficient := &Coordinator{}
	sufficient.SetBudget(0, items*perItem)
	for i := 0; i < items; i++ {
		reservation, err := sufficient.reserveTokenStep(perItem)
		if err != nil {
			t.Fatalf("item %d admission failed under sufficient budget: %v", i, err)
		}
		sufficient.commitTokenStep(&reservation, perItem)
	}
	if got := sufficient.TokensUsed(); got != items*perItem {
		t.Fatalf("workset usage = %d, want %d", got, items*perItem)
	}

	low := &Coordinator{}
	low.SetBudget(0, items*perItem-1)
	for i := 0; i < items-1; i++ {
		reservation, err := low.reserveTokenStep(perItem)
		if err != nil {
			t.Fatalf("item %d unexpectedly failed under low-budget fixture: %v", i, err)
		}
		low.commitTokenStep(&reservation, perItem)
	}
	if _, err := low.reserveTokenStep(perItem); err == nil {
		t.Fatal("low-budget workset admitted its final item")
	}
}

type tokenBudgetStreamingAgent struct{}

type tokenBudgetTestModel struct{ fantasy.LanguageModel }

func (tokenBudgetTestModel) Model() string    { return "test-model" }
func (tokenBudgetTestModel) Provider() string { return "test-provider" }

type tokenBudgetLifecycleAgent struct {
	mode  string
	calls *int
}

func (a tokenBudgetLifecycleAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a tokenBudgetLifecycleAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	(*a.calls)++
	prepare := func() error {
		if call.PrepareStep == nil {
			return nil
		}
		_, _, err := call.PrepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Model: tokenBudgetTestModel{}, Messages: call.Messages,
		})
		return err
	}
	step := func(total int64) fantasy.StepResult {
		return fantasy.StepResult{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: total}}}
	}
	switch a.mode {
	case "error":
		if err := prepare(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	case "overflow":
		if err := prepare(); err != nil {
			return nil, err
		}
		if *a.calls == 1 {
			return nil, fmt.Errorf("context length exceeded")
		}
		usage := fantasy.Usage{TotalTokens: 11}
		if err := call.OnStreamFinish(usage, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}, Steps: []fantasy.StepResult{step(11)}}, nil
	case "mixed":
		if err := prepare(); err != nil {
			return nil, err
		}
		if err := prepare(); err != nil {
			return nil, err
		}
		if err := call.OnStreamFinish(fantasy.Usage{TotalTokens: 7}, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}, Steps: []fantasy.StepResult{step(13), step(7)}}, nil
	case "duplicate":
		if err := prepare(); err != nil {
			return nil, err
		}
		usage := fantasy.Usage{TotalTokens: 5}
		if err := call.OnStreamFinish(usage, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
		if err := call.OnStreamFinish(usage, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}, Steps: []fantasy.StepResult{step(5)}}, nil
	default:
		if err := prepare(); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}, Steps: []fantasy.StepResult{step(13)}}, nil
	}
}

func newTokenBudgetRunCoordinator(t *testing.T, budget int64) *Coordinator {
	t.Helper()
	coordinator := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "usage-accounting"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	coordinator.SetBudget(0, budget)
	return coordinator
}

func TestRunAgentNoFinishUsesResultFallbackOnce(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	calls := 0
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetLifecycleAgent{mode: "no-finish", calls: &calls}, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	if got := c.TokensUsed(); got != 13 {
		t.Fatalf("no-finish fallback charged %d tokens, want 13", got)
	}
}

type tokenBudgetResultOnlyMultiStepAgent struct{}

func (tokenBudgetResultOnlyMultiStepAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (tokenBudgetResultOnlyMultiStepAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}},
		Steps: []fantasy.StepResult{
			{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: 4}}},
			{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: 6}}},
		},
	}, nil
}

func TestRunAgentResultOnlyMultiStepSettlesEachStepOnce(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetResultOnlyMultiStepAgent{}, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	if got := c.TokensUsed(); got != 10 {
		t.Fatalf("result-only multi-step usage = %d, want 10", got)
	}
	if c.tokenReservations != 0 {
		t.Fatalf("result-only multi-step left %d reserved tokens", c.tokenReservations)
	}
}

func TestRunAgentDuplicateFinishDoesNotDoubleCharge(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	calls := 0
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetLifecycleAgent{mode: "duplicate", calls: &calls}, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	if got := c.TokensUsed(); got != 5 {
		t.Fatalf("duplicate finish charged %d tokens, want 5", got)
	}
}

func TestRunAgentMixedFinishAndFallbackStreamsSettleEachStep(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	calls := 0
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetLifecycleAgent{mode: "mixed", calls: &calls}, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	if got := c.TokensUsed(); got != 20 {
		t.Fatalf("mixed finish/fallback charged %d tokens, want 20", got)
	}
}

func TestRunAgentProviderErrorReleasesOutstandingAdmission(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	calls := 0
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetLifecycleAgent{mode: "error", calls: &calls}, "worker", "prompt", nil, &taskTiming{}); err == nil {
		t.Fatal("provider error unexpectedly succeeded")
	}
	if c.tokenReservations != 0 {
		t.Fatalf("provider error left %d reserved tokens", c.tokenReservations)
	}
}

func TestRunAgentContextOverflowDoesNotReplayAndReleasesReservation(t *testing.T) {
	c := newTokenBudgetRunCoordinator(t, 10000)
	calls := 0
	if _, _, err := c.runAgentWithStatusAndHistory(context.Background(), tokenBudgetLifecycleAgent{mode: "overflow", calls: &calls}, "worker", "prompt", nil, &taskTiming{}); err == nil || !strings.Contains(err.Error(), "context length exceeded") {
		t.Fatalf("context overflow error = %v, want provider overflow", err)
	}
	if calls != 1 {
		t.Fatalf("context overflow calls = %d, want 1 without replay", calls)
	}
	if got := c.TokensUsed(); got != 0 {
		t.Fatalf("context overflow charged %d tokens without provider usage, want 0", got)
	}
	if c.tokenReservations != 0 {
		t.Fatalf("context overflow left %d reserved tokens", c.tokenReservations)
	}
}

func (tokenBudgetStreamingAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (tokenBudgetStreamingAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		if _, _, err := call.PrepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{Model: tokenBudgetTestModel{}, Messages: call.Messages}); err != nil {
			return nil, err
		}
	}
	usage := fantasy.Usage{TotalTokens: 17}
	if call.OnStreamFinish != nil {
		if err := call.OnStreamFinish(usage, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
	}
	step := fantasy.StepResult{Response: fantasy.Response{Usage: usage}}
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}, Usage: usage},
		Steps:    []fantasy.StepResult{step},
	}, nil
}

func TestTokenBudgetSettlementIsIdempotent(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 100)
	reservation, err := c.reserveTokenStep(40)
	if err != nil {
		t.Fatal(err)
	}
	if !c.commitTokenStep(&reservation, 37) {
		t.Fatal("first settlement was not accepted")
	}
	if c.commitTokenStep(&reservation, 37) {
		t.Fatal("duplicate settlement was accepted")
	}
	if got := c.TokensUsed(); got != 37 {
		t.Fatalf("duplicate settlement charged %d tokens, want 37", got)
	}
	if c.tokenReservations != 0 {
		t.Fatalf("reservation count = %d, want 0", c.tokenReservations)
	}
}

func TestTokenBudgetProviderErrorReleasesReservation(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 100)
	reservation, err := c.reserveTokenStep(80)
	if err != nil {
		t.Fatal(err)
	}
	c.releaseTokenStep(&reservation)
	if c.TokensUsed() != 0 {
		t.Fatalf("provider error release charged %d tokens", c.TokensUsed())
	}
	if _, err := c.reserveTokenStep(100); err != nil {
		t.Fatalf("released reservation stranded capacity: %v", err)
	}
}

func TestTokenBudgetFallbackSettlementAndAdmissionAreAtomic(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 100)
	c.tokensUsed.Store(95)
	reservation, err := c.reserveTokenStep(5)
	if err != nil {
		t.Fatal(err)
	}
	// The result fallback must settle the reservation while holding the same
	// ledger lock that removes its admission. Observed usage is charged even
	// when it takes the ledger beyond the cap; the next admission is refused.
	if !c.commitTokenStep(&reservation, 10) {
		t.Fatal("fallback settlement was not accepted")
	}
	if got := c.TokensUsed(); got != 105 {
		t.Fatalf("fallback settlement usage = %d, want 105", got)
	}
	if c.tokenReservations != 0 {
		t.Fatalf("fallback settlement left %d reserved tokens", c.tokenReservations)
	}
	if _, err := c.reserveTokenStep(1); err == nil {
		t.Fatal("admitted capacity after fallback usage exhausted the cap")
	}
}

type tokenBudgetObservedUsageAgent struct {
	prepare       bool
	finish        bool
	includeStep   bool
	responseUsage bool
	totalUsage    bool
	usage         int64
}

func (a tokenBudgetObservedUsageAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a tokenBudgetObservedUsageAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if a.prepare && call.PrepareStep != nil {
		if _, _, err := call.PrepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Model: tokenBudgetTestModel{}, Messages: call.Messages,
		}); err != nil {
			return nil, err
		}
	}
	usage := fantasy.Usage{TotalTokens: a.usage}
	if a.finish && call.OnStreamFinish != nil {
		if err := call.OnStreamFinish(usage, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
	}
	response := fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}
	if a.responseUsage {
		response.Usage = usage
	}
	result := &fantasy.AgentResult{Response: response}
	if a.totalUsage {
		result.TotalUsage = usage
	}
	if a.includeStep {
		result.Steps = []fantasy.StepResult{{Response: fantasy.Response{Usage: usage}}}
	}
	return result, nil
}

func newLowTokenBudgetCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := newTokenBudgetRunCoordinator(t, 100)
	c.tokensUsed.Store(95)
	return c
}

func assertLowBudgetSettlement(t *testing.T, c *Coordinator, wantNoProgress int64) {
	t.Helper()
	if got := c.TokensUsed(); got != 105 {
		t.Fatalf("settled usage = %d, want 105", got)
	}
	if got := c.noProgressCounters().Tokens; got != wantNoProgress {
		t.Fatalf("no-progress usage = %d, want %d", got, wantNoProgress)
	}
	if c.tokenReservations != 0 {
		t.Fatalf("settlement left %d reserved tokens", c.tokenReservations)
	}
	if _, err := c.reserveTokenStep(1); err == nil {
		t.Fatal("next token admission was accepted after the ledger was exhausted")
	}
}

func TestRunAgentCallbacklessAuxiliarySettlesObservedUsagePastBudget(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		usage:         10,
		responseUsage: true,
	}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	assertLowBudgetSettlement(t, c, 10)
}

func TestRunAgentCallbacklessWorkerResponseOnlyProjectsReceiptUsageOnce(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	const todoID = "worker-response-only"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	_, steps, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		responseUsage: true,
		usage:         10,
	}, "worker", "prompt", nil, &taskTiming{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("response-only worker returned %d steps, want none", len(steps))
	}
	assertLowBudgetSettlement(t, c, 10)

	// The terminal execution event sees no Steps for this response-only call;
	// replaying that projection must not charge the reconciled receipt usage a
	// second time.
	c.recordExecutionEvent(todoID, "worker", 1, "done", "model", 0, usageFromSteps(steps))
	if got := c.noProgressCounters().Tokens; got != 10 {
		t.Fatalf("receipt-backed response-only usage after event replay = %d, want 10", got)
	}
}

func TestRunAgentCallbackBackedWorkerResponseOnlyProjectsReceiptUsageOnce(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	const todoID = "worker-callback-response-only"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	_, steps, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		finish: true,
		usage:  10,
	}, "worker", "prompt", nil, &taskTiming{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("response-only worker returned %d steps, want none", len(steps))
	}
	assertLowBudgetSettlement(t, c, 10)

	// The terminal event has no step usage for this callback-backed response;
	// projecting it must not add a second 10-token receipt charge.
	c.recordExecutionEvent(todoID, "worker", 1, "done", "model", 0, usageFromSteps(steps))
	if got := c.noProgressCounters().Tokens; got != 10 {
		t.Fatalf("callback-backed response-only usage after event replay = %d, want 10", got)
	}
}

func TestRunAgentCallbackBackedWorkerResponseOnlyUsesImplicitDirectAttempt(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	const todoID = "direct-callback-response-only"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	_, steps, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		finish: true,
		usage:  10,
	}, "direct", "prompt", nil, &taskTiming{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("direct response-only worker returned %d steps, want none", len(steps))
	}
	assertLowBudgetSettlement(t, c, 10)
}

type tokenBudgetCallbackErrorAgent struct{}

func (tokenBudgetCallbackErrorAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (tokenBudgetCallbackErrorAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.OnStreamFinish != nil {
		if err := call.OnStreamFinish(fantasy.Usage{TotalTokens: 10}, fantasy.FinishReasonStop, nil); err != nil {
			return nil, err
		}
	}
	return nil, context.Canceled
}

func TestRunAgentCallbackBackedNilResultErrorProjectsReceiptUsageOnce(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	const todoID = "worker-callback-nil-result-error"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	_, steps, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetCallbackErrorAgent{}, "worker", "prompt", nil, &taskTiming{})
	if err == nil {
		t.Fatal("callback-backed nil-result stream unexpectedly succeeded")
	}
	if len(steps) != 0 {
		t.Fatalf("callback-backed nil-result stream returned %d steps, want none", len(steps))
	}
	assertLowBudgetSettlement(t, c, 10)

	// A terminal failure event carries no step usage for this callback-backed
	// stream; projecting it must not charge the receipt usage a second time.
	c.recordExecutionEvent(todoID, "worker", 1, "error", "model", 0, usageFromSteps(steps))
	if got := c.noProgressCounters().Tokens; got != 10 {
		t.Fatalf("callback-backed nil-result usage after terminal failure = %d, want 10", got)
	}
}

func TestRunAgentPrepareStepCallbackBackedWorkerResponseOnlyProjectsReceiptUsage(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	c.session.Config.Generation.MaxTokens = "1"
	const todoID = "worker-prepare-callback-response-only"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, todoID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	_, steps, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		prepare: true,
		finish:  true,
		usage:   10,
	}, "worker", "prompt", nil, &taskTiming{})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 0 {
		t.Fatalf("prepared response-only worker returned %d steps, want none", len(steps))
	}
	assertLowBudgetSettlement(t, c, 10)
}

func TestRunAgentResponseOnlyUsageWithPrepareStepSettlesPastBudget(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	c.session.Config.Generation.MaxTokens = "1"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		prepare:       true,
		responseUsage: true,
		usage:         10,
	}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	assertLowBudgetSettlement(t, c, 10)
}

func TestRunAgentTotalUsageOnlySettlesPastBudget(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		totalUsage: true,
		usage:      10,
	}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	assertLowBudgetSettlement(t, c, 10)
}

func TestRunAgentFinishWithoutPrepareSettlesObservedUsagePastBudget(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		usage:  10,
		finish: true,
	}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	assertLowBudgetSettlement(t, c, 10)
}

func TestRunAgentCallbackBackedLowBudgetSettlesAndReleasesAdmission(t *testing.T) {
	c := newLowTokenBudgetCoordinator(t)
	// Leave room for the pre-call admission so this test exercises settlement
	// after a provider call, rather than the intended admission gate.
	c.session.Config.Generation.MaxTokens = "1"
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetObservedUsageAgent{
		prepare:       true,
		finish:        true,
		includeStep:   true,
		responseUsage: true,
		usage:         10,
	}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	// The callback and result describe one provider step. The callback settles
	// the admission; result reconciliation must be idempotent.
	assertLowBudgetSettlement(t, c, 10)
}

func TestTokenBudgetSharedRootAdmissionAcrossExtraModels(t *testing.T) {
	root := &Coordinator{}
	root.SetBudget(0, 100)
	extra := &Coordinator{tokenBudgetOwner: root}
	rootReservation, err := root.reserveTokenStep(60)
	if err != nil {
		t.Fatal(err)
	}
	extraReservation, err := extra.reserveTokenStep(40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.reserveTokenStep(1); err == nil {
		t.Fatal("shared root admitted a third step beyond the cap")
	}
	root.commitTokenStep(&rootReservation, 60)
	extra.commitTokenStep(&extraReservation, 40)
	if got := root.TokensUsed(); got != 100 {
		t.Fatalf("shared root usage = %d, want 100", got)
	}
}

func TestTokenBudgetContextOverflowRetryUsesIndependentReservation(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 100)
	first, err := c.reserveTokenStep(60)
	if err != nil {
		t.Fatal(err)
	}
	c.releaseTokenStep(&first)
	second, err := c.reserveTokenStep(60)
	if err != nil {
		t.Fatalf("retry could not reserve after failed stream was released: %v", err)
	}
	c.commitTokenStep(&second, 23)
	if got := c.TokensUsed(); got != 23 {
		t.Fatalf("context retry charged %d tokens, want 23", got)
	}
}

func TestTokenBudgetCancellationDoesNotStrandReservation(t *testing.T) {
	c := &Coordinator{}
	c.SetBudget(0, 50)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("cancellation fixture did not cancel")
	}
	reservation, err := c.reserveTokenStep(50)
	if err != nil {
		t.Fatal(err)
	}
	c.releaseTokenStep(&reservation)
	if _, err := c.reserveTokenStep(50); err != nil {
		t.Fatalf("cancellation stranded reservation: %v", err)
	}
}

func TestRunAgentChargesStreamingStepOnceAcrossAttempts(t *testing.T) {
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "usage-accounting"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.SetBudget(0, 10000)
	ctx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	for attempt := 0; attempt < 2; attempt++ {
		if _, _, err := c.runAgentWithStatusAndHistory(ctx, tokenBudgetStreamingAgent{}, "worker", "prompt", nil, &taskTiming{}); err != nil {
			t.Fatalf("attempt %d failed: %v", attempt+1, err)
		}
	}
	if got := c.TokensUsed(); got != 34 {
		t.Fatalf("stream usage charged %d, want 34 (17 per attempt)", got)
	}
}
