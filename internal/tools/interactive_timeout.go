//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"sync"
	"time"
)

// WithInteractiveAwareTimeout returns a context that expires after timeout of
// *working* time: time spent blocked on interactive prompts (ask_user, path
// consent) is added back to the deadline. It replaces the old global
// deadline-freeze (askUserDeadlineCtx), which had a fatal flaw: the frozen
// deadline snapped back the instant the user answered, so a task that waited
// two hours for consent died with "context deadline exceeded" before the
// just-approved command could even start. With compensation the countdown
// pauses during a prompt and resumes where it left off.
//
// The compensation is process-global (any prompt extends every task's
// deadline). That is intentional: prompts serialize the whole team on one
// human, so no task can make progress while one is pending.
func WithInteractiveAwareTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	c := &interactiveTimeoutCtx{
		parent:   parent,
		start:    time.Now(),
		timeout:  timeout,
		baseWait: InteractiveWaitTotal(),
		done:     make(chan struct{}),
		cancelCh: make(chan struct{}),
	}
	go c.run()
	return c, c.cancel
}

type interactiveTimeoutCtx struct {
	parent   context.Context
	start    time.Time
	timeout  time.Duration
	baseWait time.Duration

	done     chan struct{}
	cancelCh chan struct{}
	cancelMu sync.Once

	mu  sync.Mutex
	err error
}

// effectiveDeadline is the base deadline pushed back by however much
// interactive waiting has happened since this context was created. While a
// prompt is active the deadline moves forward in real time, which freezes
// the countdown without any special-casing.
func (c *interactiveTimeoutCtx) effectiveDeadline() time.Time {
	compensation := InteractiveWaitTotal() - c.baseWait
	return c.start.Add(c.timeout + compensation)
}

func (c *interactiveTimeoutCtx) Deadline() (time.Time, bool) {
	return c.effectiveDeadline(), true
}

func (c *interactiveTimeoutCtx) Done() <-chan struct{} {
	return c.done
}

func (c *interactiveTimeoutCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *interactiveTimeoutCtx) Value(key any) any {
	return c.parent.Value(key)
}

func (c *interactiveTimeoutCtx) cancel() {
	c.cancelMu.Do(func() { close(c.cancelCh) })
}

func (c *interactiveTimeoutCtx) run() {
	timer := time.NewTimer(time.Until(c.effectiveDeadline()))
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			// The deadline may have moved while we slept (a prompt was or is
			// active). Only expire once the *current* effective deadline has
			// really passed.
			remaining := time.Until(c.effectiveDeadline())
			if remaining <= 0 {
				c.finish(context.DeadlineExceeded)
				return
			}
			timer.Reset(remaining)
		case <-c.parent.Done():
			c.finish(c.parent.Err())
			return
		case <-c.cancelCh:
			c.finish(context.Canceled)
			return
		}
	}
}

func (c *interactiveTimeoutCtx) finish(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	close(c.done)
}
