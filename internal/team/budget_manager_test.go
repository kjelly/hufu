package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The ledger must reproduce the pre-extraction budgetExceeded judgment exactly,
// including the message text other layers surface to users
// (docs/architecture/decision-runtime.md Phase 0.5).
func TestBudgetLedgerExceeded(t *testing.T) {
	tests := []struct {
		name        string
		maxWallSecs int64
		maxTokens   int64
		used        int64
		reserved    int64
		elapsed     time.Duration
		wantHit     bool
		wantReason  string
	}{
		{name: "unlimited never trips", elapsed: time.Hour, used: 1 << 40},
		{name: "under both budgets", maxWallSecs: 100, maxTokens: 1000, used: 999, elapsed: time.Second},
		{
			name: "wall clock exceeded", maxWallSecs: 10, elapsed: 11 * time.Second,
			wantHit: true, wantReason: "wall-clock budget exceeded (11s > 10s)",
		},
		{
			name: "wall clock exactly at limit is allowed", maxWallSecs: 10, elapsed: 10 * time.Second,
		},
		{
			name: "tokens exceeded at the limit", maxTokens: 1000, used: 1000,
			wantHit: true, wantReason: "token budget exceeded (1000 >= 1000)",
		},
		{
			name: "reservations count toward the limit", maxTokens: 1000, used: 900, reserved: 100,
			wantHit: true, wantReason: "token budget exceeded (1000 >= 1000)",
		},
		{
			name: "wall clock is reported before tokens", maxWallSecs: 1, maxTokens: 10, used: 100,
			elapsed: 5 * time.Second,
			wantHit: true, wantReason: "wall-clock budget exceeded (5s > 1s)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l budgetLedger
			l.setLimits(tt.maxWallSecs, tt.maxTokens)
			l.addTokens(tt.used)
			if tt.reserved > 0 {
				if _, err := l.reserve(tt.reserved); err != nil {
					t.Fatalf("reserve = %v", err)
				}
			}
			hit, reason := l.Exceeded(tt.elapsed)
			if hit != tt.wantHit || reason != tt.wantReason {
				t.Fatalf("Exceeded = (%v, %q), want (%v, %q)", hit, reason, tt.wantHit, tt.wantReason)
			}
		})
	}
}

// A coordinator's budgetExceeded must agree with the ledger it delegates to.
func TestCoordinatorBudgetDelegatesToLedger(t *testing.T) {
	c := &Coordinator{sessionTime: time.Now().Add(-30 * time.Second)}
	c.SetBudget(10, 0)
	hit, reason := c.budgetExceeded()
	if !hit || !strings.Contains(reason, "wall-clock budget exceeded") {
		t.Fatalf("budgetExceeded = (%v, %q), want a wall-clock trip", hit, reason)
	}

	c2 := &Coordinator{sessionTime: time.Now()}
	c2.SetBudget(0, 100)
	budget := c2.Budget()
	if budget == nil {
		t.Fatal("Budget() = nil")
	}
	if got := budget.Limits().MaxTokens; got != 100 {
		t.Fatalf("Limits().MaxTokens = %d, want 100", got)
	}
	c2.budgetLedger.addTokens(100)
	if got := budget.TokensUsed(); got != 100 {
		t.Fatalf("TokensUsed() = %d, want 100", got)
	}
	hit, reason = c2.budgetExceeded()
	if !hit || !strings.Contains(reason, "token budget exceeded") {
		t.Fatalf("budgetExceeded = (%v, %q), want a token trip", hit, reason)
	}
}

// Extra-model coordinators share the parent run's ledger, so Budget() must
// reach the single owner rather than a private zero ledger.
func TestBudgetReachesRunOwner(t *testing.T) {
	root := &Coordinator{sessionTime: time.Now()}
	root.SetBudget(0, 500)
	child := &Coordinator{sessionTime: time.Now(), tokenBudgetOwner: root}

	child.SetBudget(0, 900)
	if got := root.Budget().Limits().MaxTokens; got != 900 {
		t.Fatalf("child SetBudget did not reach the owner: MaxTokens = %d", got)
	}
	root.budgetLedger.addTokens(42)
	if got := child.TokensUsed(); got != 42 {
		t.Fatalf("child TokensUsed() = %d, want the owner's 42", got)
	}
	if child.Budget() != root.Budget() {
		t.Fatal("child and root resolved to different ledgers")
	}
}

// setLimits must keep SetBudget's long-standing behavior: a non-positive value
// leaves the existing cap untouched rather than clearing it.
func TestBudgetLedgerSetLimitsIgnoresNonPositive(t *testing.T) {
	var l budgetLedger
	l.setLimits(60, 1000)
	l.setLimits(0, 0)
	limits := l.Limits()
	if limits.MaxWallClock != time.Minute || limits.MaxTokens != 1000 {
		t.Fatalf("Limits() = %#v, want the earlier caps preserved", limits)
	}
	l.setLimits(-5, -5)
	limits = l.Limits()
	if limits.MaxWallClock != time.Minute || limits.MaxTokens != 1000 {
		t.Fatalf("Limits() = %#v, want negative values ignored", limits)
	}
}

func TestBudgetLedgerReserveAndCommit(t *testing.T) {
	var l budgetLedger
	l.setLimits(0, 1000)

	reservation, err := l.reserve(400)
	if err != nil {
		t.Fatalf("reserve = %v", err)
	}
	if got := l.Reserved(); got != 400 {
		t.Fatalf("Reserved() = %d, want 400", got)
	}
	if _, err := l.reserve(700); err == nil {
		t.Fatal("reserve beyond the remaining budget was admitted")
	}

	if !l.commit(&reservation, 250) {
		t.Fatal("commit returned false for an unsettled reservation")
	}
	if got := l.Reserved(); got != 0 {
		t.Fatalf("Reserved() after commit = %d, want 0", got)
	}
	if got := l.TokensUsed(); got != 250 {
		t.Fatalf("TokensUsed() = %d, want the provider-reported 250", got)
	}
	if l.commit(&reservation, 250) {
		t.Fatal("double commit charged the run twice")
	}
	if got := l.TokensUsed(); got != 250 {
		t.Fatalf("TokensUsed() after double commit = %d, want 250", got)
	}
}

func TestBudgetLedgerSnapshot(t *testing.T) {
	var l budgetLedger
	l.setLimits(120, 5000)
	l.addTokens(1200)
	snapshot := l.Snapshot(time.Minute)
	if snapshot.MaxDurationSeconds != 120 || snapshot.MaxTokens != 5000 || snapshot.TokensUsed != 1200 {
		t.Fatalf("Snapshot = %#v", snapshot)
	}
}

// Phase 0.5's completion condition: the counters have exactly one owner. Any
// new mutation site outside budget_manager.go reintroduces parallel accounting,
// which is what StopPolicy must never have to reconcile (spec §29).
func TestBudgetCountersHaveASingleOwner(t *testing.T) {
	mutations := []string{
		"tokensUsed.Add(",
		"tokensUsed.Store(",
		"tokenReservations +=",
		"tokenReservations -=",
		"tokenReservations =",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "budget_manager.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, mutation := range mutations {
			if strings.Contains(string(data), mutation) {
				t.Errorf("%s mutates the budget ledger (%q); counters must only be updated in budget_manager.go", name, mutation)
			}
		}
	}
}
