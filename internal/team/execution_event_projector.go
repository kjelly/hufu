package team

import "encoding/json"

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
// the model the latest of them reports, so a fanout parent sums its leaves.
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
	// receipts counts this run's receipts already attributed to a row.
	receipts int
	// lastRow indexes the task's latest row, or -1 before the first.
	lastRow int
}

// receiptTelemetry is the part of an ExecutionReceipt the projection reads.
type receiptTelemetry struct {
	RunID string          `json:"run_id"`
	Usage *ExecutionUsage `json:"usage"`
	Model string          `json:"model"`
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
	if mapped.Status == string(TaskSkipped) {
		// A skip runs no model; reporting the task's configured one would
		// credit it with an attempt that never happened.
		mapped.Model = ""
	}

	key := mapped.RunID + "\x00" + mapped.TaskID
	lifecycle := p.lifecycles[key]
	if lifecycle == nil {
		lifecycle = &projectedTaskLifecycle{lastRow: -1}
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
			lifecycle.attributeReceipts(&p.rows[lifecycle.lastRow], event)
		}
		return
	}
	lifecycle.status, lifecycle.attempt = mapped.Status, mapped.Attempt
	if terminal {
		lifecycle.attributeReceipts(&mapped, event)
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

// attributeReceipts adds to row the usage of the run's receipts that event
// carries beyond those already attributed, and takes the model of the latest.
func (l *projectedTaskLifecycle) attributeReceipts(row *ExecutionEvent, event RunEvent) {
	var payload struct {
		Receipts []receiptTelemetry `json:"execution_receipts"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return
	}
	var runReceipts []receiptTelemetry
	for _, receipt := range payload.Receipts {
		if receipt.RunID == row.RunID {
			runReceipts = append(runReceipts, receipt)
		}
	}
	if len(runReceipts) <= l.receipts {
		return
	}
	for _, receipt := range runReceipts[l.receipts:] {
		if receipt.Usage != nil {
			row.Usage = addExecutionUsage(row.Usage, *receipt.Usage)
		}
		if receipt.Model != "" {
			row.Model = receipt.Model
		}
	}
	l.receipts = len(runReceipts)
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
