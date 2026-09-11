package team

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Budget ownership for the decision-aware runtime
// (docs/architecture/decision-runtime.md §29, Phase 0.5).
//
// budgetLedger is the single owner of the run's resource counters. Nothing else
// increments or decrements them: StopPolicy, diagnosis, plan revision and the
// decision engine all read through this type. The ledger is embedded in
// Coordinator so the run-wide ledger reached through tokenBudgetRoot stays the
// one that every caller mutates, and so existing field references keep
// resolving to the same storage.

// BudgetLimits are the configured caps. Zero means unlimited, matching the
// long-standing convention of TeamConfig.MaxWallClock / MaxTotalTokens.
type BudgetLimits struct {
	MaxWallClock time.Duration
	MaxTokens    int64
}

// BudgetManager is the read/interpret surface over the run's resource ledger.
// StopPolicy interprets whether observed state permits continuation; it never
// duplicates accounting (spec §29).
type BudgetManager interface {
	// TokensUsed returns cumulative LLM tokens charged to the run.
	TokensUsed() int64
	// Reserved returns tokens admitted for in-flight steps but not yet settled.
	Reserved() int64
	// Limits returns the configured caps (0 = unlimited).
	Limits() BudgetLimits
	// Exceeded reports whether any configured budget is exhausted, with a
	// human-readable reason. elapsed is supplied by the caller because the
	// ledger owns counters, not the session's start time.
	Exceeded(elapsed time.Duration) (bool, string)
	// Snapshot returns a durable view for events and diagnostics. Attempt and
	// diagnostic-task fields are owned by the caller, not the ledger.
	Snapshot(elapsed time.Duration) BudgetSnapshot
}

// budgetLedger holds the counters. Field names are unchanged from when they
// lived directly on Coordinator so promotion keeps every existing reference,
// including tests, resolving to this one storage.
type budgetLedger struct {
	maxWallClock      time.Duration // 0 = unlimited
	tokenBudget       int64         // 0 = unlimited; cumulative LLM tokens
	tokensUsed        atomic.Int64
	tokenBudgetMu     sync.Mutex
	tokenReservations int64
}

// Budget returns the run-wide ledger. Extra-model coordinators share the parent
// run's ledger, so callers always reach the single owner.
func (c *Coordinator) Budget() BudgetManager {
	owner := c.tokenBudgetRoot()
	if owner == nil {
		return nil
	}
	return &owner.budgetLedger
}

// setLimits applies configured caps. A non-positive value leaves the existing
// cap untouched, preserving SetBudget's long-standing behavior.
func (l *budgetLedger) setLimits(maxWallClockSeconds, maxTotalTokens int64) {
	l.tokenBudgetMu.Lock()
	defer l.tokenBudgetMu.Unlock()
	if maxWallClockSeconds > 0 {
		l.maxWallClock = time.Duration(maxWallClockSeconds) * time.Second
	}
	if maxTotalTokens > 0 {
		l.tokenBudget = maxTotalTokens
	}
}

// TokensUsed implements BudgetManager.
func (l *budgetLedger) TokensUsed() int64 { return l.tokensUsed.Load() }

// Reserved implements BudgetManager.
func (l *budgetLedger) Reserved() int64 {
	l.tokenBudgetMu.Lock()
	defer l.tokenBudgetMu.Unlock()
	return l.tokenReservations
}

// Limits implements BudgetManager.
func (l *budgetLedger) Limits() BudgetLimits {
	l.tokenBudgetMu.Lock()
	defer l.tokenBudgetMu.Unlock()
	return BudgetLimits{MaxWallClock: l.maxWallClock, MaxTokens: l.tokenBudget}
}

// addTokens charges observed usage to the run.
func (l *budgetLedger) addTokens(total int64) {
	if total > 0 {
		l.tokensUsed.Add(total)
	}
}

// Exceeded implements BudgetManager.
func (l *budgetLedger) Exceeded(elapsed time.Duration) (bool, string) {
	l.tokenBudgetMu.Lock()
	maxWallClock := l.maxWallClock
	tokenBudget := l.tokenBudget
	reservations := l.tokenReservations
	l.tokenBudgetMu.Unlock()
	used := l.tokensUsed.Load()
	if maxWallClock > 0 && elapsed > maxWallClock {
		return true, fmt.Sprintf("wall-clock budget exceeded (%s > %s)", elapsed.Round(time.Second), maxWallClock)
	}
	if tokenBudget > 0 && used+reservations >= tokenBudget {
		return true, fmt.Sprintf("token budget exceeded (%d >= %d)", used+reservations, tokenBudget)
	}
	return false, ""
}

// Snapshot implements BudgetManager.
func (l *budgetLedger) Snapshot(elapsed time.Duration) BudgetSnapshot {
	limits := l.Limits()
	snapshot := BudgetSnapshot{MaxTokens: limits.MaxTokens, TokensUsed: l.tokensUsed.Load()}
	if limits.MaxWallClock > 0 {
		snapshot.MaxDurationSeconds = int64(limits.MaxWallClock / time.Second)
	}
	_ = elapsed
	return snapshot
}

// reserve admits a step against the remaining budget. Reservations keep
// concurrently admitted model steps from all observing the same remaining
// budget and starting more work than the run can safely absorb.
func (l *budgetLedger) reserve(amount int64) (tokenStepReservation, error) {
	if amount <= 0 {
		return tokenStepReservation{}, nil
	}
	l.tokenBudgetMu.Lock()
	defer l.tokenBudgetMu.Unlock()
	if l.tokenBudget <= 0 {
		return tokenStepReservation{}, nil
	}
	used := l.tokensUsed.Load()
	if used+l.tokenReservations+amount > l.tokenBudget {
		return tokenStepReservation{}, fmt.Errorf("token budget admission refused (%d requested, %d used, %d reserved, limit %d)", amount, used, l.tokenReservations, l.tokenBudget)
	}
	l.tokenReservations += amount
	return tokenStepReservation{amount: amount}, nil
}

// commit releases the admission reservation and charges exactly the
// provider-reported total once.
func (l *budgetLedger) commit(reservation *tokenStepReservation, total int64) bool {
	l.tokenBudgetMu.Lock()
	defer l.tokenBudgetMu.Unlock()
	if reservation.settled {
		return false
	}
	if reservation.amount > 0 {
		l.tokenReservations -= reservation.amount
		if l.tokenReservations < 0 {
			l.tokenReservations = 0
		}
	}
	reservation.amount = 0
	reservation.settled = true
	if total > 0 {
		l.tokensUsed.Add(total)
	}
	return true
}
