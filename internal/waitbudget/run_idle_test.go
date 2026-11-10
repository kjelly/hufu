package waitbudget

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunIdleExcludesOverlappingWaitsButChargesProviderCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var idle RunIdleClock
		total, endTotal := idle.WithTimeout(t.Context(), time.Minute)
		defer endTotal()
		worker, endWorker := WithTimeout(total, 30*time.Second)
		defer endWorker()
		time.Sleep(10 * time.Second)
		first := PauseRunIdle(worker)
		time.Sleep(2 * time.Minute)
		second := PauseRunIdle(worker)
		time.Sleep(3 * time.Minute)
		first()
		first()
		time.Sleep(time.Minute)
		second()
		if idle.Elapsed() != 6*time.Minute || total.Err() != nil || worker.Err() != nil {
			t.Fatalf("idle=%v total=%v worker=%v", idle.Elapsed(), total.Err(), worker.Err())
		}
		// A provider cooldown excludes working time but consumes total duration.
		resume := Pause(worker)
		time.Sleep(50 * time.Second)
		synctest.Wait()
		if !errors.Is(total.Err(), context.DeadlineExceeded) {
			t.Fatalf("provider wait extended run budget: %v", total.Err())
		}
		resume()
	})
}

func TestRunIdlePreservesParentCancellationAndExternalDeadline(t *testing.T) {
	for _, scenario := range []string{"cancelled", "deadline", "already_cancelled", "exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, stop := context.WithCancel(t.Context())
				defer stop()
				var outer context.Context = parent
				want := context.Canceled
				if scenario == "deadline" {
					wall, cancel := WithWallDeadline(parent, time.Now().Add(time.Second))
					defer cancel()
					outer, want = wall, context.DeadlineExceeded
				}
				if scenario == "already_cancelled" {
					stop()
				}
				remaining := time.Minute
				if scenario == "exhausted" {
					remaining, want = 0, context.DeadlineExceeded
				}
				var idle RunIdleClock
				ctx, cancel := idle.WithTimeout(outer, remaining)
				defer cancel()
				resume := PauseRunIdle(ctx)
				defer resume()
				if scenario == "cancelled" {
					stop()
				}
				<-ctx.Done()
				if !errors.Is(ctx.Err(), want) {
					t.Fatalf("error=%v want=%v", ctx.Err(), want)
				}
			})
		})
	}
}
