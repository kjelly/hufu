package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// StepModelExecution refers to an existing provider invocation. Missing target
// evidence remains unavailable; neither the proposal nor a parent worker's
// target can fill it in.
type StepModelExecution struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Provider          string `json:"provider"`
	ExecutionTarget   string `json:"execution_target,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type StructuredStepEvidence struct {
	Receipt       ExecutionStepReceipt `json:"-"`
	SourceEventID string               `json:"source_event_id"`
	SourceHash    string               `json:"source_hash"`
	ReceiptSHA256 string               `json:"receipt_sha256"`
	Invocations   []StepModelExecution `json:"invocations"`
}

type stepInvocationEvidence struct {
	event   RunEvent
	payload structuredStepInvocationObserved
	profile modelprofile.TelemetryProjection
}

// LoadStructuredStepReceipts reads only CAS objects cited by the supplied
// validated, selected lineage. It does not open a writable store, reconstruct
// missing historical receipts, or grant authority to sibling-branch events.
func LoadStructuredStepReceipts(ctx context.Context, workspace string, events []RunEvent) ([]StructuredStepEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows := []StructuredStepEvidence{}
	if !slices.ContainsFunc(events, func(event RunEvent) bool { return event.Type == string(EventStructuredStepReceiptRecorded) }) {
		return rows, nil
	}
	observed, ledger, err := indexStructuredStepEvidence(events)
	if err != nil {
		return nil, err
	}
	store, err := OpenFileArtifactStoreReadOnly(workspace, "")
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Type != string(EventStructuredStepReceiptRecorded) {
			continue
		}
		row, err := readStructuredStepEvidence(ctx, store, event, observed, ledger)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, ctx.Err()
}

func readStepInvocationEvidence(event RunEvent, profiles map[string]RunEvent) (stepInvocationEvidence, error) {
	payload, err := decodeStructuredStepInvocation(event)
	if err != nil {
		return stepInvocationEvidence{}, err
	}
	profileEvent, ok := profiles[payload.ProfileEventID]
	if !ok || profileEvent.Hash != payload.ProfileHash || profileEvent.RunID != event.RunID || profileEvent.BranchID != event.BranchID ||
		profileEvent.Hash != ComputeEventHash(profileEvent.PreviousHash, profileEvent.ID, profileEvent.Type, profileEvent.Timestamp, profileEvent.Payload) {
		return stepInvocationEvidence{}, fmt.Errorf("structured step invocation profile source mismatch")
	}
	var profile modelprofile.TelemetryProjection
	if err := json.Unmarshal(profileEvent.Payload, &profile); err != nil {
		return stepInvocationEvidence{}, err
	}
	if (profile.SchemaVersion != 1 && profile.SchemaVersion != 2) || profile.InvocationID != payload.ModelExecutionID || profile.ModelID == "" || profile.Provider == "" {
		return stepInvocationEvidence{}, fmt.Errorf("structured step invocation profile identity mismatch")
	}
	return stepInvocationEvidence{event: event, payload: payload, profile: profile}, nil
}

func readStructuredStepEvidence(ctx context.Context, store *FileArtifactStore, event RunEvent, observed map[stepReceiptScopeKey][]stepInvocationEvidence, ledger cost.Projection) (StructuredStepEvidence, error) {
	payload, err := decodeStructuredStepReceiptEvent(event)
	if err != nil {
		return StructuredStepEvidence{}, err
	}
	if err := store.Verify(ctx, payload.Artifact); err != nil {
		return StructuredStepEvidence{}, err
	}
	reader, err := store.Open(ctx, payload.Artifact.ID)
	if err != nil {
		return StructuredStepEvidence{}, err
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, maxStructuredStepReceiptBytes+1))
	if err != nil {
		return StructuredStepEvidence{}, err
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != payload.Artifact.SHA256 || int64(len(raw)) != payload.Artifact.Bytes {
		return StructuredStepEvidence{}, fmt.Errorf("structured step receipt content mismatch")
	}
	receipt, err := decodeStructuredStepReceipt(raw)
	if err != nil {
		return StructuredStepEvidence{}, err
	}
	if receipt.ID != payload.ReceiptID || receipt.RunID != event.RunID || receipt.BranchID != event.BranchID ||
		receipt.TaskID != event.TaskID || receipt.Attempt != event.Attempt || receipt.OccurrenceRevision != payload.OccurrenceRevision {
		return StructuredStepEvidence{}, fmt.Errorf("structured step receipt occurrence mismatch")
	}
	row := StructuredStepEvidence{Receipt: receipt, SourceEventID: event.ID, SourceHash: event.Hash, ReceiptSHA256: payload.Artifact.SHA256, Invocations: []StepModelExecution{}}
	if payload.Summary != nil && !sameStructuredStepSummary(*payload.Summary, summarizeStructuredStep(receipt)) {
		return StructuredStepEvidence{}, fmt.Errorf("structured step summary differs from complete receipt")
	}
	row.Invocations, err = projectStepModelExecutions(receipt, observed, ledger)
	if err != nil {
		return StructuredStepEvidence{}, err
	}
	return row, ctx.Err()
}

func projectStepModelExecutions(receipt ExecutionStepReceipt, observed map[stepReceiptScopeKey][]stepInvocationEvidence, ledger cost.Projection) ([]StepModelExecution, error) {
	models := []StepModelExecution{}
	var ids []string
	for _, invocation := range observed[stepReceiptSourceKey(receipt)] {
		if invocation.event.RunID != receipt.RunID || invocation.event.BranchID != receipt.BranchID ||
			invocation.event.TaskID != receipt.TaskID || invocation.event.Attempt != receipt.Attempt ||
			invocation.payload.StepID != receipt.StepID || invocation.payload.RepairAttempt != receipt.RepairAttempt || invocation.payload.OccurrenceRevision != receipt.OccurrenceRevision {
			return nil, fmt.Errorf("structured step invocation occurrence mismatch")
		}
		ids = append(ids, invocation.payload.ModelExecutionID)
		actual := StepModelExecution{ID: invocation.payload.ModelExecutionID, Model: invocation.profile.ModelID, Provider: invocation.profile.Provider,
			UnavailableReason: "execution_target_unavailable"}
		if invocation.profile.ExecutionTarget != "" {
			target, err := canonicalStepProfileTarget(invocation.profile.ExecutionTarget)
			if err != nil {
				return nil, err
			}
			actual.ExecutionTarget, actual.UnavailableReason = target, ""
		}
		if entry, ok := ledger.Invocation(receipt.RunID, actual.ID); ok {
			if entry.Reservation.TaskID != receipt.TaskID || entry.Reservation.OccurrenceAttempt != receipt.Attempt {
				return nil, fmt.Errorf("structured step cost invocation occurrence mismatch")
			}
			if actual.ExecutionTarget != "" && actual.ExecutionTarget != entry.Reservation.ExecutionTarget {
				return nil, fmt.Errorf("structured step profile and cost target differ")
			}
			// Legacy bare-model telemetry cannot prove which default selected
			// the old reservation. Keep that target unavailable rather than
			// manufacturing a historical binding from the ledger alone.
			if actual.ExecutionTarget == "" {
				selector, err := execution.ParseExecutionSelector(invocation.profile.ModelID)
				if err == nil && selector.Backend != "" && execution.CanonicalTargetBackendName(selector.Backend) == execution.CanonicalTargetBackendName(invocation.profile.Provider) {
					target := execution.ExecutionTarget{Backend: execution.CanonicalTargetBackendName(selector.Backend), Model: selector.Model}
					if target.Validate() == nil && target.String() == entry.Reservation.ExecutionTarget {
						actual.ExecutionTarget, actual.UnavailableReason = target.String(), ""
					}
				}
			}
		}
		models = append(models, actual)
	}
	if !slices.Equal(receipt.ModelExecutionIDs, ids) {
		return nil, fmt.Errorf("structured step model invocation evidence mismatch")
	}
	return models, nil
}

func canonicalStepProfileTarget(value string) (string, error) {
	selector, err := execution.ParseExecutionSelector(value)
	if err != nil || selector.Backend == "" {
		return "", fmt.Errorf("invalid runtime profile execution target")
	}
	target := execution.ExecutionTarget{Backend: execution.CanonicalTargetBackendName(selector.Backend), Model: selector.Model}
	if err := target.Validate(); err != nil || target.String() != value {
		return "", fmt.Errorf("noncanonical runtime profile execution target")
	}
	return target.String(), nil
}

func stepInvocationSourceKey(event RunEvent, payload structuredStepInvocationObserved) stepReceiptScopeKey {
	return stepReceiptScopeKey{runID: event.RunID, branchID: event.BranchID, taskID: event.TaskID, attempt: event.Attempt, revision: payload.OccurrenceRevision, receiptID: payload.ReceiptID}
}
