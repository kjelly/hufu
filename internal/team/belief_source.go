package team

import (
	"cmp"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
)

// BeliefVerificationEpoch derives a narrow, immutable receipt/input scope. It
// does not claim to fingerprint the live OS or unrecorded workspace contents.
func BeliefVerificationEpoch(model belief.Model, scope belief.Scope, event RunEvent, receiptKey string) (belief.Epoch, error) {
	receipt, err := beliefSourceReceipt(event, scope, receiptKey)
	if err != nil {
		return belief.Epoch{}, err
	}
	modelHash, err := belief.ModelDigest(model)
	if err != nil {
		return belief.Epoch{}, err
	}
	epoch := belief.Epoch{ModelDigest: modelHash, Scope: scope, SubjectRevision: receipt.RunInputSnapshotHash, EnvironmentRevision: beliefEnvironmentRevision(receipt)}
	epoch.ID = beliefIdentity("epoch", struct {
		Scope                                belief.Scope
		Model, Subject, Environment, Receipt string
	}{scope, modelHash, epoch.SubjectRevision, epoch.EnvironmentRevision, beliefReceiptKey(receipt)})
	return epoch, nil
}
func beliefSourceReceipt(event RunEvent, scope belief.Scope, key string) (ExecutionReceipt, error) {
	if !slices.Contains([]string{"task_completed", "task_failed", "task_blocked", "task_protocol_incomplete"}, event.Type) || event.RunID != scope.RunID || event.TaskID != scope.TaskID || scope.SessionID != "" && event.SessionID != scope.SessionID {
		return ExecutionReceipt{}, fmt.Errorf("belief source is not an owning task verification")
	}
	receipts, err := beliefSourceReceipts(event)
	if err != nil {
		return ExecutionReceipt{}, err
	}
	var matched *ExecutionReceipt
	for _, receipt := range receipts {
		if receipt.RunID != scope.RunID || receipt.TaskID != scope.TaskID || cmp.Or(receipt.OccurrenceAttempt, receipt.Attempt) != scope.Attempt || receipt.VerifyResult == nil {
			continue
		}
		if receipt.RunInputSnapshotHash == "" || receipt.VerifyResult.Fingerprint == "" || receipt.ModelExecutionID == "" && receipt.ActionInvocationID == "" {
			continue
		}
		if key != "" && beliefReceiptKey(receipt) != key {
			continue
		}
		if matched != nil && beliefReceiptKey(*matched) != beliefReceiptKey(receipt) {
			return ExecutionReceipt{}, fmt.Errorf("ambiguous belief source receipt")
		}
		copyReceipt := receipt
		matched = &copyReceipt
	}
	if matched == nil {
		return ExecutionReceipt{}, fmt.Errorf("belief source receipt binding unavailable")
	}
	return *matched, nil
}

// Some current success paths store objective verification on the terminal
// event rather than inside its current receipt. Bind that typed result only
// to the explicit current receipt and matching occurrence. Historical receipts
// never inherit a todo-wide result; persisted receipt bytes are never changed.
func beliefSourceReceipts(event RunEvent) ([]ExecutionReceipt, error) {
	var payload struct {
		Receipt      *ExecutionReceipt   `json:"execution_receipt"`
		Receipts     []ExecutionReceipt  `json:"execution_receipts"`
		Verification *VerificationResult `json:"verify_result"`
		Attempt      int                 `json:"attempt"`
		InputHash    string              `json:"run_input_snapshot_hash"`
	}
	// Canonical execution events encode durations as integer nanoseconds.
	if err := json.Unmarshal(event.Payload, &payload, jsonv1.FormatDurationAsNano(true)); err != nil {
		return nil, fmt.Errorf("invalid belief source receipt payload: %w", err)
	}
	receipts := slices.Clone(payload.Receipts)
	if payload.Receipt != nil {
		// The current receipt is also commonly present in the history array.
		// Remove only exact aliases before enriching that one current receipt.
		identity := beliefIdentity("receipt_alias", payload.Receipt)
		receipts = slices.DeleteFunc(receipts, func(receipt ExecutionReceipt) bool {
			return beliefIdentity("receipt_alias", receipt) == identity
		})
		current := *payload.Receipt
		if payload.Attempt > 0 && cmp.Or(current.OccurrenceAttempt, current.Attempt) == payload.Attempt && current.RunID == event.RunID && current.TaskID == event.TaskID {
			if current.RunInputSnapshotHash != "" && payload.InputHash != "" && current.RunInputSnapshotHash != payload.InputHash {
				return nil, fmt.Errorf("belief current receipt input revision mismatch")
			}
			if current.RunInputSnapshotHash == "" {
				current.RunInputSnapshotHash = payload.InputHash
			}
			if current.VerifyResult == nil {
				current.VerifyResult = payload.Verification
			}
		}
		receipts = append(receipts, current)
	}
	return receipts, nil
}
func beliefReceiptKey(receipt ExecutionReceipt) string {
	return beliefIdentity("receipt", struct {
		Run, Task                                   string
		Occurrence, Attempt                         int
		ModelExecution, Action, Input, Verification string
	}{receipt.RunID, receipt.TaskID, cmp.Or(receipt.OccurrenceAttempt, receipt.Attempt), receipt.Attempt, receipt.ModelExecutionID, receipt.ActionInvocationID, receipt.RunInputSnapshotHash, receipt.VerifyResult.Fingerprint})
}
func beliefEnvironmentRevision(receipt ExecutionReceipt) string {
	result := receipt.VerifyResult
	return beliefIdentity("verification_environment", struct {
		Spec                      *VerificationSpec
		Command, WorkDir, Backend string
		Target                    any
	}{result.Spec, result.Command, result.WorkDir, receipt.Backend, receipt.ExecutionTarget})
}
func beliefVerificationObservation(epoch belief.Epoch, event RunEvent, model belief.Model, experimentID, key string) (belief.Observation, BeliefSource, error) {
	receipt, err := beliefSourceReceipt(event, epoch.Scope, key)
	if err != nil {
		return belief.Observation{}, BeliefSource{}, err
	}
	if epoch.SubjectRevision != receipt.RunInputSnapshotHash || epoch.EnvironmentRevision != beliefEnvironmentRevision(receipt) {
		return belief.Observation{}, BeliefSource{}, fmt.Errorf("belief source revision mismatch")
	}
	index := slices.IndexFunc(model.Experiments, func(e belief.Experiment) bool { return e.ID == experimentID && e.Source == "verification" })
	if index < 0 {
		return belief.Observation{}, BeliefSource{}, fmt.Errorf("belief experiment does not admit verification")
	}
	outcome := beliefVerificationOutcome(receipt)
	if !slices.ContainsFunc(model.Experiments[index].Outcomes, func(o belief.Outcome) bool { return o.ID == outcome }) {
		return belief.Observation{}, BeliefSource{}, fmt.Errorf("verification outcome is outside belief model")
	}
	source := BeliefSource{EventID: event.ID, EventHash: event.Hash, ReceiptKey: beliefReceiptKey(receipt)}
	observation := belief.Observation{ID: beliefIdentity("observation", []string{epoch.ID, source.ReceiptKey, experimentID}), ExperimentID: experimentID, OutcomeID: outcome, SourceID: source.ReceiptKey}
	return observation, source, nil
}

func beliefVerificationOutcome(receipt ExecutionReceipt) string {
	if receipt.VerifyResult.ExitCode == 0 && !receipt.VerifyResult.TimedOut && !receipt.VerifyResult.Overturned {
		return "pass"
	}
	return "fail"
}
