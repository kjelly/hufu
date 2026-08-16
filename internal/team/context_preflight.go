package team

import (
	"fmt"
	"strings"
)

// PrepareContextPreflight makes a coordinator safe for a CLI-owned model
// invocation. It establishes the same session and event lineage used by a
// normal run before a sidecar can prepare/persist a manifest.
func (c *Coordinator) PrepareContextPreflight() error {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" || c.contextRepo == nil {
		return fmt.Errorf("context preflight requires a coordinator workspace and canonical repository")
	}
	if c.sessionData == nil {
		c.sessionData = NewSession()
	}
	// The preflight event-store run ID and every request/manifest must share
	// one stable identity. Do this before initEventStore rather than letting its
	// local timestamp fallback diverge from contextRunID().
	if c.executionRunID == "" {
		c.executionRunID = c.contextRunID()
	}
	if c.eventStore == nil {
		c.initEventStore()
	}
	if c.eventStore == nil || c.sessionData.RecoveryRequired {
		reason := "event store unavailable"
		if c.sessionData.RecoveryReason != "" {
			reason = c.sessionData.RecoveryReason
		}
		return fmt.Errorf("context preflight failed: %s", reason)
	}
	return nil
}

// CloseContextPreflight releases preflight-only resources after a CLI model
// invocation. It never mutates the persisted lineage.
func (c *Coordinator) CloseContextPreflight() {
	if c == nil {
		return
	}
	if c.eventStore != nil {
		_ = c.eventStore.Close()
		c.eventStore = nil
	}
	if closer, ok := c.contextRepo.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}
