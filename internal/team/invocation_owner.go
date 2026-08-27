package team

import (
	"context"
	"log"
	"sync"
)

// invocationOwner is the joinable cancellation owner for one public
// coordinator invocation. Its parent watcher is deliberately independent of
// the watchdog: a provider call that ignores its context must still have its
// Hufu-owned proxy aborted while the synchronous Stream call is blocked.
type invocationOwner struct {
	coordinator *Coordinator
	lease       *invocationLease
	parent      context.Context
	ctx         context.Context
	cancel      context.CancelCauseFunc
	watchdog    *invocationWatchdog
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	abortOnce   sync.Once
	abortErr    error
}

// invocationLease serializes the mutable lifetime of one public coordinator
// invocation. Its boundary state belongs to this lease, so a later
// invocation cannot observe or abort an earlier invocation's provider owner.
type invocationLease struct {
	coordinator *Coordinator
	boundaryMu  sync.Mutex
	started     bool
	err         error
	releaseOnce sync.Once
}

func (c *Coordinator) acquireInvocationLease(ctx context.Context) (*invocationLease, error) {
	if c == nil {
		return nil, context.Canceled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := invocationContextError(ctx); err != nil {
			return nil, err
		}
		c.invocationLeaseMu.Lock()
		if !c.invocationLeaseHeld {
			if err := invocationContextError(ctx); err != nil {
				c.invocationLeaseMu.Unlock()
				return nil, err
			}
			lease := &invocationLease{coordinator: c}
			c.invocationLeaseHeld = true
			c.invocationLease = lease
			c.invocationLeaseMu.Unlock()
			return lease, nil
		}
		wait := c.invocationLeaseWait
		if wait == nil {
			wait = make(chan struct{})
			c.invocationLeaseWait = wait
		}
		c.invocationLeaseMu.Unlock()

		select {
		case <-ctx.Done():
			return nil, invocationContextError(ctx)
		case <-wait:
		}
	}
}

func (l *invocationLease) release() {
	if l == nil || l.coordinator == nil {
		return
	}
	l.releaseOnce.Do(func() {
		c := l.coordinator
		c.invocationLeaseMu.Lock()
		if c.invocationLease == l {
			c.invocationLease = nil
			c.invocationLeaseHeld = false
			wait := c.invocationLeaseWait
			c.invocationLeaseWait = nil
			if wait != nil {
				close(wait)
			}
		}
		c.invocationLeaseMu.Unlock()
	})
}

func (c *Coordinator) activeInvocationLease() *invocationLease {
	if c == nil {
		return nil
	}
	c.invocationLeaseMu.Lock()
	lease := c.invocationLease
	c.invocationLeaseMu.Unlock()
	return lease
}

func newInvocationOwner(c *Coordinator, parent context.Context) *invocationOwner {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	return &invocationOwner{
		coordinator: c,
		parent:      parent,
		ctx:         ctx,
		cancel:      cancel,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

func newInvocationOwnerWithLease(c *Coordinator, parent context.Context, lease *invocationLease) *invocationOwner {
	owner := newInvocationOwner(c, parent)
	owner.lease = lease
	return owner
}

func (o *invocationOwner) start() {
	go func() {
		defer close(o.done)
		select {
		case <-o.parent.Done():
			// Parent cancellation is authoritative and must synchronously close
			// the provider boundary before this owner can be joined.
			if err := o.abortProviderBoundary(); err != nil {
				log.Printf("warning: invocation parent cancellation abort: %v", err)
			}
			cause := context.Cause(o.parent)
			if cause == nil {
				cause = o.parent.Err()
			}
			o.cancel(cause)
		case <-o.stop:
		}
	}()
}

// abortProviderBoundary is the exactly-once close gate shared by parent
// cancellation, stall handling, and normal invocation finalization.
func (o *invocationOwner) abortProviderBoundary() error {
	if o == nil {
		return nil
	}
	o.abortOnce.Do(func() {
		o.abortErr = o.coordinator.abortProviderExecutionBoundary()
	})
	return o.abortErr
}

// close cancels the invocation, synchronously closes the provider boundary,
// and joins the parent watcher. The caller joins the watchdog separately.
func (o *invocationOwner) close() error {
	if o == nil {
		return nil
	}
	o.cancel(nil)
	o.stopOnce.Do(func() { close(o.stop) })
	err := o.abortProviderBoundary()
	<-o.done
	return err
}
