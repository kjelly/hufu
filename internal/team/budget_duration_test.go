package team

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestRunBudgetExcludesIdleAcrossStepAndProviderChecks(t *testing.T) {
	c := newTestCoordinator(t)
	c.SetBudget(60, 0)
	synctest.Test(t, func(t *testing.T) {
		c.sessionTime = time.Now()
		ctx := c.runIdle.Context(t.Context())
		time.Sleep(10 * time.Second)
		// A quota timer already in flight must observe another worker's idle.
		quota, cancel := (providerRequestAdmission{c}).quotaWaitContext(ctx)
		defer cancel()
		resume := waitbudget.PauseRunIdle(ctx)
		time.Sleep(10 * time.Minute)
		if exceeded, _ := c.budgetExceeded(); exceeded || quota.Err() != nil {
			t.Fatal("runtime idle exhausted run duration")
		}
		resume()
		if elapsed := c.runBudgetElapsed(); elapsed != 10*time.Second {
			t.Fatalf("charged elapsed=%v", elapsed)
		}
		// A later quota timer starts with the remaining charged budget, without
		// counting prior idle twice. Both must expire together after 50 seconds.
		later, endLater := (providerRequestAdmission{c}).quotaWaitContext(ctx)
		defer endLater()
		time.Sleep(50 * time.Second)
		synctest.Wait()
		if !errors.Is(quota.Err(), context.DeadlineExceeded) || !errors.Is(later.Err(), context.DeadlineExceeded) {
			t.Fatalf("quota=%v later=%v", quota.Err(), later.Err())
		}
		time.Sleep(time.Second)
		if exceeded, _ := c.budgetExceeded(); !exceeded {
			t.Fatal("run budget did not resume charging")
		}
	})
}
