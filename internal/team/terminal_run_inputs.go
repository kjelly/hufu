package team

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
)

// selectRecoveryRunInputSnapshot records a successful frozen-input admission
// for this invocation. Merely loading an old session does not select its inputs
// for a new request that might fail before input resolution completes.
func (c *Coordinator) selectRecoveryRunInputSnapshot(snapshot *RunInputSnapshot) error {
	if err := c.mutateSessionData(func(session *SessionData) error {
		session.ActiveRunInputSnapshotID = snapshot.ID
		return nil
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.runInputSelectionRunID = c.executionRunID
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) terminalRunInputs(runID string) (*RunInputSnapshot, string) {
	snapshot := c.RunInputSnapshot()
	if snapshot == nil || snapshot.RunID == runID {
		return snapshot, ""
	}
	c.mu.RLock()
	selected := c.runInputSelectionRunID == runID && runID != ""
	c.mu.RUnlock()
	if !selected {
		return nil, ""
	}
	// Keep the exact original snapshot and hashes. The terminal event belongs
	// to this recovery invocation; its explicitly selected inputs belong to
	// the original task run and remain pinned there.
	return snapshot, snapshot.RunID
}

// validateTerminalInputOrigins binds recovery terminal inputs to an exact
// preceding snapshot in the selected branch's lineage, including its hash.
func validateTerminalInputOrigins(events []RunEvent) error {
	snapshots := make(map[string]RunInputSnapshot)
	for _, event := range events {
		switch event.Type {
		case string(EventRunInputsResolved):
			var snapshot RunInputSnapshot
			if json.Unmarshal(event.Payload, &snapshot) == nil && snapshot.RunID == event.RunID && ValidateRunInputSnapshot(&snapshot) == nil {
				snapshots[snapshot.ID] = snapshot
			}
		case "run_finished":
			// Legacy terminal events may have no structured payload. They
			// cannot declare an input origin and retain their existing replay.
			if len(event.Payload) == 0 {
				continue
			}
			var payload RunFinishedEventPayload
			if err := json.Unmarshal(event.Payload, &payload, jsonv1.DefaultOptionsV1()); err != nil {
				return err
			}
			if payload.RunInputOriginRunID == "" {
				continue
			}
			if err := validateRunFinishedEventPayload(event); err != nil {
				return err
			}
			source, ok := snapshots[payload.RunInputs.ID]
			if !ok || source.RunID != payload.RunInputOriginRunID || source.SnapshotHash != payload.RunInputs.SnapshotHash {
				return fmt.Errorf("recovery terminal inputs have no matching source in the branch lineage")
			}
		}
	}
	return nil
}

func (c *Coordinator) validateTerminalInputOriginForAppend(event RunEvent) error {
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return err
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return err
	}
	lineage := FilterEventsForBranch(events, tree, event.BranchID)
	return validateTerminalInputOrigins(append(lineage, event))
}
