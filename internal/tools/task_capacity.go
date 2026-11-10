package tools

import (
	"context"
	"sync"
)

// TaskCapacity is a scheduler-owned, process-local concurrency lease.
// Waiting does not change the durable task occurrence or release its resources.
type TaskCapacity interface {
	EnterTool(context.Context) (func(), error)
	Wait(context.Context) (func() error, error)
}

type taskCapacityKey struct{}
type capacityToolKey struct{}

// WithTaskCapacity attaches the admitted task's lease to its execution context.
func WithTaskCapacity(ctx context.Context, capacity TaskCapacity) context.Context {
	return context.WithValue(ctx, taskCapacityKey{}, capacity)
}

// EnterCapacityTool accounts for parallel tool calls, counting nested gateway
// wrappers only once. A suspended task must regain capacity before any tool runs.
func EnterCapacityTool(ctx context.Context) (context.Context, func(), error) {
	c, _ := ctx.Value(taskCapacityKey{}).(TaskCapacity)
	if c == nil || ctx.Value(capacityToolKey{}) == c {
		return ctx, func() {}, nil
	}
	leave, err := c.EnterTool(ctx)
	return context.WithValue(ctx, capacityToolKey{}, c), leave, err
}

// BeginCapacityWait yields a task's slot while waiting for human input or a
// runtime resource. Call the returned function before resuming execution, even
// on error. This does not itself exclude the wait from duration budgets.
func BeginCapacityWait(ctx context.Context) (func() error, error) {
	c, _ := ctx.Value(taskCapacityKey{}).(TaskCapacity)
	if c == nil {
		return func() error { return nil }, nil
	}
	// Native actions can reach a wait without a model tool wrapper. Count
	// that caller too, so its wait cannot hide another working parallel tool.
	ctx, leave, err := EnterCapacityTool(ctx)
	if err != nil {
		return nil, err
	}
	resume, err := c.Wait(ctx)
	if err != nil {
		leave()
		return nil, err
	}
	return sync.OnceValue(func() error {
		defer leave()
		return resume()
	}), nil
}

// AcquireTaskResource waits for a runtime resource without occupying the last
// task slot needed by that resource's suspended owner. The caller releases ch
// after use. Team-slot re-entry itself must use an unmanaged semaphore wait.
func AcquireTaskResource(ctx context.Context, ch chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ch == nil {
		return nil
	}
	select {
	case ch <- struct{}{}:
		return nil
	default:
	}
	resume, err := BeginCapacityWait(ctx)
	if err != nil {
		return err
	}
	acquired := false
	select {
	case ch <- struct{}{}:
		acquired = true
	case <-ctx.Done():
		err = ctx.Err()
	}
	if resumeErr := resume(); resumeErr != nil {
		if acquired {
			<-ch
		}
		return resumeErr
	}
	return err
}
