package improve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

// writeExecutionEvents records rows in workspace's event store as the
// canonical events the runtime writes for them, so the execution-event
// projection reproduces each row. Every run opens with the run_started the
// coordinator always records, and each terminal row adds the attempt receipt
// that carries its usage and model. A row the runtime could never produce is
// a fixture error rather than a silently different projection.
func writeExecutionEvents(tb testing.TB, workspace string, rows []team.ExecutionEvent) {
	tb.Helper()
	appendEventChain(tb, workspace, executionFixtureEvents(tb, rows))
}

// appendEventChain appends events to workspace's event store as schema v1
// records chained after its current tail. It writes without per-event sync,
// which keeps large benchmark fixtures fast; the result validates like any
// durable log.
func appendEventChain(tb testing.TB, workspace string, events []team.RunEvent) {
	tb.Helper()
	path := filepath.Join(workspace, "logs", "event_store.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	previousID, previousHash, count := eventChainTail(tb, path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		tb.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 256*1024)
	for i, event := range events {
		event.SchemaVersion = 1
		event.ID = fmt.Sprintf("fixture-%08d", count+i)
		event.PreviousID, event.PreviousHash = previousID, previousHash
		if event.SessionID == "" {
			event.SessionID = "fixture-session"
		}
		if len(event.Payload) > 0 {
			// The marshaled record carries the compacted payload; hash
			// exactly those bytes, as the event store does.
			var compact bytes.Buffer
			if err := json.Compact(&compact, event.Payload); err != nil {
				tb.Fatal(err)
			}
			event.Payload = compact.Bytes()
		}
		event.Hash = team.ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		data, err := json.Marshal(event)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			tb.Fatal(err)
		}
		previousID, previousHash = event.ID, event.Hash
	}
	if err := writer.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err := file.Close(); err != nil {
		tb.Fatal(err)
	}
}

func eventChainTail(tb testing.TB, path string) (id, hash string, count int) {
	tb.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", "", 0
	}
	if err != nil {
		tb.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "", "", 0
	}
	var last team.RunEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		tb.Fatal(err)
	}
	return last.ID, last.Hash, len(lines)
}

var executionFixtureEventTypes = map[string]team.EventType{
	"in_progress": team.EventTaskStarted,
	"planned":     team.EventTaskPlanned,
	"verifying":   team.EventTaskVerifying,
	"done":        team.EventTaskCompleted,
	"error":       team.EventTaskFailed,
	"skipped":     team.EventTaskSkipped,
}

func executionFixtureEvents(tb testing.TB, rows []team.ExecutionEvent) []team.RunEvent {
	tb.Helper()
	started := make(map[string]bool)
	runs := make(map[string]team.ExecutionEvent)
	receipts := make(map[string][]team.ExecutionReceipt)
	var events []team.RunEvent
	for _, row := range rows {
		// A run's rows take their team and revision from its run_started.
		if run, ok := runs[row.RunID]; ok && (run.Team != row.Team || run.TeamRevision != row.TeamRevision) {
			tb.Fatalf("fixture row has team %q revision %q, but run %q has team %q revision %q", row.Team, row.TeamRevision, row.RunID, run.Team, run.TeamRevision)
		}
		runs[row.RunID] = row
		if row.Status != "run_started" && !started[row.RunID] {
			events = append(events, executionFixtureEvent(tb, row, team.EventRunStarted, "coordinator", map[string]any{"team": row.Team, "team_revision": row.TeamRevision}))
		}
		switch row.Status {
		case "run_started":
			events = append(events, executionFixtureEvent(tb, row, team.EventRunStarted, "coordinator", map[string]any{"team": row.Team, "team_revision": row.TeamRevision}))
		case "run_finished":
			repairCost := row.RepairCost
			if repairCost.Attempts == 0 {
				repairCost.Attempts = row.RepairAttempts
			}
			events = append(events, executionFixtureEvent(tb, row, team.EventRunFinished, "coordinator", map[string]any{
				"team": row.Team, "outcome": row.Outcome, "acceptance_state": row.AcceptanceState,
				"evidence_manifest": map[string]any{"manifest_hash": row.EvidenceManifestHash},
				"telemetry":         map[string]any{"repair_cost": repairCost, "decision_chain": row.DecisionChain, "plan_revision": row.PlanRevision},
			}))
		default:
			eventType, ok := executionFixtureEventTypes[row.Status]
			if !ok {
				tb.Fatalf("fixture status %q has no canonical task event", row.Status)
			}
			payload := map[string]any{
				"id": row.TaskID, "status": row.Status, "agent": row.Agent, "attempt": row.Attempt,
				"model": row.Model, "source": row.TaskType, "skills": row.Skills, "phase": row.Phase,
			}
			key := row.RunID + "\x00" + row.TaskID
			switch row.Status {
			case "done", "error", "planned":
				usage := row.Usage
				receipts[key] = append(receipts[key], team.ExecutionReceipt{RunID: row.RunID, TaskID: row.TaskID, Attempt: row.Attempt, Usage: &usage, Model: row.Model})
				payload["execution_receipts"] = receipts[key]
			default:
				if row.Usage != (team.ExecutionUsage{}) {
					tb.Fatalf("fixture row reports usage on status %q, which the runtime only records on terminal rows: %+v", row.Status, row)
				}
			}
			events = append(events, executionFixtureEvent(tb, row, eventType, row.Agent, payload))
		}
		started[row.RunID] = true
	}
	return events
}

func executionFixtureEvent(tb testing.TB, row team.ExecutionEvent, eventType team.EventType, actor string, payload map[string]any) team.RunEvent {
	tb.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		tb.Fatal(err)
	}
	taskID := row.TaskID
	if eventType == team.EventRunStarted || eventType == team.EventRunFinished {
		taskID = ""
	}
	return team.RunEvent{
		RunID: row.RunID, TaskID: taskID, Actor: actor, Type: string(eventType),
		Timestamp: row.Timestamp, InvocationID: row.InvocationID, Payload: data,
	}
}

// projectedFixtureRows counts the run-scoped execution rows rows project to.
func projectedFixtureRows(tb testing.TB, rows []team.ExecutionEvent) int64 {
	tb.Helper()
	projector := team.NewExecutionEventProjector()
	for _, event := range executionFixtureEvents(tb, rows) {
		projector.Add(event)
	}
	var count int64
	for _, row := range projector.Rows() {
		if row.RunID != "" {
			count++
		}
	}
	return count
}
