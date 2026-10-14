package main

import (
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

type resultReadyIdentity struct {
	SchemaVersion int    `json:"schema_version"`
	ResultID      string `json:"result_id"`
	InvocationID  string `json:"invocation_id,omitempty"`
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	Attempt       int    `json:"attempt"`
	BranchID      string `json:"branch_id"`
	SourceEventID string `json:"source_event_id"`
}

func resultStatusIdentity(event team.StatusEvent) *resultReadyIdentity {
	if event.Type != "task_result_ready" {
		return nil
	}
	text := func(key string) string { value, _ := event.Data[key].(string); return value }
	id := text("result_id")
	if !strings.HasPrefix(id, "res-") || len(id) != 68 {
		return nil
	}
	attempt, _ := event.Data["attempt"].(int)
	return &resultReadyIdentity{SchemaVersion: 1, ResultID: id, InvocationID: text("invocation_id"), RunID: text("run_id"),
		TaskID: event.TodoID, Attempt: attempt, BranchID: text("branch_id"), SourceEventID: text("source_event_id")}
}
