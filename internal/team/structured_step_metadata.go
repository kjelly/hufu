package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
)

// StructuredStepProjectionEvents retains receipt sources from the caller's
// canonical task replay, while keeping profile/cost events needed to verify
// them. A fresh task_created starts a new occurrence; retry/resume retains it.
func StructuredStepProjectionEvents(lineage, taskEvents []RunEvent) ([]RunEvent, error) {
	type occurrence struct {
		run  string
		refs []string
	}
	tasks := make(map[string]*occurrence)
	for _, event := range taskEvents {
		if event.Type != string(EventTaskCreated) && event.Type != string(EventStructuredStepReceiptRecorded) {
			continue
		}
		id := event.TaskID
		var creation struct {
			ID     string `json:"id"`
			Retry  bool   `json:"reset_for_retry"`
			Resume bool   `json:"reset_for_resume"`
		}
		if event.Type == string(EventTaskCreated) {
			if err := json.Unmarshal(event.Payload, &creation); err != nil {
				return nil, err
			}
			if id == "" {
				id = creation.ID
			}
		}
		if id == "" {
			continue
		}
		state := tasks[id]
		if state == nil || (event.Type == string(EventTaskCreated) && state.run != event.RunID && !creation.Retry && !creation.Resume) {
			state = &occurrence{run: event.RunID}
			tasks[id] = state
		}
		if event.Type == string(EventStructuredStepReceiptRecorded) {
			state.refs = append(state.refs, event.ID)
		}
	}
	allowed := make(map[string]bool)
	for _, state := range tasks {
		for _, id := range state.refs {
			allowed[id] = true
		}
	}
	result := make([]RunEvent, 0, len(lineage))
	for _, event := range lineage {
		if event.Type != string(EventStructuredStepReceiptRecorded) || allowed[event.ID] {
			result = append(result, event)
		}
	}
	return result, nil
}

func validateStructuredStepSummary(summary StructuredStepSummary, receiptID string) error {
	if summary.ReceiptID != receiptID || summary.StepID == "" || summary.Tool == "" || summary.RepairAttempt < 0 || summary.ExitCode == nil {
		return fmt.Errorf("invalid structured step summary identity")
	}
	switch summary.ExecutionKind {
	case "tool_call", "unknown":
		if len(summary.ModelExecutionIDs) != 0 {
			return fmt.Errorf("non-generative summary claims model invocations")
		}
	case "model_generation":
		if len(summary.ModelExecutionIDs) == 0 {
			return fmt.Errorf("generative summary has no invocation evidence")
		}
	default:
		return fmt.Errorf("invalid structured step summary execution kind")
	}
	seen := make(map[string]bool)
	for _, id := range summary.ModelExecutionIDs {
		if id == "" || strings.TrimSpace(id) != id || seen[id] {
			return fmt.Errorf("invalid or duplicate summary invocation identity")
		}
		seen[id] = true
	}
	return nil
}

// StructuredStepSummary is a runtime projection of the complete CAS receipt.
// It contains no tool input, output, diagnostic text, or duplicate target.
type StructuredStepSummary struct {
	ReceiptID         string   `json:"receipt_id"`
	StepID            string   `json:"step_id,omitempty"`
	Tool              string   `json:"tool,omitempty"`
	ExecutionKind     string   `json:"execution_kind"`
	ProposedModel     string   `json:"proposed_model,omitempty"`
	ModelExecutionIDs []string `json:"model_execution_ids,omitempty"`
	RepairAttempt     int      `json:"repair_attempt,omitzero"`
	ExitCode          *int     `json:"exit_code"`
}

type StructuredStepMetadata struct {
	StructuredStepSummary
	RunID              string               `json:"run_id,omitempty"`
	BranchID           string               `json:"branch_id,omitempty"`
	TaskID             string               `json:"task_id"`
	Attempt            int                  `json:"attempt,omitzero"`
	OccurrenceRevision int                  `json:"occurrence_revision,omitzero"`
	SourceEventID      string               `json:"source_event_id,omitempty"`
	SourceHash         string               `json:"source_hash,omitempty"`
	ReceiptSHA256      string               `json:"receipt_sha256,omitempty"`
	Invocations        []StepModelExecution `json:"invocations"`
	UnavailableReason  string               `json:"unavailable_reason,omitempty"`
}

func summarizeStructuredStep(receipt ExecutionStepReceipt) StructuredStepSummary {
	return StructuredStepSummary{ReceiptID: receipt.ID, StepID: receipt.StepID, Tool: receipt.Tool,
		ExecutionKind: receipt.EffectiveExecutionKind(), ProposedModel: receipt.ProposedModel,
		ModelExecutionIDs: slices.Clone(receipt.ModelExecutionIDs), RepairAttempt: receipt.RepairAttempt, ExitCode: new(receipt.ExitCode)}
}

func sameStructuredStepSummary(a, b StructuredStepSummary) bool {
	return a.ReceiptID == b.ReceiptID && a.StepID == b.StepID && a.Tool == b.Tool && a.ExecutionKind == b.ExecutionKind &&
		a.ProposedModel == b.ProposedModel && slices.Equal(a.ModelExecutionIDs, b.ModelExecutionIDs) && a.RepairAttempt == b.RepairAttempt &&
		a.ExitCode != nil && b.ExitCode != nil && *a.ExitCode == *b.ExitCode
}

func (row StructuredStepEvidence) Metadata() StructuredStepMetadata {
	r := row.Receipt
	return StructuredStepMetadata{StructuredStepSummary: summarizeStructuredStep(r), RunID: r.RunID, BranchID: r.BranchID,
		TaskID: r.TaskID, Attempt: r.Attempt, OccurrenceRevision: r.OccurrenceRevision, SourceEventID: row.SourceEventID,
		SourceHash: row.SourceHash, ReceiptSHA256: row.ReceiptSHA256, Invocations: slices.Clone(row.Invocations)}
}

func indexStructuredStepEvidence(events []RunEvent) (map[stepReceiptScopeKey][]stepInvocationEvidence, cost.Projection, error) {
	ledger, _, err := ProjectCostLedger(events)
	if err != nil {
		return nil, ledger, err
	}
	profiles := make(map[string]RunEvent)
	observed := make(map[stepReceiptScopeKey][]stepInvocationEvidence)
	invocations := make(map[string]bool)
	receipts := make(map[stepReceiptScopeKey]bool)
	for _, event := range events {
		switch EventType(event.Type) {
		case EventModelProfileResolved:
			profiles[event.ID] = event
		case EventStructuredStepInvocationObserved:
			entry, err := readStepInvocationEvidence(event, profiles)
			if err != nil {
				return nil, ledger, err
			}
			binding := stepInvocationSourceKey(event, entry.payload)
			key := event.RunID + "\x00" + event.BranchID + "\x00" + entry.payload.ModelExecutionID
			if receipts[binding] || invocations[key] {
				return nil, ledger, fmt.Errorf("duplicate or late structured step invocation binding")
			}
			invocations[key] = true
			observed[binding] = append(observed[binding], entry)
		case EventStructuredStepReceiptRecorded:
			p, err := decodeStructuredStepReceiptEvent(event)
			if err != nil {
				return nil, ledger, err
			}
			key := stepReceiptScopeKey{runID: event.RunID, branchID: event.BranchID, taskID: event.TaskID, attempt: event.Attempt, revision: p.OccurrenceRevision, receiptID: p.ReceiptID}
			if receipts[key] {
				return nil, ledger, fmt.Errorf("duplicate structured step receipt source")
			}
			receipts[key] = true
		}
	}
	return observed, ledger, nil
}

// ProjectStructuredStepMetadata is pure: immutable report capture can run it
// inside the terminal transaction without reading CAS or mutable checkpoints.
// Live inspect additionally verifies the complete CAS bytes through the loader.
func ProjectStructuredStepMetadata(events []RunEvent) ([]StructuredStepMetadata, error) {
	rows := []StructuredStepMetadata{}
	if !slices.ContainsFunc(events, func(e RunEvent) bool { return e.Type == string(EventStructuredStepReceiptRecorded) }) {
		return rows, nil
	}
	observed, ledger, err := indexStructuredStepEvidence(events)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Type != string(EventStructuredStepReceiptRecorded) {
			continue
		}
		p, err := decodeStructuredStepReceiptEvent(event)
		if err != nil {
			return nil, err
		}
		row := StructuredStepMetadata{RunID: event.RunID, BranchID: event.BranchID, TaskID: event.TaskID, Attempt: event.Attempt,
			OccurrenceRevision: p.OccurrenceRevision, SourceEventID: event.ID, SourceHash: event.Hash, ReceiptSHA256: p.Artifact.SHA256,
			Invocations: []StepModelExecution{}}
		if p.Summary == nil {
			row.ReceiptID, row.ExecutionKind, row.UnavailableReason = p.ReceiptID, "unknown", "receipt_summary_unavailable"
		} else {
			row.StructuredStepSummary = *p.Summary
			row.ModelExecutionIDs = slices.Clone(p.Summary.ModelExecutionIDs)
			receipt := ExecutionStepReceipt{ID: p.ReceiptID, RunID: event.RunID, BranchID: event.BranchID, TaskID: event.TaskID,
				Attempt: event.Attempt, OccurrenceRevision: p.OccurrenceRevision, StepID: p.Summary.StepID,
				RepairAttempt: p.Summary.RepairAttempt, ModelExecutionIDs: p.Summary.ModelExecutionIDs}
			row.Invocations, err = projectStepModelExecutions(receipt, observed, ledger)
			if err != nil {
				return nil, err
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// IncludeLegacyStructuredSteps exposes only recorded citations or declared
// steps when execution evidence is absent. It never infers a historical call.
func IncludeLegacyStructuredSteps(rows []StructuredStepMetadata, tasks []*TodoItem) []StructuredStepMetadata {
	result := slices.Clone(rows)
	seen := make(map[string]bool)
	for _, row := range rows {
		seen[row.TaskID] = true
	}
	for _, task := range tasks {
		if task == nil || seen[task.ID] || len(task.Execution.Steps) == 0 {
			continue
		}
		if task.TypedResult != nil && len(task.TypedResult.ReceiptIDs) > 0 {
			for _, id := range task.TypedResult.ReceiptIDs {
				result = append(result, StructuredStepMetadata{ReceiptID: id, ExecutionKind: "unknown", TaskID: task.ID,
					Attempt: task.TypedResult.Attempt, Invocations: []StepModelExecution{}, UnavailableReason: "historical_attribution_unavailable"})
			}
			continue
		}
		for _, step := range task.Execution.Steps {
			result = append(result, StructuredStepMetadata{StepID: step.ID, Tool: step.Tool, ExecutionKind: "unknown", TaskID: task.ID,
				Invocations: []StepModelExecution{}, UnavailableReason: "execution_evidence_unavailable"})
		}
	}
	if result == nil {
		return []StructuredStepMetadata{}
	}
	return result
}
