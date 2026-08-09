package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// TestAttemptBudgetDoesNotChargeResentHistory is the regression for the failure
// that killed four of five tasks in one real run. Every model step resends the
// whole conversation; charging each request in full turned a 500k budget into a
// ~25-step ceiling that shrank as the injected context grew. The run charged
// 497k for 35 tool calls whose outputs totalled under 7k tokens, so any task
// that had to poll a long-running job could not finish at all.
func TestAttemptBudgetDoesNotChargeResentHistory(t *testing.T) {
	budget := newAttemptBudget(1000)
	for step := 0; step < 200; step++ {
		if err := budget.reserveContext(400); err != nil {
			t.Fatalf("resending an unchanged 400-token conversation must be free (step %d): %v", step, err)
		}
	}
	if used, limit := budget.snapshot(); used != 400 || limit != 1000 {
		t.Fatalf("used=%d limit=%d, want the conversation charged exactly once", used, limit)
	}
}

func TestAttemptBudgetChargesContextGrowth(t *testing.T) {
	budget := newAttemptBudget(100)
	if err := budget.reserveContext(40); err != nil {
		t.Fatalf("first request within budget: %v", err)
	}
	if err := budget.reserveContext(70); err != nil {
		t.Fatalf("30 tokens of growth within budget: %v", err)
	}
	if used, _ := budget.snapshot(); used != 70 {
		t.Fatalf("used = %d, want 70 (only growth charged)", used)
	}
	if err := budget.reserveContext(230); !isAttemptBudgetExceeded(err) {
		t.Fatalf("growth beyond the limit error = %v, want budget exceeded", err)
	}
}

// TestAttemptBudgetRechargesGrowthAfterCompaction keeps the guard from being
// defeated by a grow/compact/regrow loop: tracking a high-water mark instead of
// the last request size would make every cycle after the first one free.
func TestAttemptBudgetRechargesGrowthAfterCompaction(t *testing.T) {
	budget := newAttemptBudget(100)
	if err := budget.reserveContext(60); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := budget.reserveContext(10); err != nil {
		t.Fatalf("compaction must not fail the attempt: %v", err)
	}
	if err := budget.reserveContext(60); !isAttemptBudgetExceeded(err) {
		t.Fatalf("regrowth after compaction error = %v, want it charged as new content", err)
	}
}

// TestAttemptBudgetChargesOutputOnly pins which half of provider usage is
// charged. Input tokens are the resent conversation, already accounted for as
// growth; charging them again is what made the guard a step ceiling.
func TestAttemptBudgetChargesOutputOnly(t *testing.T) {
	budget := newAttemptBudget(10)
	if err := budget.chargeOutput(6); err != nil {
		t.Fatalf("output within budget: %v", err)
	}
	if err := budget.chargeOutput(5); !isAttemptBudgetExceeded(err) {
		t.Fatalf("output beyond budget error = %v, want budget exceeded", err)
	}
}

func TestAttemptBudgetZeroDisablesGuard(t *testing.T) {
	if got := newAttemptBudget(0); got != nil {
		t.Fatalf("newAttemptBudget(0) = %#v, want nil", got)
	}
}

func TestAttemptBudgetYAMLExplicitZeroIsHonored(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "team.yaml", "name: attempt-budget-zero\nacceptance: 'true'\nreliability:\n  max-tokens-per-attempt: 0\n"); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reliability.MaxTokensPerAttemptSet || cfg.Reliability.MaxTokensPerAttempt != 0 {
		t.Fatalf("parsed per-attempt budget = %+v, want explicit zero", cfg.Reliability)
	}
	c := &Coordinator{session: &TeamSession{Config: cfg}}
	if got := c.reliabilityConfig().MaxTokensPerAttempt; got != 0 {
		t.Fatalf("reliabilityConfig MaxTokensPerAttempt = %d, want 0", got)
	}
	if def := agent.DefaultReliabilityConfig().MaxTokensPerAttempt; def <= 0 {
		t.Fatalf("default per-attempt budget = %d, want positive", def)
	}
}

type attemptBudgetStreamAgent struct {
	messages []fantasy.Message
	// stepMessages, when set, supplies a distinct conversation per step so a
	// growing context can be exercised. Without it every step resends the same
	// messages, which is deliberately free.
	stepMessages [][]fantasy.Message
	usages       []fantasy.Usage
	prepareCalls int
	finishCalls  int
}

func (a *attemptBudgetStreamAgent) messagesForStep(step int) []fantasy.Message {
	if step < len(a.stepMessages) {
		return a.stepMessages[step]
	}
	return a.messages
}

func (a *attemptBudgetStreamAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.result(), nil
}

func (a *attemptBudgetStreamAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	for step, usage := range a.usages {
		if call.PrepareStep != nil {
			a.prepareCalls++
			if _, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Model: attemptBudgetTestModel{}, StepNumber: step, Messages: a.messagesForStep(step)}); err != nil {
				return a.result(), err
			}
		}
		if call.OnStreamFinish != nil {
			a.finishCalls++
			if err := call.OnStreamFinish(usage, "stop", nil); err != nil {
				return a.result(), err
			}
		}
	}
	return a.result(), nil
}

// attemptBudgetTestModel is only passed into PrepareStep so the production
// request logger sees the same non-nil model contract as a real fantasy agent.
// None of its provider methods are invoked by this deterministic stream fake.
type attemptBudgetTestModel struct{}

func (attemptBudgetTestModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (attemptBudgetTestModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (attemptBudgetTestModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (attemptBudgetTestModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (attemptBudgetTestModel) Provider() string { return "test" }

func (attemptBudgetTestModel) Model() string { return "test-model" }

func (a *attemptBudgetStreamAgent) result() *fantasy.AgentResult {
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}}
}

func TestRunAgentAttemptBudgetStopsBeforeNextModelStep(t *testing.T) {
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "attempt-budget"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	stream := &attemptBudgetStreamAgent{
		// The conversation grows by one estimated token per step, so the second
		// request asks for growth the budget cannot cover.
		stepMessages: [][]fantasy.Message{
			{fantasy.NewUserMessage("abcd")},
			{fantasy.NewUserMessage("abcdefgh")},
		},
		usages: []fantasy.Usage{{}, {}},
	}
	ctx := context.WithValue(context.Background(), attemptBudgetKey{}, newAttemptBudget(1))
	_, _, err := c.runAgentWithStatusAndHistory(ctx, stream, "worker", "prompt", nil, &taskTiming{})
	if !isAttemptBudgetExceeded(err) {
		t.Fatalf("run error = %v, want per-attempt budget exceeded", err)
	}
	if stream.prepareCalls != 2 || stream.finishCalls != 1 {
		t.Fatalf("calls prepare=%d finish=%d, want 2/1 so second request was blocked before streaming", stream.prepareCalls, stream.finishCalls)
	}
}

func TestRunAgentAttemptBudgetChargesProviderUsage(t *testing.T) {
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "attempt-budget"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	stream := &attemptBudgetStreamAgent{
		messages: []fantasy.Message{fantasy.NewUserMessage("a")},
		// Input tokens are the resent conversation and must not be charged
		// again; output is what the model actually generated.
		usages: []fantasy.Usage{{InputTokens: 4000, OutputTokens: 5}},
	}
	ctx := context.WithValue(context.Background(), attemptBudgetKey{}, newAttemptBudget(4))
	_, _, err := c.runAgentWithStatusAndHistory(ctx, stream, "worker", "prompt", nil, &taskTiming{})
	if !isAttemptBudgetExceeded(err) {
		t.Fatalf("run error = %v, want generated output to exceed attempt budget", err)
	}
	if stream.finishCalls != 1 || !strings.Contains(err.Error(), "attempt content budget exceeded") {
		t.Fatalf("finish calls=%d error=%v, want finish callback to enforce budget", stream.finishCalls, err)
	}
}

// TestRunAgentAttemptBudgetSurvivesLongResentConversation is the end-to-end
// shape of the real failure: many steps, a large but unchanging conversation,
// and only small provider-reported output. This must complete.
func TestRunAgentAttemptBudgetSurvivesLongResentConversation(t *testing.T) {
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "attempt-budget"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	usages := make([]fantasy.Usage, 35)
	for i := range usages {
		usages[i] = fantasy.Usage{InputTokens: 20_000, OutputTokens: 200}
	}
	stream := &attemptBudgetStreamAgent{
		messages: []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("x", 80_000))}, // ≈20k tokens resent every step
		usages:   usages,
	}
	ctx := context.WithValue(context.Background(), attemptBudgetKey{}, newAttemptBudget(500_000))
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, stream, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatalf("35 steps over an unchanging 20k-token conversation must not exhaust a 500k budget: %v", err)
	}
	if stream.finishCalls != len(usages) {
		t.Fatalf("finish calls = %d, want all %d steps to run", stream.finishCalls, len(usages))
	}
}
