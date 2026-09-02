package auditverify

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func chainedEvents(n int) []team.RunEvent {
	events := make([]team.RunEvent, 0, n)
	prevID, prevHash := "", ""
	for i := 0; i < n; i++ {
		id := "evt-" + string(rune('a'+i))
		payload := []byte(`{"n":` + string(rune('0'+i)) + `}`)
		hash := team.ComputeEventHash(prevHash, id, "task_created", "2026-01-01T00:00:00Z", payload)
		events = append(events, team.RunEvent{
			ID: id, PreviousID: prevID, PreviousHash: prevHash,
			Type: "task_created", Timestamp: "2026-01-01T00:00:00Z", Payload: payload, Hash: hash,
		})
		prevID, prevHash = id, hash
	}
	return events
}

func TestVerifyEventChainValidChain(t *testing.T) {
	chain := VerifyEventChain(chainedEvents(4))
	if !chain.Valid {
		t.Fatalf("valid chain reported invalid: %v", chain.Findings)
	}
	if chain.Events != 4 {
		t.Fatalf("Events = %d, want 4", chain.Events)
	}
}

func TestVerifyEventChainEmpty(t *testing.T) {
	chain := VerifyEventChain(nil)
	if !chain.Valid || chain.Events != 0 {
		t.Fatalf("empty chain = %#v, want valid/zero", chain)
	}
}

func TestVerifyEventChainMutatedPayload(t *testing.T) {
	events := chainedEvents(3)
	events[1].Payload = []byte(`{"n":99}`) // hash no longer matches this payload
	chain := VerifyEventChain(events)
	if chain.Valid {
		t.Fatal("mutated payload chain reported valid")
	}
}

func TestVerifyEventChainChangedPreviousHash(t *testing.T) {
	events := chainedEvents(3)
	events[2].PreviousHash = "deadbeef"
	chain := VerifyEventChain(events)
	if chain.Valid {
		t.Fatal("changed previous_hash chain reported valid")
	}
}

func TestVerifyEventChainDeletedMiddleEvent(t *testing.T) {
	events := chainedEvents(4)
	spliced := append(append([]team.RunEvent{}, events[:1]...), events[2:]...)
	chain := VerifyEventChain(spliced)
	if chain.Valid {
		t.Fatal("chain with a deleted middle event reported valid")
	}
}

func TestVerifyEventChainHeadIdentity(t *testing.T) {
	events := chainedEvents(5)
	chain := VerifyEventChain(events)
	last := events[len(events)-1]
	if chain.HeadID != last.ID || chain.HeadHash != last.Hash {
		t.Fatalf("head = (%s, %s), want (%s, %s)", chain.HeadID, chain.HeadHash, last.ID, last.Hash)
	}
}
