package team

import (
	"cmp"
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
	var payload struct {
		Receipt  *ExecutionReceipt  `json:"execution_receipt"`
		Receipts []ExecutionReceipt `json:"execution_receipts"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return ExecutionReceipt{}, fmt.Errorf("invalid belief source receipt payload")
	}
	receipts := slices.Clone(payload.Receipts)
	if payload.Receipt != nil {
		receipts = append(receipts, *payload.Receipt)
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
