package team

import (
	"fmt"
	"slices"
	"strings"
)

func (c *Coordinator) SetRunInputAssignments(assignments []RunInputAssignment) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runInputAssignments = cloneRunInputAssignments(assignments)
}

func (c *Coordinator) RunInputSnapshot() *RunInputSnapshot {
	if c == nil {
		return nil
	}
	var snapshot *RunInputSnapshot
	c.viewSessionData(func(session *SessionData) {
		snapshot = CloneRunInputSnapshot(session.RunInputSnapshot)
	})
	return snapshot
}

func (c *Coordinator) resolveRunInputsForInvocation(_ string) error {
	if c == nil || c.session == nil {
		return nil
	}
	c.mu.RLock()
	assignments := cloneRunInputAssignments(c.runInputAssignments)
	c.mu.RUnlock()
	if len(c.session.RunInputDefinitions) == 0 && len(assignments) == 0 {
		return nil
	}
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		return fmt.Errorf("run input resolution requires an active run identity")
	}
	snapshot, err := ResolveRunInputSnapshot(c.session.RunInputDefinitions, assignments, runID, runID+":invocation", c.session.Config.Name)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}
	if err := c.emitEvent(string(EventRunInputsResolved), "runtime", "", snapshot); err != nil {
		return fmt.Errorf("persist run_inputs_resolved event: %w", err)
	}
	return c.mutateSessionData(func(session *SessionData) error {
		session.RunInputSnapshot = CloneRunInputSnapshot(snapshot)
		return nil
	})
}

func (c *Coordinator) previewRunInputs() (*RunInputSnapshot, error) {
	if c == nil || c.session == nil {
		return nil, nil
	}
	c.mu.RLock()
	assignments := cloneRunInputAssignments(c.runInputAssignments)
	c.mu.RUnlock()
	return ResolveRunInputSnapshot(c.session.RunInputDefinitions, assignments, "dry-run", "dry-run:invocation", c.session.Config.Name)
}

func cloneRunInputAssignments(src []RunInputAssignment) []RunInputAssignment {
	clone := slices.Clone(src)
	for index := range clone {
		clone[index].RawValue = slices.Clone(src[index].RawValue)
	}
	return clone
}
