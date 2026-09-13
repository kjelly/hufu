package improve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestHandoffStoreCreateGetAndIdempotentCreate(t *testing.T) {
	workspace := t.TempDir()
	handoff, err := NewImprovementHandoff(
		HandoffSkill,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "promotion_proposal", ID: "proposal", Revision: "draft-revision"},
		[]SourceBinding{
			{Ref: ArtifactRef{Kind: "context_item", ID: "source-b", Revision: "hash-b"}, ContentHash: "hash-b", ProjectID: "project", TeamID: "team"},
			{Ref: ArtifactRef{Kind: "context_item", ID: "source-a", Revision: "hash-a"}, ContentHash: "hash-a", ProjectID: "project", TeamID: "team"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(workspace)
	var audits []team.RunEvent
	store.audit = func(_ context.Context, event team.RunEvent) error {
		audits = append(audits, event)
		return nil
	}
	created, err := store.Create(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != handoff.ID || len(audits) != 1 || audits[0].Type != "handoff_created" {
		t.Fatalf("unexpected create result: %#v audits=%#v", created, audits)
	}
	if got, err := store.Get(handoff.ID); err != nil || got.ID != handoff.ID {
		t.Fatalf("get handoff: %#v %v", got, err)
	}
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	if len(audits) != 2 || audits[0].IdempotencyKey != audits[1].IdempotencyKey {
		t.Fatalf("idempotent create audit keys = %#v", audits)
	}

	data, err := marshalHandoff(handoff)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-2] = '2'
	if err := team.AtomicWriteFile(store.path(handoff.ID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(handoff.ID); err == nil {
		t.Fatal("tampered handoff was accepted")
	}
}

func TestHandoffStoreTransitionCASAndImmutableBindings(t *testing.T) {
	handoff, err := NewImprovementHandoff(
		HandoffMemoryPolicy,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "memory_policy_proposal", ID: "proposal", Revision: "proposal-revision"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return nil }
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	next := handoff
	next.Status = HandoffCandidateReady
	next.Candidate = &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate", Revision: "candidate-revision"}
	updated, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision = %d, want 2", updated.Revision)
	}
	if _, err := store.Transition(t.Context(), handoff.ID, handoff.Revision, next); err == nil {
		t.Fatal("stale CAS transition was accepted")
	}
	mutated := updated
	mutated.Scope.TeamID = "other-team"
	if _, err := store.Transition(t.Context(), handoff.ID, updated.Revision, mutated); err == nil {
		t.Fatal("immutable scope mutation was accepted")
	}
}

func TestHandoffStoreRetainsRecordWhenAuditFails(t *testing.T) {
	handoff, err := NewImprovementHandoff(
		HandoffConsolidation,
		HandoffScope{ProjectID: "project", TeamID: "team"},
		ArtifactRef{Kind: "consolidation_proposal", ID: "proposal", Revision: "proposal-revision"},
		[]SourceBinding{{Ref: ArtifactRef{Kind: "context_item", ID: "source", Revision: "hash"}, ContentHash: "hash", ProjectID: "project", TeamID: "team"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := NewHandoffStore(t.TempDir())
	store.audit = func(context.Context, team.RunEvent) error { return errors.New("audit unavailable") }
	if _, err := store.Create(t.Context(), handoff); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if _, err := store.Get(handoff.ID); err != nil {
		t.Fatalf("handoff was not retained after audit failure: %v", err)
	}
}

func TestMemoryPolicyOptimizationProposalIsDurableAndContentFree(t *testing.T) {
	workspace := t.TempDir()
	base := DefaultMemoryPolicySnapshot("base-policy")
	if _, err := WriteMemoryPolicySnapshot(workspace, base); err != nil {
		t.Fatal(err)
	}
	optimizer, err := ProposeMemoryPolicyOptimization("candidate-policy", base, Metrics{MemoryHarmfulUseRate: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := NewMemoryPolicyOptimizationProposal(workspace, "memory-proposal", optimizer, Metrics{MemoryHarmfulUseRate: 0.2})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMemoryPolicyOptimizationProposal(workspace, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Candidate.Kind != "memory_policy_snapshot" || loaded.Candidate.ID != optimizer.Candidate.ID {
		t.Fatalf("unexpected candidate ref: %#v", loaded.Candidate)
	}
	data, err := os.ReadFile(filepath.Join(ImprovementRoot(workspace), "memory-policies", "proposals", proposal.ID, "proposal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"retrieval"`)) || bytes.Contains(data, []byte(`"learning"`)) {
		t.Fatal("durable proposal inlined policy content")
	}
}
