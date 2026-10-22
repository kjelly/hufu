package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
)

// ValidateRequestAcceptanceBinding is shared by the live completion gate and
// offline audit. It rechecks immutable inputs and actual quoted tool results;
// it does not re-run the semantic reviewer or any task/tool side effects.
func ValidateRequestAcceptanceBinding(ctx context.Context, result *RunResult, items []*TodoItem, store ArtifactStore) error {
	if result == nil {
		return nil
	}
	hasReview := result.Acceptance != nil && result.Acceptance.RequestAcceptance != nil
	if result.RunInputs == nil {
		if hasReview && result.Outcome == RunOutcomeCompleted && result.GoalSatisfied {
			return fmt.Errorf("completed request acceptance has no invocation snapshot")
		}
		return nil
	}
	configured := false
	for _, input := range result.RunInputs.Inputs {
		configured = configured || input.Name == requestAcceptanceInput
	}
	if !configured {
		if hasReview && result.Outcome == RunOutcomeCompleted && result.GoalSatisfied {
			return fmt.Errorf("completed request acceptance has no frozen requirements")
		}
		return nil
	}
	// An incomplete/failed run need not have finished the semantic review.
	if result.Outcome != RunOutcomeCompleted || !result.GoalSatisfied {
		return nil
	}
	contract, err := RequestAcceptanceContractFromSnapshot(result.RunInputs)
	if err != nil {
		return err
	}
	if result.Acceptance == nil || result.Acceptance.RequestAcceptance == nil {
		return fmt.Errorf("completed request has no request acceptance review")
	}
	review := result.Acceptance.RequestAcceptance
	if err := review.Validate(); err != nil {
		return err
	}
	if review.SnapshotID != result.RunInputs.ID || review.SnapshotHash != result.RunInputs.SnapshotHash {
		return fmt.Errorf("request acceptance review belongs to a different invocation snapshot")
	}
	response := strings.TrimSuffix(result.Response, FormatRequestAcceptance(review))
	if review.ResponseHash != runInputHash([]byte(response)) {
		return fmt.Errorf("request acceptance review belongs to a different final response")
	}
	want, _ := json.Marshal(contract, json.Deterministic(true))
	got, _ := json.Marshal(review.Contract, json.Deterministic(true))
	if !bytes.Equal(want, got) {
		return fmt.Errorf("request acceptance review weakened or changed its frozen contract")
	}
	for _, check := range review.Checks {
		for _, proof := range check.Evidence {
			if err := validateRequestAcceptanceProof(ctx, result, items, store, proof); err != nil {
				return fmt.Errorf("request acceptance %s: %w", check.ID, err)
			}
		}
	}
	return nil
}

func validateRequestAcceptanceProof(ctx context.Context, result *RunResult, items []*TodoItem, store ArtifactStore, proof RequestAcceptanceProof) error {
	if proof.SourceID == requestAcceptanceFinalResponseSource {
		review := result.Acceptance.RequestAcceptance
		response := strings.TrimSuffix(result.Response, FormatRequestAcceptance(review))
		if !review.validFinalResponseProof(proof) || proof.ArtifactHash != runInputHash([]byte(response)) || !strings.Contains(response, proof.Quote) {
			return fmt.Errorf("quotation does not match the hash-bound final response")
		}
		return nil
	}
	var item *TodoItem
	for _, candidate := range items {
		if candidate != nil && candidate.ID == proof.TaskID {
			item = candidate
			break
		}
	}
	if item == nil || item.Status != TaskDone || item.RunInputSnapshotID != result.RunInputs.ID || item.RunInputSnapshotHash != result.RunInputs.SnapshotHash {
		return fmt.Errorf("proof is not from a completed task bound to this invocation")
	}
	receipt := latestSuccessfulExecutionReceipt(item, result.RunID)
	if receipt == nil && result.RunInputs.RunID != result.RunID {
		receipt = latestSuccessfulExecutionReceipt(item, result.RunInputs.RunID)
	}
	if receipt == nil || receipt.TranscriptRef != proof.ArtifactID || receipt.RunInputSnapshotID != result.RunInputs.ID || receipt.RunInputSnapshotHash != result.RunInputs.SnapshotHash || store == nil {
		return fmt.Errorf("proof does not reference the winning task transcript")
	}
	ref, err := store.Resolve(ctx, ArtifactRef{ID: proof.ArtifactID, SHA256: proof.ArtifactHash})
	if err != nil {
		return err
	}
	if ref.SHA256 != proof.ArtifactHash {
		return fmt.Errorf("proof artifact digest mismatch")
	}
	if err := store.Verify(ctx, ref); err != nil {
		return err
	}
	if verification := requestAcceptanceVerificationResult(item, receipt); proof.Kind == "observed" && verification != nil &&
		proof.ToolCallID == "runtime-verification:"+verification.Fingerprint {
		if text := requestAcceptanceVerificationText(item, receipt); text != "" && strings.Contains(text, proof.Quote) {
			return nil
		}
		return fmt.Errorf("quotation does not match a successful runtime verifier receipt")
	}
	if proof.Kind == "response" && item.TypedResult != nil {
		body, err := json.Marshal(item.TypedResult, json.Deterministic(true))
		if err == nil && strings.Contains(string(body), proof.Quote) {
			return nil
		}
	}
	if (proof.Kind != "response" && (proof.Kind != "observed" || proof.ToolCallID == "")) || ref.Bytes > 16<<20 {
		return fmt.Errorf("invalid observed proof")
	}
	return validateRequestTranscriptQuote(ctx, store, ref, proof, receipt, item)
}

func requestAcceptanceVerificationResult(item *TodoItem, receipt *ExecutionReceipt) *VerificationResult {
	if receipt != nil && receipt.VerifyResult != nil {
		return receipt.VerifyResult
	}
	if item != nil {
		return item.VerifyResult
	}
	return nil
}

func requestAcceptanceVerificationText(item *TodoItem, receipt *ExecutionReceipt) string {
	verification := requestAcceptanceVerificationResult(item, receipt)
	if !isVerifySuccess(verification) || verification.Fingerprint == "" {
		return ""
	}
	// VerificationResult is a legacy wire type containing time.Duration.
	// Project only the observation fields into this review's v2 JSON contract.
	body, err := json.Marshal(struct {
		Command     string `json:"command"`
		ExitCode    int    `json:"exit_code"`
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		TimedOut    bool   `json:"timed_out"`
		WeakWarning bool   `json:"weak_warning"`
		Overturned  bool   `json:"overturned"`
		Fingerprint string `json:"fingerprint"`
	}{verification.Command, verification.ExitCode, verification.Stdout, verification.Stderr,
		verification.TimedOut, verification.WeakWarning, verification.Overturned, verification.Fingerprint}, json.Deterministic(true))
	if err != nil {
		return ""
	}
	return string(body)
}

func validateRequestTranscriptQuote(ctx context.Context, store ArtifactStore, ref ArtifactRef, proof RequestAcceptanceProof, receipt *ExecutionReceipt, item *TodoItem) error {
	body, err := store.Open(ctx, ref.ID)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	scanner := bufio.NewScanner(io.LimitReader(body, (16<<20)+1))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	called := ""
	for scanner.Scan() {
		var record taskTranscriptRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return err
		}
		if record.Event == "assistant_output" && strings.Contains(record.Output, proof.Quote) {
			if proof.Kind == "response" || (receipt.ActionInvocationID != "" && proof.ToolCallID == receipt.ActionInvocationID && item.TypedResult != nil && item.TypedResult.Source == "runtime") {
				return nil
			}
		}
		if record.ToolCallID != proof.ToolCallID {
			continue
		}
		if record.Event == "tool_call" {
			called = record.Tool
		}
		if record.Event == "tool_result" && called != "" && record.Tool == called && record.Tool != "submit_result" &&
			!record.Error && (record.ExitCode == nil || *record.ExitCode == 0) && strings.Contains(record.Output, proof.Quote) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("quotation is not in a successful matching tool result")
}

// FormatRequestAcceptance is reused by final text, reports, and TUI result
// views. JSON/inspect expose the full typed value, including evidence refs.
func FormatRequestAcceptance(result *RequestAcceptanceResult) string {
	if result == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Request acceptance\n\n")
	if result.Error != "" {
		fmt.Fprintf(&b, "Review unavailable: %s\n", result.Error)
	}
	if !result.RequestCovered {
		b.WriteString("The extracted contract did not pass the original-request coverage check.\n")
	}
	for _, requirement := range result.Contract.Requirements {
		state, reason := "missing", "No accepted review evidence"
		for _, check := range result.Checks {
			if check.ID == requirement.ID {
				state, reason = check.Status, check.Reason
				break
			}
		}
		necessity := "optional"
		if requirement.Required {
			necessity = "required"
		}
		fmt.Fprintf(&b, "- %s (%s): %s — %s. %s\n", requirement.ID, necessity, state, requirement.Statement, reason)
		if result.MaterialReview != nil {
			for _, material := range result.MaterialReview.Checks {
				if material.ID == requirement.ID {
					fmt.Fprintf(&b, "  Material equivalence: %s; adequate=%t. %s\n", material.Form, material.Adequate, material.Reason)
				}
			}
		}
		if delivery := requirement.Delivery; delivery != nil {
			fmt.Fprintf(&b, "  Delivery: %s; extent=%s; source=%s; authorized substitutes=%d.\n", delivery.Target, delivery.Extent, delivery.Source, len(delivery.AllowedSubstitutes))
			for _, check := range result.Checks {
				if check.ID == requirement.ID && check.Delivery != nil {
					fmt.Fprintf(&b, "  Observed delivery: match=%s; extent=%s; source=%s.\n", check.Delivery.Match, check.Delivery.Extent, check.Delivery.Source)
					for _, gap := range check.Delivery.Gaps {
						fmt.Fprintf(&b, "  Gap: %s\n", gap)
					}
					for _, limitation := range check.Delivery.Limitations {
						fmt.Fprintf(&b, "  Limitation: %s\n", limitation)
					}
				}
			}
		}
	}
	return b.String()
}
