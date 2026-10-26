// Package waitbudget excludes provider cooldowns from model working time.
// Parent cancellation and ordinary parent deadlines always remain authoritative.
package waitbudget

import (
	"context"
	"sync"
	"time"
)

type clockKey struct{}

// WithWallDeadline owns an ordinary wall-clock timer even if a parent reports
// an earlier moving working-time deadline. Parent cancellation is forwarded;
// merely wrapping a paused context with context.WithDeadline can elide the
// hard timer and inadvertently make an operational deadline movable too.
func WithWallDeadline(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(deadlineHiddenContext{parent}, deadline)
}

// Hide only Deadline so WithDeadline always owns a timer. Retaining Done, Err
// and Value lets context propagate the parent's error and cause normally.
type deadlineHiddenContext struct {
	context.Context
}

func (deadlineHiddenContext) Deadline() (time.Time, bool) { return time.Time{}, false }

type clock struct {
	mu     sync.Mutex
	active int
	start  time.Time
	total  time.Duration
	parent *clock
}

func (c *clock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active > 0 {
		return c.total + time.Since(c.start)
	}
	return c.total
}

// Pause suspends all working-time budgets in ctx's ancestry. Overlapping waits
// count once, including when concurrent workers share a coordinator deadline.
func Pause(ctx context.Context) func() {
	c, _ := ctx.Value(clockKey{}).(*clock)
	for current := c; current != nil; current = current.parent {
		current.mu.Lock()
		if current.active == 0 {
			current.start = time.Now()
		}
		current.active++
		current.mu.Unlock()
	}
	return sync.OnceFunc(func() {
		for current := c; current != nil; current = current.parent {
			current.mu.Lock()
			current.active--
			if current.active == 0 {
				current.total += time.Since(current.start)
			}
			current.mu.Unlock()
		}
	})
}

// WithTimeout budgets model working time. additional optionally supplies the
// cumulative interactive wait already maintained by the tools package.
func WithTimeout(parent context.Context, timeout time.Duration, additional ...func() time.Duration) (context.Context, context.CancelFunc) {
	ancestor, _ := parent.Value(clockKey{}).(*clock)
	c := &timeoutContext{parent: parent, clock: &clock{parent: ancestor}, start: time.Now(), timeout: timeout, done: make(chan struct{}), cancelCh: make(chan struct{})}
	if len(additional) > 0 {
		c.additional = additional[0]
		c.baseAdditional = c.additional()
	}
	if err := parent.Err(); err != nil {
		c.finish(err)
		return c, c.cancel
	}
	if timeout <= 0 {
		c.finish(context.DeadlineExceeded)
		return c, c.cancel
	}
	go c.run()
	return c, c.cancel
}

type timeoutContext struct {
	parent         context.Context
	clock          *clock
	start          time.Time
	timeout        time.Duration
	additional     func() time.Duration
	baseAdditional time.Duration
	done           chan struct{}
	cancelCh       chan struct{}
	once           sync.Once
	mu             sync.Mutex
	err            error
}

func (c *timeoutContext) Deadline() (time.Time, bool) {
	wait := c.clock.elapsed()
	if c.additional != nil {
		wait += c.additional() - c.baseAdditional
	}
	deadline := c.start.Add(c.timeout + wait)
	if parent, ok := c.parent.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	return deadline, true
}
func (c *timeoutContext) Done() <-chan struct{} { return c.done }
func (c *timeoutContext) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *timeoutContext) Value(key any) any {
	if _, ok := key.(clockKey); ok {
		return c.clock
	}
	return c.parent.Value(key)
}
func (c *timeoutContext) cancel() { c.once.Do(func() { close(c.cancelCh) }) }
func (c *timeoutContext) run() {
	deadline, _ := c.Deadline()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var err error
	for err == nil {
		select {
		case <-timer.C:
			deadline, _ = c.Deadline()
			if remaining := time.Until(deadline); remaining > 0 {
				timer.Reset(remaining)
			} else {
				err = context.DeadlineExceeded
			}
		case <-c.parent.Done():
			err = c.parent.Err()
		case <-c.cancelCh:
			err = context.Canceled
		}
	}
	c.finish(err)
}

func (c *timeoutContext) finish(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	close(c.done)
}
