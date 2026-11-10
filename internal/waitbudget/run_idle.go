package waitbudget

import (
	"context"
	"sync"
	"time"
)

type runIdleKey struct{}

// RunIdleClock measures the union of explicit runtime idle intervals across a
// run. Its zero value is ready for use. Provider cooldowns do not pause it.
type RunIdleClock struct{ clock clock }

// Context attaches the run's shared idle accounting to a tool context.
func (c *RunIdleClock) Context(parent context.Context) context.Context {
	return context.WithValue(parent, runIdleKey{}, c)
}

func (c *RunIdleClock) Elapsed() time.Duration { return c.clock.elapsed() }

// WithTimeout limits remaining run time, excluding only explicit run idle.
// Ordinary working-time pauses must not extend this budget. External parent
// cancellation and deadlines are still authoritative.
func (c *RunIdleClock) WithTimeout(parent context.Context, remaining time.Duration) (context.Context, context.CancelFunc) {
	timed, cancel := WithTimeout(parent, remaining, c.Elapsed)
	return c.Context(runTimeoutContext{Context: timed, workingClock: parent.Value(clockKey{})}), cancel
}

// Hide the timer's working clock from Pause: only the explicit run idle
// accumulator supplied to WithTimeout may extend a run duration budget.
type runTimeoutContext struct {
	context.Context
	workingClock any
}

func (c runTimeoutContext) Value(key any) any {
	if _, ok := key.(clockKey); ok {
		return c.workingClock
	}
	return c.Context.Value(key)
}

// PauseRunIdle excludes runtime-controlled idle time from both working and run
// duration budgets. Overlapping waits count once, and release is idempotent.
func PauseRunIdle(ctx context.Context) func() {
	resumeWork := Pause(ctx)
	c, _ := ctx.Value(runIdleKey{}).(*RunIdleClock)
	if c == nil {
		return resumeWork
	}
	resumeRun := pause(&c.clock)
	return sync.OnceFunc(func() { resumeRun(); resumeWork() })
}
