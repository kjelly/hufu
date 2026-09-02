package auditverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// WitnessSchemaVersion is the schema version for DecisionWitness.
const WitnessSchemaVersion = 1

// ReceiptRef points at a specific execution attempt without copying its
// receipt into the witness (spec.md §7.4).
type ReceiptRef struct {
	RunID            string `json:"run_id"`
	TaskID           string `json:"task_id"`
	Attempt          int    `json:"attempt"`
	ModelExecutionID string `json:"model_execution_id"`
	ProducerID       string `json:"producer_id"`
	ReceiptHash      string `json:"receipt_hash,omitempty"`
}

// CriterionWitness links an acceptance criterion to its verification
// evidence (spec.md §7.2).
type CriterionWitness struct {
	CriterionID string `json:"criterion_id"`

	// Required reflects the AcceptanceCriterion.Required flag recovered from
	// the run's own acceptance_contract_modified event, when one is present in
	// the workspace's lineage. Absent that event (older schema, or no
	// configured contract) this is left false rather than guessed true.
	Required bool `json:"required"`

	Status string `json:"status"`

	VerificationFingerprint string `json:"verification_fingerprint,omitempty"`

	ReceiptRefs            []ReceiptRef `json:"receipt_refs,omitempty"`
	ArtifactIDs            []string     `json:"artifact_ids,omitempty"`
	EvidenceRequirementIDs []string     `json:"evidence_requirement_ids,omitempty"`
}

// TaskWitness links a task to the evidence-bound winning attempt that
// justified its Done status (spec.md §7.3).
type TaskWitness struct {
	TaskID string `json:"task_id"`

	Status team.TaskStatus `json:"status"`

	WinningAttempt ReceiptRef `json:"winning_attempt"`

	EvidenceRequirementID string   `json:"evidence_requirement_id,omitempty"`
	ArtifactIDs           []string `json:"artifact_ids,omitempty"`
}

// GateWitness is a durable record of the completion decision. It only
// carries facts derivable from persisted data: a run-time-only precondition
// such as "no leaked terminal sessions" cannot be replayed after the fact, so
// it is deliberately not represented here rather than reported as a
// fabricated zero.
type GateWitness struct {
	Accepted bool     `json:"accepted"`
	Reasons  []string `json:"reasons,omitempty"`

	AcceptanceState        team.AcceptanceState `json:"acceptance_state"`
	EvidenceManifestStatus string               `json:"evidence_manifest_status,omitempty"`
	RequiredTasksTotal     int                  `json:"required_tasks_total"`
	RequiredTasksDone      int                  `json:"required_tasks_done"`
}

// DecisionWitness is the persisted, hash-linked explanation of why a run was
// certified with its outcome (spec.md §7.1).
type DecisionWitness struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`

	Outcome       team.RunOutcome `json:"outcome"`
	GoalSatisfied bool            `json:"goal_satisfied"`

	AcceptanceState team.AcceptanceState `json:"acceptance_state"`

	EvidenceManifestHash string `json:"evidence_manifest_hash,omitempty"`

	EventHeadID   string `json:"event_head_id"`
	EventHeadHash string `json:"event_head_hash"`

	Criteria []CriterionWitness `json:"criteria,omitempty"`
	Tasks    []TaskWitness      `json:"tasks,omitempty"`

	Gate GateWitness `json:"gate"`

	WitnessHash string `json:"witness_hash"`
}

// Seal computes WitnessHash = SHA256(normalized witness without WitnessHash),
// where "normalized" means Criteria/Tasks sorted by id so the hash does not
// depend on build-time iteration order (spec.md §37).
func (w *DecisionWitness) Seal() error {
	if w == nil {
		return fmt.Errorf("nil decision witness")
	}
	w.SchemaVersion = WitnessSchemaVersion
	sortWitnessSlices(w.Criteria, w.Tasks)
	unsealed := *w
	unsealed.WitnessHash = ""
	data, err := json.Marshal(unsealed)
	if err != nil {
		return fmt.Errorf("marshal decision witness: %w", err)
	}
	sum := sha256.Sum256(data)
	w.WitnessHash = hex.EncodeToString(sum[:])
	return nil
}

// Verify recomputes WitnessHash and reports whether it still matches.
func (w DecisionWitness) Verify() error {
	if w.WitnessHash == "" {
		return fmt.Errorf("decision witness is unsealed")
	}
	sortWitnessSlices(w.Criteria, w.Tasks)
	unsealed := w
	unsealed.WitnessHash = ""
	data, err := json.Marshal(unsealed)
	if err != nil {
		return fmt.Errorf("marshal decision witness: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != w.WitnessHash {
		return fmt.Errorf("decision witness hash mismatch")
	}
	return nil
}

func sortWitnessSlices(criteria []CriterionWitness, tasks []TaskWitness) {
	sort.Slice(criteria, func(i, j int) bool { return criteria[i].CriterionID < criteria[j].CriterionID })
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].TaskID < tasks[j].TaskID })
}

// acceptanceContractModifiedPayload mirrors the wire shape emitted by
// Coordinator.emitEvent("acceptance_contract_modified", ...): only the
// fields the witness needs.
type acceptanceContractModifiedPayload struct {
	NewSpec team.AcceptanceSpec `json:"new_spec"`
}

// requiredCriteriaIDs scans lineage for the run's own acceptance_contract_modified
// events -- emitted whenever the coordinator sets or updates its acceptance
// contract, including the initial team.yaml load -- and returns the Required
// flag from the latest such event's criteria. The returned map's key
// presence, not just its value, matters: an id absent from the map has an
// unknown Required flag (older schema, or no configured contract) and must
// not be treated as "confirmed not required".
func requiredCriteriaIDs(lineage []team.RunEvent, runID string) map[string]bool {
	result := make(map[string]bool)
	for _, event := range lineage {
		if event.Type != "acceptance_contract_modified" || event.RunID != runID {
			continue
		}
		var payload acceptanceContractModifiedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		// A later revision replaces the map wholesale: it is the run's
		// current contract, not an addition to a prior one.
		next := make(map[string]bool, len(payload.NewSpec.Criteria))
		for _, criterion := range payload.NewSpec.Criteria {
			next[criterion.ID] = criterion.Required
		}
		result = next
	}
	return result
}

// buildRunWitness assembles the GateWitness from a runProjection and the
// AuditVerificationResult already computed for it, then builds and seals the
// full DecisionWitness. It is the one witness-construction entry point
// shared by ExplainRun and ExportRun so both derive "accepted" the same way.
func buildRunWitness(runID string, verification *AuditVerificationResult, projection *runProjection) (*DecisionWitness, error) {
	gate := GateWitness{
		Accepted:               verification.Completion.Status == AuditDimensionPass && projection.runResult.Outcome == team.RunOutcomeCompleted,
		AcceptanceState:        acceptanceState(projection.runResult),
		EvidenceManifestStatus: evidenceStatus(projection.runResult),
		RequiredTasksTotal:     len(projection.tasks),
	}
	for _, item := range projection.tasks {
		if item != nil && item.Status == team.TaskDone {
			gate.RequiredTasksDone++
		}
	}
	if !gate.Accepted && projection.runResult.Reason != "" {
		gate.Reasons = []string{projection.runResult.Reason}
	}
	return buildDecisionWitness(runID, projection.runResult, projection.tasks,
		projection.terminalEvent.ID, projection.terminalEvent.Hash, projection.requiredCriteria, gate)
}

// buildDecisionWitness assembles a DecisionWitness from already-verified
// projection data (spec.md §7). It fabricates nothing: a task or criterion
// with no persisted evidence binding simply gets a zero-value ReceiptRef /
// empty fingerprint rather than a guessed one.
func buildDecisionWitness(runID string, runResult *team.RunResult, tasks []*team.TodoItem, eventHeadID, eventHeadHash string, requiredIDs map[string]bool, gate GateWitness) (*DecisionWitness, error) {
	if runResult == nil {
		return nil, fmt.Errorf("nil run result")
	}
	witness := &DecisionWitness{
		RunID:           runID,
		Outcome:         runResult.Outcome,
		GoalSatisfied:   runResult.GoalSatisfied,
		AcceptanceState: acceptanceState(runResult),
		EventHeadID:     eventHeadID,
		EventHeadHash:   eventHeadHash,
		Gate:            gate,
	}
	if runResult.EvidenceManifest != nil {
		witness.EvidenceManifestHash = runResult.EvidenceManifest.ManifestHash
	}

	tasksByID := make(map[string]*team.TodoItem, len(tasks))
	for _, item := range tasks {
		if item != nil {
			tasksByID[item.ID] = item
		}
	}

	// Build task witnesses first: criteria link to tasks via TodoItem.Advances
	// below, so the per-task receipt/artifact facts must already exist.
	taskWitnessByID := make(map[string]TaskWitness)
	if runResult.EvidenceManifest != nil {
		for _, er := range runResult.EvidenceManifest.EvidenceResults {
			taskID := strings.TrimPrefix(er.RequirementID, "task:")
			if taskID == er.RequirementID || er.Binding == nil {
				continue
			}
			item := tasksByID[taskID]
			tw := TaskWitness{TaskID: taskID, EvidenceRequirementID: er.RequirementID}
			if item != nil {
				tw.Status = item.Status
			}
			binding := er.Binding
			tw.WinningAttempt = ReceiptRef{
				RunID: binding.RunID, TaskID: binding.TaskID, Attempt: binding.Attempt,
				ModelExecutionID: binding.ModelExecutionID, ProducerID: binding.ProducerID,
			}
			if item != nil {
				if receipt := team.LatestSuccessfulExecutionReceipt(item, runID); receipt != nil {
					if hash, err := HashExecutionReceipt(*receipt); err == nil {
						tw.WinningAttempt.ReceiptHash = hash
					}
				}
			}
			tw.ArtifactIDs = append([]string(nil), binding.ArtifactIDs...)
			witness.Tasks = append(witness.Tasks, tw)
			taskWitnessByID[taskID] = tw
		}
	}

	if runResult.Acceptance != nil {
		for _, cr := range runResult.Acceptance.CriterionResults {
			cw := CriterionWitness{CriterionID: cr.ID, Status: string(cr.State), Required: requiredIDs[cr.ID]}
			if len(cr.Evidence) > 0 {
				cw.VerificationFingerprint = VerificationFingerprint(cr.Evidence[len(cr.Evidence)-1])
			}
			// Link every Done task that declared this criterion among the ones
			// it advances (TodoItem.Advances) to its own evidence-bound receipt
			// and artifacts. A criterion advanced by no completed task, or by a
			// task with no evidence binding, simply gets no linkage here rather
			// than a guessed one.
			for _, item := range tasks {
				if item == nil || item.Status != team.TaskDone || !slices.Contains(item.Advances, cr.ID) {
					continue
				}
				tw, ok := taskWitnessByID[item.ID]
				if !ok {
					continue
				}
				cw.EvidenceRequirementIDs = append(cw.EvidenceRequirementIDs, tw.EvidenceRequirementID)
				cw.ArtifactIDs = append(cw.ArtifactIDs, tw.ArtifactIDs...)
				cw.ReceiptRefs = append(cw.ReceiptRefs, tw.WinningAttempt)
			}
			witness.Criteria = append(witness.Criteria, cw)
		}
	}

	if err := witness.Seal(); err != nil {
		return nil, err
	}
	return witness, nil
}

// VerificationFingerprint returns v's persisted fingerprint, letting a
// CriterionWitness bind to "the exact persisted verification" (spec.md §19)
// without recomputing team.ComputeVerificationFingerprintFull -- which cannot
// be reproduced faithfully after the fact anyway, since it also hashes
// runtime-only inputs (acceptance revision, security mode) that audit has no
// access to. A nil result or one persisted before Fingerprint existed
// returns "": callers must treat that as "unavailable", never a wildcard.
func VerificationFingerprint(v *team.VerificationResult) string {
	if v == nil {
		return ""
	}
	return v.Fingerprint
}
