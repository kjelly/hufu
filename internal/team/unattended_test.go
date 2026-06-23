package team

import (
	"context"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

func newBudgetCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "test", Timeout: 30}},
		sessionTime: time.Now(),
	}
}

func TestSetUnattended(t *testing.T) {
	c := newBudgetCoordinator(t)
	if c.IsUnattended() {
		t.Fatal("default should not be unattended")
	}
	c.SetUnattended(true)
	if !c.IsUnattended() {
		t.Error("SetUnattended(true) not reflected")
	}
}

func TestBudgetExceeded_Disabled(t *testing.T) {
	c := newBudgetCoordinator(t)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("no budget configured should never be exceeded")
	}
}

func TestBudgetExceeded_WallClock(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.sessionTime = time.Now().Add(-10 * time.Minute)
	c.SetBudget(60, 0) // 60s budget, already 10min elapsed
	ex, reason := c.budgetExceeded()
	if !ex {
		t.Fatal("wall-clock budget should be exceeded")
	}
	if reason == "" {
		t.Error("expected a reason")
	}
}

func TestBudgetExceeded_WallClock_NotYet(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(3600, 0)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("fresh session should be within a 1h budget")
	}
}

func TestBudgetExceeded_Tokens(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(0, 1000)
	c.tokensUsed.Store(1500)
	ex, reason := c.budgetExceeded()
	if !ex {
		t.Fatal("token budget should be exceeded")
	}
	if reason == "" {
		t.Error("expected a reason")
	}
}

func TestBudgetExceeded_Tokens_NotYet(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(0, 1000)
	c.tokensUsed.Store(999)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("999 < 1000 should be within budget")
	}
}

func TestSetBudget_IgnoresZero(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(100, 200)
	c.SetBudget(0, 0) // zeros must not clear existing budgets
	if c.maxWallClock != 100*time.Second {
		t.Errorf("wall-clock budget should be preserved, got %v", c.maxWallClock)
	}
	if c.tokenBudget != 200 {
		t.Errorf("token budget should be preserved, got %d", c.tokenBudget)
	}
}

func TestRunAcceptance_Empty(t *testing.T) {
	c := newBudgetCoordinator(t)
	if err := c.runAcceptance(context.Background()); err != nil {
		t.Errorf("no acceptance command should be a no-op, got %v", err)
	}
}

func TestRunAcceptance_Pass(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetAcceptance("true")
	if err := c.runAcceptance(context.Background()); err != nil {
		t.Errorf("`true` should pass, got %v", err)
	}
}

func TestRunAcceptance_Fail(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetAcceptance("echo nope >&2; false")
	if err := c.runAcceptance(context.Background()); err == nil {
		t.Fatal("`false` should fail acceptance")
	}
}
