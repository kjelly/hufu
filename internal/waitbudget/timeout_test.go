package waitbudget

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestTimeoutAlreadyExpired(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second} {
		ctx, cancel := WithTimeout(t.Context(), duration)
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("timeout %s: %v", duration, ctx.Err())
		}
		cancel()
	}
	parent, stop := context.WithCancel(t.Context())
	stop()
	ctx, cancel := WithTimeout(parent, time.Hour)
	defer cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("cancelled parent: %v", ctx.Err())
	}
}

func TestWallDeadlinePreservesParentErrorAndCause(t *testing.T) {
	customCause := errors.New("parent completion cause")
	for _, tt := range []struct {
		name               string
		timeout, completed bool
		cause              error
	}{
		{name: "cancelled"},
		{name: "cancelled with cause", cause: customCause},
		{name: "already cancelled", completed: true, cause: customCause},
		{name: "timed out", timeout: true},
		{name: "timed out with cause", timeout: true, cause: customCause},
		{name: "already timed out", timeout: true, completed: true, cause: customCause},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var parentCtx context.Context = parent
				complete := func() { cancel(tt.cause) }
				if tt.timeout {
					timed, stop := context.WithTimeoutCause(parent, time.Minute, tt.cause)
					defer stop()
					parentCtx = timed
					complete = func() { time.Sleep(time.Minute); synctest.Wait() }
				}
				if tt.completed {
					complete()
				}
				ctx, stop := WithWallDeadline(parentCtx, time.Now().Add(time.Hour))
				defer stop()
				child, stopChild := context.WithCancel(ctx)
				defer stopChild()
				if !tt.completed {
					complete()
				}
				synctest.Wait()
				for _, got := range []context.Context{ctx, child} {
					if !errors.Is(got.Err(), parentCtx.Err()) || !errors.Is(context.Cause(got), context.Cause(parentCtx)) {
						t.Fatalf("completion=(%v, %v), want parent (%v, %v)", got.Err(), context.Cause(got), parentCtx.Err(), context.Cause(parentCtx))
					}
				}
			})
		})
	}
}

func TestWallDeadlineRemainsFixedDuringProviderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := WithTimeout(t.Context(), time.Second)
		defer cancel()
		deadline := time.Now().Add(time.Minute)
		ctx, stop := WithWallDeadline(parent, deadline)
		defer stop()
		resume := Pause(ctx)
		defer resume()
		time.Sleep(time.Minute)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(ctx), context.DeadlineExceeded) || parent.Err() != nil {
			t.Fatalf("wall deadline=%v cause=%v paused parent=%v", ctx.Err(), context.Cause(ctx), parent.Err())
		}
		if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("wall deadline moved: %v", got)
		}
	})
}

func TestTimeoutExcludesOverlappingProviderWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := WithTimeout(t.Context(), time.Minute)
		defer cancel()
		child, stop := WithTimeout(parent, 30*time.Second)
		defer stop()
		time.Sleep(10 * time.Second)
		resume := Pause(child)
		time.Sleep(10 * time.Minute)
		other := Pause(child)
		time.Sleep(time.Minute)
		resume()
		resume()
		time.Sleep(time.Minute)
		other()
		if child.Err() != nil || parent.Err() != nil {
			t.Fatal("quota wait consumed working time")
		}
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if !errors.Is(child.Err(), context.DeadlineExceeded) {
			t.Fatalf("child error = %v", child.Err())
		}
		if parent.Err() != nil {
			t.Fatal("child timeout ended parent")
		}
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if !errors.Is(parent.Err(), context.DeadlineExceeded) {
			t.Fatalf("parent error = %v", parent.Err())
		}
	})
}

func TestTimeoutPauseHonorsOuterLimits(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "wall deadline", true: "cancellation"}[cancelParent], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, stop := context.WithTimeout(t.Context(), time.Minute)
				defer stop()
				ctx, cancel := WithTimeout(parent, 10*time.Second)
				defer cancel()
				resume := Pause(ctx)
				defer resume()
				if cancelParent {
					stop()
				} else {
					time.Sleep(time.Minute)
				}
				synctest.Wait()
				want := context.DeadlineExceeded
				if cancelParent {
					want = context.Canceled
				}
				if !errors.Is(ctx.Err(), want) {
					t.Fatalf("error=%v, want %v", ctx.Err(), want)
				}
			})
		})
	}
}
