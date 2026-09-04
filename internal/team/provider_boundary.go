package team

import (
	"context"
	"fmt"
	"os"
)

// startProviderExecutionBoundary is the invocation-owned admission gate for
// model calls. It is intentionally separate from Fantasy's context: the
// proxy process is the resource that can be killed when a provider ignores
// cancellation.
func (c *Coordinator) startProviderExecutionBoundary(ctx context.Context) error {
	if c == nil || c.providerManager == nil {
		return nil
	}
	c.providerBoundaryLifecycleMu.Lock()
	defer c.providerBoundaryLifecycleMu.Unlock()
	lease := c.activeInvocationLease()
	started, boundaryErr := c.providerBoundaryState(lease)
	if started {
		return boundaryErr
	}
	if err := invocationContextError(ctx); err != nil {
		c.setProviderBoundaryState(lease, false, err)
		return err
	}
	// A previous invocation may have failed before the boundary became live.
	// Clear its diagnostic and invocation-owned auxiliary model instances so a
	// later run cannot retain a stale provider URL or startup error.
	c.setProviderBoundaryState(lease, false, nil)
	c.sidecarInitMu.Lock()
	c.sidecarInst = nil
	c.sidecarInit = false
	c.sidecarInitMu.Unlock()
	c.guardInitMu.Lock()
	c.guardInst = nil
	c.guardInit = false
	c.guardInitMu.Unlock()
	c.judgeInitMu.Lock()
	c.judgeInst = nil
	c.judgeInit = false
	c.judgeInitMu.Unlock()
	executable, err := os.Executable()
	if err != nil {
		boundaryErr := fmt.Errorf("provider hard-abort boundary unavailable: resolve hufu executable: %w", err)
		c.setProviderBoundaryState(lease, false, boundaryErr)
		return boundaryErr
	}
	start := c.providerManager.StartInvocationProxy
	if c.providerBoundaryStart != nil {
		start = c.providerBoundaryStart
	}
	if err := start(ctx, executable); err != nil {
		if ctxErr := invocationContextError(ctx); ctxErr != nil {
			err = ctxErr
		}
		boundaryErr := fmt.Errorf("provider hard-abort boundary unavailable: %w", err)
		c.setProviderBoundaryState(lease, false, boundaryErr)
		return boundaryErr
	}
	c.setProviderBoundaryState(lease, true, nil)
	// A successful proxy start re-establishes the provider execution boundary.
	// Runtime show/ps/observed evidence may describe the previous boundary, so
	// invalidate it before any sidecar or provider-bound work can begin.
	if c.modelProfileRuntime != nil {
		c.modelProfileRuntime.InvalidateProviders(c.providerManager.EffectiveProviderRefs())
	}
	if err := invocationContextError(ctx); err != nil {
		// Startup raced with cancellation. Marking the boundary live before
		// entering the shared abort gate lets that gate synchronously reap the
		// just-started proxy before this invocation returns.
		if abortErr := c.abortProviderExecutionBoundaryLocked(); abortErr != nil {
			return err
		}
		return err
	}
	// Skill-pattern analysis is an invocation-owned model consumer. It is
	// deliberately wired only after the proxy is live; NewCoordinator must not
	// construct this sidecar against the direct provider.
	if c.skillDetector != nil && c.sidecarModel != "" && c.agentPool != nil {
		if s := c.AgentPool().Sidecar(); s != nil {
			c.skillDetector.SetSidecar(s)
		}
	}
	return nil
}

func (c *Coordinator) abortProviderExecutionBoundary() error {
	if c == nil || c.providerManager == nil {
		return nil
	}
	c.providerBoundaryLifecycleMu.Lock()
	defer c.providerBoundaryLifecycleMu.Unlock()
	return c.abortProviderExecutionBoundaryLocked()
}

func (c *Coordinator) abortProviderExecutionBoundaryLocked() error {
	lease := c.activeInvocationLease()
	started, _ := c.providerBoundaryState(lease)
	if !started {
		return nil
	}
	var err error
	if c.providerBoundaryAbort != nil {
		err = c.providerBoundaryAbort()
	} else {
		err = c.providerManager.AbortInvocationProxy()
	}
	// Abort is terminal for this invocation. Mark the boundary stopped only
	// after the manager has synchronously killed/reaped its owners, so a later
	// invocation cannot race a new proxy against an old process group.
	c.setProviderBoundaryState(lease, false, nil)
	return err
}

func (c *Coordinator) stopProviderExecutionBoundary() error {
	if c == nil || c.providerManager == nil {
		return nil
	}
	c.providerBoundaryLifecycleMu.Lock()
	defer c.providerBoundaryLifecycleMu.Unlock()
	return c.stopProviderExecutionBoundaryLocked()
}

func (c *Coordinator) stopProviderExecutionBoundaryLocked() error {
	lease := c.activeInvocationLease()
	started, _ := c.providerBoundaryState(lease)
	if !started {
		return nil
	}
	err := c.providerManager.StopInvocationProxy()
	c.setProviderBoundaryState(lease, false, nil)
	return err
}

func (c *Coordinator) providerBoundaryState(lease *invocationLease) (bool, error) {
	if lease != nil {
		lease.boundaryMu.Lock()
		defer lease.boundaryMu.Unlock()
		return lease.started, lease.err
	}
	c.providerBoundaryMu.Lock()
	defer c.providerBoundaryMu.Unlock()
	return c.providerBoundaryStarted, c.providerBoundaryErr
}

func (c *Coordinator) setProviderBoundaryState(lease *invocationLease, started bool, err error) {
	if lease != nil {
		lease.boundaryMu.Lock()
		lease.started = started
		lease.err = err
		lease.boundaryMu.Unlock()
	}
	c.providerBoundaryMu.Lock()
	c.providerBoundaryStarted = started
	c.providerBoundaryErr = err
	c.providerBoundaryMu.Unlock()
}

func invocationContextError(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// providerBoundaryRequired returns an explicit diagnostic for callers that
// try to run a real provider outside a public invocation. Tests and injected
// fantasy agents do not need this path; production provider execution does.
func (c *Coordinator) providerBoundaryRequired(ctx context.Context) error {
	if c == nil || c.providerManager == nil {
		return nil
	}
	// Construction helpers are also used by offline plan/phase tests and by
	// configuration inspection. They do not execute a provider call. The
	// public Run/Continue/RunDirectAgent paths install the watchdog before any
	// real agent is constructed, so actual invocation admission remains closed
	// unless the proxy can be started.
	if c.invocationWatchdog.Load() == nil {
		return nil
	}
	return c.startProviderExecutionBoundary(ctx)
}

// providerBoundaryReady is the construction-side admission check for every
// Hufu-owned auxiliary model. A real coordinator has a provider manager, so a
// sidecar must never be constructed against its direct upstream URL. Nil
// managers are retained for offline/unit-test coordinators that cannot make a
// provider call in the first place.
func (c *Coordinator) providerBoundaryReady() bool {
	if c == nil || c.providerManager == nil {
		return true
	}
	lease := c.activeInvocationLease()
	started, _ := c.providerBoundaryState(lease)
	return started
}
