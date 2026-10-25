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

func TestWallDeadlineForwardsParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithCancel(t.Context())
		ctx, stop := WithWallDeadline(parent, time.Now().Add(time.Hour))
		defer stop()
		cancel()
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("wall deadline lost parent cancellation: %v", ctx.Err())
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
