// Package eventchain owns the durable event hash and linkage contract shared
// by runtime readers and workspace migration verification.
package eventchain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// FramedSchemaVersion is the first event schema with length-delimited hashes.
const FramedSchemaVersion = 3

// Entry contains the persisted fields that participate in event-chain
// verification.
type Entry struct {
	SchemaVersion int             `json:"schema_version,omitempty"`
	ID            string          `json:"id"`
	PreviousID    string          `json:"previous_id,omitempty"`
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	PreviousHash  string          `json:"previous_hash,omitempty"`
	Hash          string          `json:"hash,omitempty"`
}

// Verifier validates a root-based event chain in sequence.
type Verifier struct {
	sequence     int
	previousID   string
	previousHash string
}

// ComputeHash computes the historical digest for schemas before framing.
func ComputeHash(previousHash, id, eventType, timestamp string, payload json.RawMessage) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(previousHash))
	_, _ = hash.Write([]byte(id))
	_, _ = hash.Write([]byte(eventType))
	_, _ = hash.Write([]byte(timestamp))
	_, _ = hash.Write(payload)
	return hex.EncodeToString(hash.Sum(nil))
}

// Digest computes the digest using the event's persisted schema version.
func (e Entry) Digest() string {
	if e.SchemaVersion < FramedSchemaVersion {
		return ComputeHash(e.PreviousHash, e.ID, e.Type, e.Timestamp, e.Payload)
	}

	hash := sha256.New()
	_, _ = io.WriteString(hash, "hufu:event-chain\x00")
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(e.SchemaVersion))
	_, _ = hash.Write(size[:])
	for _, field := range [...]string{e.PreviousHash, e.ID, e.Type, e.Timestamp} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = io.WriteString(hash, field)
	}
	binary.BigEndian.PutUint64(size[:], uint64(len(e.Payload)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write(e.Payload)
	return hex.EncodeToString(hash.Sum(nil))
}

// VerifyDigest checks the event identity and digest without assuming linkage.
func (e Entry) VerifyDigest() error {
	if e.ID == "" || e.Type == "" {
		return errors.New("event ID and type are required")
	}
	if e.Hash != e.Digest() {
		return errors.New("hash invalid")
	}
	return nil
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
	if err := entry.VerifyDigest(); err != nil {
		return fmt.Errorf("event %d (%s): %w", v.sequence, entry.ID, err)
	}
	v.sequence++
	v.previousID = entry.ID
	v.previousHash = entry.Hash
	return nil
}

// ResumeVerifier returns a verifier positioned after sequence verified
// entries, the last of which had previousID and previousHash, so a caller
// that already verified a prefix verifies only the entries that follow it.
func ResumeVerifier(sequence int, previousID, previousHash string) Verifier {
	return Verifier{sequence: sequence, previousID: previousID, previousHash: previousHash}
}

// Sequence returns the number of successfully verified entries.
func (v *Verifier) Sequence() int {
	return v.sequence
}
