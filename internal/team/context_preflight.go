package team

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ContextPreflightReentryError identifies an overlapping preflight on one
// coordinator. A second preflight must fail instead of replacing the first
// invocation's provider owner and lease.
type ContextPreflightReentryError struct{}

func (*ContextPreflightReentryError) Error() string { return "context preflight is already active" }

// PrepareContextPreflight preserves the context-free API for callers that do
// not have a cancellation scope. CLI-owned callers should use
// PrepareContextPreflightContext so cancellation owns the provider boundary.
func (c *Coordinator) PrepareContextPreflight() error {
	return c.PrepareContextPreflightContext(context.Background())
}

// PrepareContextPreflightContext makes a coordinator safe for a CLI-owned model
// invocation. It establishes the same session and event lineage used by a
// normal run before a sidecar can prepare/persist a manifest. When a sidecar
// is configured, parent owns the cancellation of a scoped provider boundary;
// cancellation aborts and reaps the proxy even if the model ignores context.
func (c *Coordinator) PrepareContextPreflightContext(parent context.Context) error {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" || c.contextRepo == nil {
		return fmt.Errorf("context preflight requires a coordinator workspace and canonical repository")
	}
	if parent == nil {
		parent = context.Background()
	}
	c.preflightMu.Lock()
	preflightActive := c.preflightStarting || c.preflightLease != nil
	if !preflightActive {
		c.preflightStarting = true
	}
	c.preflightMu.Unlock()
	if preflightActive {
		return &ContextPreflightReentryError{}
	}
	lease, err := c.acquireInvocationLease(parent)
	if err != nil {
		c.preflightMu.Lock()
		c.preflightStarting = false
		c.preflightMu.Unlock()
		return err
	}
	releaseLease := true
	defer func() {
		if !releaseLease {
			return
		}
		c.preflightMu.Lock()
		c.preflightStarting = false
		c.preflightMu.Unlock()
		lease.release()
	}()
	_ = c.mutateSessionData(func(sd *SessionData) error { return nil })
	// The preflight event-store run ID and every request/manifest must share
	// one stable identity. Do this before initEventStore rather than letting its
	// local timestamp fallback diverge from contextRunID().
	if c.executionRunID == "" {
		c.executionRunID = c.contextRunID()
	}
	if c.eventStore == nil {
		c.initEventStore()
	}
	sessionData := c.SessionData()
	if c.eventStore == nil || (sessionData != nil && sessionData.RecoveryRequired) {
		reason := "event store unavailable"
		if sessionData != nil && sessionData.RecoveryReason != "" {
			reason = sessionData.RecoveryReason
		}
		return fmt.Errorf("context preflight failed: %s", reason)
	}
	c.preflightMu.Lock()
	if c.preflightLease != nil {
		c.preflightMu.Unlock()
		return &ContextPreflightReentryError{}
	}
	c.preflightLease = lease
	c.preflightStarting = false
	c.preflightMu.Unlock()
	releaseLease = false
	// A preflight sidecar is still a Hufu-owned model invocation. Establish a
	// scoped provider owner before callers can resolve the shared sidecar. Teams
	// without a configured sidecar do not need to start a provider boundary and
	// retain the deterministic fallback paths used by the CLI.
	if c.sidecarModel != "" {
		invocationCtx := c.beginContextPreflight(parent, lease)
		if err := c.startProviderExecutionBoundary(invocationCtx); err != nil {
			c.CloseContextPreflight()
			return fmt.Errorf("context preflight provider boundary unavailable: %w", err)
		}
		if err := invocationCtx.Err(); err != nil {
			c.CloseContextPreflight()
			return fmt.Errorf("context preflight cancelled: %w", err)
		}
	}
	return nil
}

// CloseContextPreflight releases preflight-only resources after a CLI model
// invocation. It never mutates the persisted lineage.
func (c *Coordinator) CloseContextPreflight() {
	if c == nil {
		return
	}
	c.preflightMu.Lock()
	c.preflightContext = nil
	owner := c.preflightOwner
	c.preflightOwner = nil
	lease := c.preflightLease
	c.preflightLease = nil
	c.preflightMu.Unlock()
	if owner != nil {
		if err := owner.close(); err != nil {
			log.Printf("warning: close context preflight provider boundary: %v", err)
		}
		if owner.watchdog != nil {
			owner.watchdog.wait()
			c.invocationWatchdog.CompareAndSwap(owner.watchdog, nil)
		}
	}
	if lease == nil {
		return
	}
	if c.eventStore != nil {
		_ = c.eventStore.Close()
		c.eventStore = nil
	}
	if closer, ok := c.contextRepo.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	lease.release()
}

// ContextPreflight returns the live context owned by the current CLI
// preflight scope. Callers use it for the actual sidecar operation so watchdog
// cancellation and caller cancellation share one context boundary.
func (c *Coordinator) ContextPreflight() context.Context {
	if c == nil {
		return context.Background()
	}
	c.preflightMu.Lock()
	ctx := c.preflightContext
	c.preflightMu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// beginContextPreflight installs a Hufu-owned cancellation scope around a
// CLI sidecar call. The owner goroutine is joined by CloseContextPreflight;
// it is not a detached timeout or cleanup fallback.
func (c *Coordinator) beginContextPreflight(parent context.Context, lease *invocationLease) context.Context {
	owner := newInvocationOwnerWithLease(c, parent, lease)
	watchdog := c.newInvocationWatchdog(owner.ctx, owner.cancel)
	watchdog.owner = owner
	owner.watchdog = watchdog
	c.preflightMu.Lock()
	c.preflightContext = owner.ctx
	c.preflightOwner = owner
	c.preflightMu.Unlock()
	c.setInvocationWatchdog(watchdog)
	owner.start()
	watchdog.start()
	return owner.ctx
}
