package team

import (
	"context"
	"sync"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// taskCapacity separates admission capacity from task lifetime. The scheduler
// still owns resource claims and drains the task even when its slot is yielded.
// The mutex protects tool goroutines; scheduler maps remain scheduler-owned.
type taskCapacity struct {
	mu              sync.Mutex
	parent          context.Context
	slot            semSlot
	wake            chan<- struct{}
	active, waiting int
}

func (c *taskCapacity) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.slot.released {
		return nil
	}
	resume := waitbudget.Pause(ctx)
	defer resume()
	slot, err := acquireSemUnmanaged(ctx, c.slot.ch)
	if err != nil {
		return err
	}
	c.slot = slot
	return ctx.Err()
}

func (c *taskCapacity) yieldIfIdle() {
	if c.waiting == 0 || c.active > c.waiting || c.slot.released {
		return
	}
	c.slot.release()
	// Unlimited capacity still records suspension for balanced re-entry.
	c.slot.released = true
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *taskCapacity) EnterTool(ctx context.Context) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	c.active++
	return sync.OnceFunc(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.active--
		c.yieldIfIdle()
	}), nil
}

func (c *taskCapacity) Wait(ctx context.Context) (func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.waiting++
	c.yieldIfIdle()
	return sync.OnceValue(func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.waiting--
		// A tool-local cancellation must not let its live worker continue
		// without capacity. Only cancellation of the admitted task can skip
		// re-entry; preserve the tool error after restoring its task's slot.
		admission := c.parent
		if admission == nil {
			admission = ctx
		}
		resumeWork := waitbudget.Pause(ctx)
		defer resumeWork()
		if err := c.acquire(admission); err != nil {
			return err
		}
		return ctx.Err()
	}), nil
}

func (c *taskCapacity) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slot.release()
}
