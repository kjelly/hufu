package team

import (
	"errors"
	"fmt"
	"sync"
)

// attemptBudget is a conservative, per-agent-attempt circuit breaker on the
// content one attempt may accumulate. A run-level budget is checked only
// between coordinator operations; without this guard one long model/tool loop
// can consume most of that budget before control returns to the coordinator.
//
// It charges *new* content, not re-sent content. Every model step resends the
// whole conversation, so charging each request in full made the limit behave as
// a step ceiling that shrank as the injected context grew: a real run charged
// 497k for 35 tool calls whose outputs totalled under 7k tokens, and every task
// that had to poll a long-running job died at the same point no matter how
// little work it did. Growth accounting bounds what an attempt can actually
// pull in — context growth plus generated output — so a runaway loop still
// trips while an honest long task does not.
type attemptBudget struct {
	limit int64

	mu sync.Mutex
	// used is the total new content charged so far.
	used int64
	// context is the size of the request last charged, so resending the same
	// history costs nothing and only real growth is charged. It follows the
	// request down as well as up: after compaction shrinks the conversation,
	// regrowing it is new content again and is charged again.
	context int64
	// outputContextCredit is provider-reported output already charged when it
	// was generated. The immediately following request normally includes that
	// output in its context, so reserveContext credits only the reflected part
	// instead of charging the same content twice. The credit expires at the
	// next request; hidden reasoning or output removed by compaction therefore
	// remains charged and cannot subsidize unrelated later growth.
	outputContextCredit int64
}

type attemptBudgetExceededError struct {
	Limit     int64
	Used      int64
	Requested int64
}

func (e *attemptBudgetExceededError) Error() string {
	return fmt.Sprintf("attempt content budget exceeded: %d new tokens accumulated in this attempt, +%d requested, limit %d "+
		"(counts newly added context and generated output, not resent history)", e.Used, e.Requested, e.Limit)
}

func newAttemptBudget(limit int) *attemptBudget {
	if limit <= 0 {
		return nil
	}
	return &attemptBudget{limit: int64(limit)}
}

// reserveContext charges the growth of this request relative to the previous
// step. The estimate is derived from the messages themselves, so the guard
// keeps working against a provider that reports no usage at all.
func (b *attemptBudget) reserveContext(estimate int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	growth := estimate - b.context
	if growth <= 0 {
		// A resend, or a compaction. Track the new size so later regrowth is
		// charged from here rather than from a high-water mark. Any generated
		// output not reflected in this request remains charged, but cannot be
		// used as credit against unrelated future growth.
		b.context = estimate
		b.outputContextCredit = 0
		return nil
	}
	credit := b.outputContextCredit
	if credit > growth {
		credit = growth
	}
	charge := growth - credit
	if b.used+charge > b.limit {
		return &attemptBudgetExceededError{Limit: b.limit, Used: b.used, Requested: charge}
	}
	b.used += charge
	b.context = estimate
	b.outputContextCredit = 0
	return nil
}

// chargeOutput charges what the model generated on one step. The charge is
// retained immediately so a terminal output cannot escape the budget. The
// next request may credit the portion reflected in its context, preventing
// the same assistant output from being counted once as generated output and a
// second time as context growth.
func (b *attemptBudget) chargeOutput(tokens int64) error {
	if b == nil || tokens <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+tokens > b.limit {
		return &attemptBudgetExceededError{Limit: b.limit, Used: b.used, Requested: tokens}
	}
	b.used += tokens
	b.outputContextCredit += tokens
	return nil
}

// snapshot reports the charged total and the limit, for telemetry.
func (b *attemptBudget) snapshot() (used, limit int64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.limit
}

type attemptBudgetKey struct{}

func isAttemptBudgetExceeded(err error) bool {
	var target *attemptBudgetExceededError
	return errors.As(err, &target)
}
