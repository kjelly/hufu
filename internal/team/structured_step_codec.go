package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/utils"
)

const maxStructuredStepReceiptBytes = 64 << 20

type stepNamedDigest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Maps keyed by user-declared output names are encoded as arrays. A name such
// as "api_key" denotes an artifact, not a credential whose digest the JSON
// redactor should discover as a secret. No arbitrary values bypass redaction.
type structuredStepReceiptBlob struct {
	Version  int                  `json:"version"`
	Receipt  ExecutionStepReceipt `json:"receipt"`
	Consumed []stepNamedDigest    `json:"consumed_digests,omitempty"`
	Produced []stepNamedDigest    `json:"produced_digests,omitempty"`
}

func encodeStructuredStepReceipt(receipt ExecutionStepReceipt) (ExecutionStepReceipt, []byte, error) {
	blob := structuredStepReceiptBlob{Version: 1, Receipt: cloneExecutionStepReceipt(receipt),
		Consumed: namedStepDigests(receipt.ConsumedDigests), Produced: namedStepDigests(receipt.ProducedDigests)}
	blob.Receipt.ConsumedDigests, blob.Receipt.ProducedDigests = nil, nil
	raw, err := json.Marshal(blob, json.Deterministic(true))
	if err != nil {
		return receipt, nil, err
	}
	if len(raw) > maxStructuredStepReceiptBytes {
		return receipt, nil, fmt.Errorf("structured step receipt exceeds storage limit; diagnostics were not truncated")
	}
	raw, err = utils.RedactJSONCompact(raw)
	if err != nil {
		return receipt, nil, err
	}
	var redacted structuredStepReceiptBlob
	if err := json.Unmarshal(raw, &redacted, json.RejectUnknownMembers(true)); err != nil {
		return receipt, nil, err
	}
	originalIdentity, err := stepReceiptIdentityBytes(blob)
	if err != nil {
		return receipt, nil, err
	}
	redactedIdentity, err := stepReceiptIdentityBytes(redacted)
	if err != nil || !bytes.Equal(originalIdentity, redactedIdentity) {
		return receipt, nil, fmt.Errorf("structured step receipt identity changed during redaction")
	}
	redacted.Receipt.StdoutRef = executionOutputRef(receipt.ID, "stdout", redacted.Receipt.Stdout)
	redacted.Receipt.StderrRef = executionOutputRef(receipt.ID, "stderr", redacted.Receipt.Stderr)
	raw, err = json.Marshal(redacted, json.Deterministic(true))
	if err != nil {
		return receipt, nil, err
	}
	canonical, err := decodeStructuredStepReceipt(raw)
	return canonical, raw, err
}

func stepReceiptIdentityBytes(blob structuredStepReceiptBlob) ([]byte, error) {
	blob.Receipt = cloneExecutionStepReceipt(blob.Receipt)
	blob.Receipt.Stdout, blob.Receipt.Stderr, blob.Receipt.ProposedModel = "", "", ""
	blob.Receipt.StdoutRef, blob.Receipt.StderrRef = ArtifactRef{}, ArtifactRef{}
	for i := range blob.Receipt.Consumed {
		blob.Receipt.Consumed[i].Description = ""
	}
	for i := range blob.Receipt.Produced {
		blob.Receipt.Produced[i].Description = ""
	}
	return json.Marshal(blob, json.Deterministic(true))
}

func namedStepDigests(values map[string]string) []stepNamedDigest {
	var result []stepNamedDigest
	for _, name := range slices.Sorted(maps.Keys(values)) {
		result = append(result, stepNamedDigest{Name: name, SHA256: values[name]})
	}
	return result
}

func restoreStepDigests(values []stepNamedDigest) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		if value.Name == "" || !validStructuredStepDigest(value.SHA256) {
			return nil, fmt.Errorf("invalid structured step artifact digest")
		}
		if _, exists := result[value.Name]; exists {
			return nil, fmt.Errorf("duplicate structured step artifact digest")
		}
		result[value.Name] = value.SHA256
	}
	return result, nil
}

func decodeStructuredStepReceipt(raw []byte) (ExecutionStepReceipt, error) {
	var blob structuredStepReceiptBlob
	if len(raw) > maxStructuredStepReceiptBytes {
		return ExecutionStepReceipt{}, fmt.Errorf("structured step receipt exceeds storage limit")
	}
	if err := json.Unmarshal(raw, &blob, json.RejectUnknownMembers(true)); err != nil {
		return ExecutionStepReceipt{}, err
	}
	if blob.Version != 1 || blob.Receipt.ConsumedDigests != nil || blob.Receipt.ProducedDigests != nil {
		return ExecutionStepReceipt{}, fmt.Errorf("invalid structured step receipt encoding")
	}
	var err error
	blob.Receipt.ConsumedDigests, err = restoreStepDigests(blob.Consumed)
	if err != nil {
		return ExecutionStepReceipt{}, err
	}
	blob.Receipt.ProducedDigests, err = restoreStepDigests(blob.Produced)
	if err != nil {
		return ExecutionStepReceipt{}, err
	}
	if err := validateStructuredStepReceipt(blob.Receipt); err != nil {
		return ExecutionStepReceipt{}, err
	}
	return blob.Receipt, nil
}

func validateStructuredStepReceipt(receipt ExecutionStepReceipt) error {
	if receipt.ID == "" || receipt.RunID == "" || receipt.BranchID == "" || receipt.TaskID == "" || receipt.Attempt < 1 ||
		receipt.OccurrenceRevision < 0 || receipt.RepairAttempt < 0 || receipt.StepID == "" || receipt.Tool == "" ||
		!validStructuredStepDigest(receipt.InputSHA256) || receipt.StartedAt.IsZero() || receipt.FinishedAt.Before(receipt.StartedAt) {
		return fmt.Errorf("invalid structured step receipt identity")
	}
	switch receipt.ExecutionKind {
	case "tool_call", "unknown":
		if len(receipt.ModelExecutionIDs) != 0 {
			return fmt.Errorf("non-generative step claims model invocations")
		}
	case "model_generation":
		if len(receipt.ModelExecutionIDs) == 0 {
			return fmt.Errorf("generative step has no model invocation evidence")
		}
	default:
		return fmt.Errorf("invalid structured step execution kind")
	}
	if !sameArtifactRef(receipt.StdoutRef, executionOutputRef(receipt.ID, "stdout", receipt.Stdout)) ||
		!sameArtifactRef(receipt.StderrRef, executionOutputRef(receipt.ID, "stderr", receipt.Stderr)) {
		return fmt.Errorf("structured step diagnostic digest mismatch")
	}
	return nil
}
