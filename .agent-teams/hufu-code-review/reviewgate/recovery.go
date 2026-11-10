package main

import (
	// The embedded interpreter exports encoding/json, not encoding/json/v2.
	"encoding/json"
	"fmt"
)

// Recovery grants come from the action invocation's runtime environment,
// never from a checkpoint field or a model's proposed report.
func recoveryOrigins(encoded string) (map[string]string, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > 64<<10 {
		return nil, fmt.Errorf("runtime recovery origins exceed 64 KiB")
	}
	var origins map[string]string
	if err := json.Unmarshal([]byte(encoded), &origins); err != nil {
		return nil, fmt.Errorf("invalid runtime recovery origins: %w", err)
	}
	return origins, nil
}

func (current session) acceptsReceiptOrigin(item task, runID string) bool {
	if item.Receipt == nil || runID == "" || item.Receipt.RunID == "" || item.Receipt.TaskID != item.ID {
		return false
	}
	return item.Receipt.RunID == runID ||
		(current.RecoveredTaskRuns[item.ID] == item.Receipt.RunID && current.SnapshotID != "" && item.SnapshotID == current.SnapshotID)
}
