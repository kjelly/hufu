package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// HistoricalResultID addresses a persisted terminal event, independently of
// workspace location or the branch through which an inherited event is read.
// An ID is a locator, never permission to read the event or its content.
func HistoricalResultID(event RunEvent) string {
	if event.ID == "" || !IsTerminalEvent(event.Type) {
		return ""
	}
	identity := fmt.Sprintf("hufu-result-v1\x00%s\x00%s\x00%s\x00%s\x00%s", event.ID, event.SessionID, event.RunID, event.TaskID, event.Type)
	digest := sha256.Sum256([]byte(identity))
	return "res-" + hex.EncodeToString(digest[:])
}

// HistoricalResultCommand quotes the team selector as shell data, so a copied
// reference cannot turn a configured display name into executable shell text.
func HistoricalResultCommand(resultID, teamName string) string {
	command := "hufu results show " + resultID
	if teamName != "" {
		command += " --team '" + strings.ReplaceAll(teamName, "'", "'\\''") + "'"
	}
	return command
}

func (c *Coordinator) reportHistoricalResult(event RunEvent) {
	if event.TaskID == "" || HistoricalResultID(event) == "" {
		return
	}
	var payload struct {
		Attempt int `json:"attempt"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	c.report(c.newEvent("task_result_ready").withTodoID(event.TaskID).withAgent(event.Actor).withMessage("saved task result").withData(map[string]any{
		"schema_version": 1, "result_id": HistoricalResultID(event), "invocation_id": event.InvocationID,
		"run_id": event.RunID, "task_id": event.TaskID, "attempt": max(event.Attempt, payload.Attempt),
		"branch_id": event.BranchID, "source_event_id": event.ID,
	}))
}
