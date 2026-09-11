package team

import (
	"context"
	"encoding/json"
	"fmt"
)

// ValidateRequiredResourceLocks resolves, hash-locks, and durably persists
// every resource declared under team.yaml's required-resources (spec.md
// "Generic Required Resource Lock"). It is distinct from the existing
// ValidateResourceLocks (coordinator.go), which checks an unrelated
// mechanism — workspace directories and CapabilityRequirement probes — that
// happens to share a similar name; see spec.md §2.
//
// It is a no-op when the team declares no required resources (runtime
// invariant 10). On resume it compares the freshly resolved
// LockedResourceSet digest against the one already persisted for this
// session and fails closed on drift (invariant 6) instead of silently
// re-locking against a changed source.
//
// rootDir is the directory required-resource paths are resolved relative
// to; callers decide what that root should be. Wiring this method into the
// coordinator's pre-dispatch path — including which root to pass — is a
// deliberately deferred follow-up (spec.md §2/§10), not decided here.
func (c *Coordinator) ValidateRequiredResourceLocks(ctx context.Context, rootDir string) error {
	if c == nil || c.session == nil {
		return nil
	}
	specs := c.session.Config.RequiredResources
	if len(specs) == 0 {
		return nil
	}

	current, loaded, err := ResolveRequiredResources(rootDir, specs)
	if err != nil {
		return fmt.Errorf("required resource lock: %w", err)
	}

	var persisted *LockedResourceSet
	c.viewSessionData(func(sd *SessionData) {
		persisted = cloneLockedResourceSet(sd.RequiredResourceLockSet)
	})

	if persisted != nil {
		if persisted.Digest != current.Digest {
			return fmt.Errorf("required resource lock drift detected (persisted=%s current=%s)", persisted.Digest, current.Digest)
		}
		c.setLoadedRequiredResources(loaded)
		return nil
	}

	payload, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshal required resource lock set: %w", err)
	}
	if journal := c.EventJournal(); journal != nil {
		key := "required-resource-lock:" + current.Digest
		if _, err := journal.Append(ctx, RunEvent{
			Type: string(EventResourceLocked), Actor: "coordinator", IdempotencyKey: key, Payload: payload,
		}); err != nil {
			return fmt.Errorf("append resource_locked event: %w", err)
		}
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.RequiredResourceLockSet = current
		return nil
	}); err != nil {
		return fmt.Errorf("persist required resource lock set: %w", err)
	}
	if err := c.persistSession("persist required resource lock set"); err != nil {
		return err
	}
	c.setLoadedRequiredResources(loaded)
	return nil
}

func (c *Coordinator) setLoadedRequiredResources(loaded []*LoadedResource) {
	c.loadedRequiredResourcesMu.Lock()
	defer c.loadedRequiredResourcesMu.Unlock()
	c.loadedRequiredResources = loaded
}

// LoadedRequiredResources returns the in-memory content
// ValidateRequiredResourceLocks resolved for this run (invariant 11: never
// re-read from disk after that). It is nil until that method has run at
// least once.
func (c *Coordinator) LoadedRequiredResources() []*LoadedResource {
	c.loadedRequiredResourcesMu.Lock()
	defer c.loadedRequiredResourcesMu.Unlock()
	return append([]*LoadedResource(nil), c.loadedRequiredResources...)
}
