package team

import (
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/audit"
)

// Close releases resources created and owned by the coordinator. Callers must
// wait for any public invocation to return before calling Close. Close is safe
// to call concurrently and repeatedly; every caller observes the same result.
// Resources supplied to NewCoordinator by the caller are not closed.
func (c *Coordinator) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.closeOwnedResources()
	})
	return c.closeErr
}

func (c *Coordinator) closeOwnedResources() error {
	var closeErrs []error
	if err := c.closeContextPreflight(); err != nil {
		closeErrs = append(closeErrs, err)
	}

	c.terminalControlMu.Lock()
	broker := c.terminalBroker
	c.terminalBroker = nil
	c.ptyTerminalEnabled = false
	c.terminalControlMu.Unlock()
	if broker != nil {
		if err := broker.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close terminal broker: %w", err))
		}
	}

	if c.eventStore != nil {
		if err := c.eventStore.Close(); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("close event store: %w", err))
		}
		c.eventStore = nil
		c.SetEventJournal(eventStoreJournal{})
	}
	if c.journal != nil {
		if err := c.journal.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
		c.journal = nil
	}
	if err := c.closeContextRepository(); err != nil {
		closeErrs = append(closeErrs, err)
	}
	if err := c.closeAuditLogger(); err != nil {
		closeErrs = append(closeErrs, err)
	}
	return errors.Join(closeErrs...)
}

func (c *Coordinator) closeContextRepository() error {
	if c == nil || !c.ownsContextRepo || c.contextRepo == nil {
		return nil
	}
	c.contextRepoCloseOnce.Do(func() {
		if err := c.contextRepo.Close(); err != nil {
			c.contextRepoCloseErr = fmt.Errorf("close canonical context store: %w", err)
		}
	})
	return c.contextRepoCloseErr
}

func (c *Coordinator) closeAuditLogger() error {
	if c == nil || !c.ownsAuditLogger || c.auditLogger == nil {
		return nil
	}
	c.auditLoggerCloseOnce.Do(func() {
		audit.ClearDefault(c.auditLogger)
		if err := c.auditLogger.Close(); err != nil {
			c.auditLoggerCloseErr = fmt.Errorf("close audit logger: %w", err)
		}
	})
	return c.auditLoggerCloseErr
}
