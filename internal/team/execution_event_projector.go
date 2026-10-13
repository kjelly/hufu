package team

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// ExecutionEventProjector derives the execution-event telemetry stream from
// canonical run events. Add events in log order; Rows returns the stream.
// It holds only per-run and per-task lifecycle state, so a whole event log
// can be streamed through it in one pass.
//
// The event store records every durable state transition, while the
// execution-event stream records one row each time a task enters a status
// within an attempt. A task's repeated durable transitions collapse into the
// status it is already in, but a status the task re-enters after another one
// is kept (an approved --plan task starting again after planned). Attempts
// follow the in-dispatch counter the attempt-starting task_started event
// carries; logs without that counter fall back to the task's retry count.
//
// Task rows take their team and team revision from the run's run_started
// event. A terminal row (done, error, planned) carries the token usage of the
// attempt receipts that appeared since the task's previous terminal row, and
// the model the latest of them reports. A task's receipt list is not
// append-only (a new dispatch replaces the receipt of the same attempt
// number), so receipts are told apart by identity, not by position. A model
// fanout parent sums its leaves' receipts but reports its own model, and a
// runtime action reports none, as the execution telemetry always has.
type ExecutionEventProjector struct {
	runs       map[string]projectedRun
	lifecycles map[string]*projectedTaskLifecycle
	rows       []ExecutionEvent
}

type projectedRun struct {
	team         string
	teamRevision string
}

type projectedTaskLifecycle struct {
	agent           string
	taskType        string
	skills          []string
	dispatchAttempt int
	status          string
	attempt         int
	// attributed holds the identities of receipts already attributed.
	attributed map[string]bool
	// lastRow indexes the task's latest row, or -1 before the first.
	lastRow int
}

// receiptTelemetry is the part of an ExecutionReceipt the projection reads.
type receiptTelemetry struct {
	RunID             string          `json:"run_id"`
	Attempt           int             `json:"attempt"`
	OccurrenceAttempt int             `json:"occurrence_attempt"`
	StartedAt         string          `json:"started_at"`
	FinishedAt        string          `json:"finished_at"`
	ModelExecutionID  string          `json:"model_execution_id"`
	ProducerID        string          `json:"producer_id"`
	Usage             *ExecutionUsage `json:"usage"`
	Model             string          `json:"model"`
}

func (r receiptTelemetry) identity() string {
	return strings.Join([]string{r.RunID, strconv.Itoa(r.Attempt), strconv.Itoa(r.OccurrenceAttempt), r.StartedAt, r.FinishedAt, r.ModelExecutionID, r.ProducerID}, "\x00")
}

// executionTaskShape is the part of a task transition payload that decides
// which model a row reports.
type executionTaskShape struct {
	Action            json.RawMessage    `json:"action"`
	ModelTopology     []string           `json:"model_topology"`
	ExecutionTopology []json.RawMessage  `json:"execution_topology"`
	Receipts          []receiptTelemetry `json:"execution_receipts"`
}

// runtimeAction reports whether the task runs a structured action rather
// than a model.
func (s executionTaskShape) runtimeAction() bool {
	action := bytes.TrimSpace(s.Action)
	return len(action) > 0 && !bytes.Equal(action, []byte("null"))
}

// modelFanout reports whether the task fans out across several models, the
// test the runtime applies (len(ModelTopology) > 1 || len(ExecutionTopology) > 1).
func (s executionTaskShape) modelFanout() bool {
	return len(s.ModelTopology) > 1 || len(s.ExecutionTopology) > 1
}

// NewExecutionEventProjector returns an empty projector.
func NewExecutionEventProjector() *ExecutionEventProjector {
	return &ExecutionEventProjector{
		runs:       make(map[string]projectedRun),
		lifecycles: make(map[string]*projectedTaskLifecycle),
	}
}

// Add projects event. Events that map to no execution-event row are ignored.
func (p *ExecutionEventProjector) Add(event RunEvent) {
	mapped, ok := ExecutionEventFromRunEvent(event)
	if !ok {
		return
	}
	run := p.runs[mapped.RunID]
	switch mapped.Status {
	case "run_started":
		p.runs[mapped.RunID] = projectedRun{team: mapped.Team, teamRevision: mapped.TeamRevision}
		p.rows = append(p.rows, mapped)
		return
	case "run_finished":
		if mapped.Team == "" {
			mapped.Team = run.team
		}
		mapped.TeamRevision = run.teamRevision
		p.rows = append(p.rows, mapped)
		return
	}
	mapped.Team, mapped.TeamRevision = run.team, run.teamRevision
	var shape executionTaskShape
	_ = json.Unmarshal(event.Payload, &shape)
	if mapped.Status == string(TaskSkipped) || shape.runtimeAction() {
		// A skip runs no model, and a runtime action runs a provider
		// command; reporting the task's configured model would credit it
		// with a model attempt that never happened.
		mapped.Model = ""
	}

	key := mapped.RunID + "\x00" + mapped.TaskID
	lifecycle := p.lifecycles[key]
	if lifecycle == nil {
		lifecycle = &projectedTaskLifecycle{lastRow: -1, attributed: make(map[string]bool)}
		p.lifecycles[key] = lifecycle
	}
	if event.Type == string(EventExecutionFallbackDecided) && mapped.Agent == "execution-route" && lifecycle.agent != "" {
		mapped.Agent = lifecycle.agent
	}
	if mapped.Agent != "" {
		lifecycle.agent = mapped.Agent
	}
	if event.Type == string(EventExecutionFallbackDecided) {
		// A fallback decision carries no task transition payload; the
		// failed attempt belongs to the task as its transitions describe it.
		mapped.TaskType, mapped.Skills = lifecycle.taskType, lifecycle.skills
	} else {
		lifecycle.taskType, lifecycle.skills = mapped.TaskType, mapped.Skills
	}
	if attempt := eventDispatchAttempt(event); attempt > 0 {
		lifecycle.dispatchAttempt = attempt
	}
	if lifecycle.dispatchAttempt > 0 {
		mapped.Attempt = lifecycle.dispatchAttempt
	}
	terminal := isTerminalExecutionStatus(mapped.Status)
	if mapped.Status == lifecycle.status && mapped.Attempt == lifecycle.attempt {
		// A repeated durable transition can carry a receipt the first one
		// did not; it belongs to the row already emitted.
		if terminal && lifecycle.lastRow >= 0 {
			lifecycle.attributeReceipts(&p.rows[lifecycle.lastRow], shape)
		}
		return
	}
	lifecycle.status, lifecycle.attempt = mapped.Status, mapped.Attempt
	if terminal {
		lifecycle.attributeReceipts(&mapped, shape)
	}
	p.rows = append(p.rows, mapped)
	lifecycle.lastRow = len(p.rows) - 1
}

// Rows returns the projected stream so far. The caller must not modify it.
func (p *ExecutionEventProjector) Rows() []ExecutionEvent {
	return p.rows
}

func isTerminalExecutionStatus(status string) bool {
	switch status {
	case "done", "error", "planned":
		return true
	default:
		return false
	}
}

// attributeReceipts adds to row the usage of the run's receipts in shape that
// no earlier row was attributed, and takes the model of the latest of them
// unless the task fans out or runs an action.
func (l *projectedTaskLifecycle) attributeReceipts(row *ExecutionEvent, shape executionTaskShape) {
	for _, receipt := range shape.Receipts {
		if receipt.RunID != row.RunID {
			continue
		}
		identity := receipt.identity()
		if l.attributed[identity] {
			continue
		}
		l.attributed[identity] = true
		if receipt.Usage != nil {
			row.Usage = addExecutionUsage(row.Usage, *receipt.Usage)
		}
		if receipt.Model != "" && !shape.modelFanout() && !shape.runtimeAction() {
			row.Model = receipt.Model
		}
	}
}

func addExecutionUsage(total, usage ExecutionUsage) ExecutionUsage {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.TotalTokens += usage.TotalTokens
	total.ProgressTokens += usage.ProgressTokens
	total.CacheReadTokens += usage.CacheReadTokens
	total.CacheCreationTokens += usage.CacheCreationTokens
	return total
}
