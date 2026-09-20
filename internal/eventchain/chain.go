// Package eventchain owns the durable event hash and linkage contract shared
// by runtime readers and workspace migration verification.
package eventchain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Entry contains the persisted fields that participate in event-chain
// verification.
type Entry struct {
	ID           string          `json:"id"`
	PreviousID   string          `json:"previous_id,omitempty"`
	Type         string          `json:"type"`
	Timestamp    string          `json:"timestamp"`
	Payload      json.RawMessage `json:"payload,omitempty"`
	PreviousHash string          `json:"previous_hash,omitempty"`
	Hash         string          `json:"hash,omitempty"`
}

// Verifier validates a root-based event chain in sequence.
type Verifier struct {
	sequence     int
	previousID   string
	previousHash string
}

// ComputeHash computes the canonical SHA-256 event digest.
func ComputeHash(previousHash, id, eventType, timestamp string, payload json.RawMessage) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(previousHash))
	_, _ = hash.Write([]byte(id))
	_, _ = hash.Write([]byte(eventType))
	_, _ = hash.Write([]byte(timestamp))
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

// Verify validates and advances the chain. It does not mutate verifier state
// when validation fails.
func (v *Verifier) Verify(entry Entry) error {
	if v.sequence == 0 {
		if entry.PreviousID != "" || entry.PreviousHash != "" {
			return fmt.Errorf("first event (%s) must have empty previous_id and previous_hash", entry.ID)
		}
	} else if entry.PreviousID != v.previousID || entry.PreviousHash != v.previousHash {
		return fmt.Errorf("event %d (%s) does not continue hash chain", v.sequence, entry.ID)
	}
	if entry.Hash != ComputeHash(entry.PreviousHash, entry.ID, entry.Type, entry.Timestamp, entry.Payload) {
		return fmt.Errorf("event %d (%s) hash invalid", v.sequence, entry.ID)
	}
	v.sequence++
	v.previousID = entry.ID
	v.previousHash = entry.Hash
	return nil
}

// Sequence returns the number of successfully verified entries.
func (v *Verifier) Sequence() int {
	return v.sequence
}
