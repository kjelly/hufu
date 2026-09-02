package auditverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/kjelly/hufu/internal/team"
)

// NormalizeExecutionReceipt returns a deterministic, deep-copied projection
// of receipt suitable for hashing (spec.md §8.2). It never mutates receipt.
//
// team.ExecutionReceipt currently has no slice whose element order is
// semantically insignificant (RepairProvenance.History and ToolDispositions
// are both meaningful sequences and must keep their original order), so there
// is nothing to sort today; if a future field needs order-independent
// hashing, sort it here rather than at the call site.
func NormalizeExecutionReceipt(receipt team.ExecutionReceipt) team.ExecutionReceipt {
	data, err := json.Marshal(receipt)
	if err != nil {
		// ExecutionReceipt has no cyclic references or non-finite floats; a
		// marshal failure here means the value is fundamentally broken. Return
		// the zero value rather than the (possibly still-shared) original, so a
		// hash computed from this can never coincidentally collide with a
		// genuine receipt's hash.
		return team.ExecutionReceipt{}
	}
	var normalized team.ExecutionReceipt
	if err := json.Unmarshal(data, &normalized); err != nil {
		return team.ExecutionReceipt{}
	}
	return normalized
}

// HashExecutionReceipt computes sha256(json.Marshal(NormalizeExecutionReceipt(receipt)))
// (spec.md §8.2). RFC 8785 canonicalization is deliberately not used yet
// (spec.md: "第一版不必導入 RFC 8785").
func HashExecutionReceipt(receipt team.ExecutionReceipt) (string, error) {
	normalized := NormalizeExecutionReceipt(receipt)
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal normalized execution receipt: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
