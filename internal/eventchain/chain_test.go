package eventchain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifierRejectsBrokenRootLinkageAndHash(t *testing.T) {
	payload := json.RawMessage(`{"value":1}`)
	root := Entry{ID: "one", Type: "created", Timestamp: "2026-01-01T00:00:00Z", Payload: payload}
	root.Hash = ComputeHash(root.PreviousHash, root.ID, root.Type, root.Timestamp, root.Payload)
	var verifier Verifier
	if err := verifier.Verify(root); err != nil {
		t.Fatal(err)
	}

	brokenLink := Entry{ID: "two", PreviousID: "wrong", PreviousHash: root.Hash, Type: "updated", Timestamp: root.Timestamp, Payload: payload}
	brokenLink.Hash = ComputeHash(brokenLink.PreviousHash, brokenLink.ID, brokenLink.Type, brokenLink.Timestamp, brokenLink.Payload)
	if err := verifier.Verify(brokenLink); err == nil || !strings.Contains(err.Error(), "does not continue hash chain") {
		t.Fatalf("broken linkage error = %v", err)
	}
	if verifier.Sequence() != 1 {
		t.Fatalf("failed verification advanced sequence to %d", verifier.Sequence())
	}

	brokenHash := brokenLink
	brokenHash.PreviousID = root.ID
	brokenHash.Hash = "invalid"
	if err := verifier.Verify(brokenHash); err == nil || !strings.Contains(err.Error(), "hash invalid") {
		t.Fatalf("broken hash error = %v", err)
	}
}

func TestVerifierRequiresRootWithoutPredecessor(t *testing.T) {
	entry := Entry{ID: "one", PreviousID: "previous", PreviousHash: "hash", Type: "created", Timestamp: "2026-01-01T00:00:00Z"}
	entry.Hash = ComputeHash(entry.PreviousHash, entry.ID, entry.Type, entry.Timestamp, entry.Payload)
	var verifier Verifier
	if err := verifier.Verify(entry); err == nil || !strings.Contains(err.Error(), "first event") {
		t.Fatalf("root predecessor error = %v", err)
	}
}
