package context

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPromotionRepositoryLifecyclePersistsAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repo, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	p := PromotionProposal{ProjectID: "p", TeamID: "team", Type: PromotionTypeTeamPolicy, TargetPath: "coordinator.md", TargetBaseHash: "base", Draft: "## Policy\nDo it.", PolicyVersion: "memory-policy-v1", Sources: []PromotionSourceSnapshot{{ContextItemID: "ctx-1", ContentHash: "hash", AggregateRevision: 3}}, Status: PromotionStatusProposed}
	p.ID = PromotionProposalID(p)
	event := PromotionOutboxEvent{IdempotencyKey: p.ID + ":proposed", EventType: "memory_promotion_proposed", Payload: json.RawMessage(`{"schema_version":1}`)}
	got, created, err := repo.CreatePromotion(ctx, p, event)
	if err != nil {
		t.Fatal(err)
	}
	if !created || got.ID != p.ID {
		t.Fatalf("create = %#v, %v", got, created)
	}
	edited, err := repo.UpdatePromotionDraft(ctx, p.ID, "p", "team", "changed", HashPromotionContent("changed"), PromotionOutboxEvent{IdempotencyKey: p.ID + ":edited", EventType: "memory_promotion_edited", Payload: json.RawMessage(`{"schema_version":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Draft != "changed" {
		t.Fatalf("draft=%q", edited.Draft)
	}
	approved, err := repo.TransitionPromotion(ctx, p.ID, "p", "team", PromotionStatusApproved, "", PromotionOutboxEvent{IdempotencyKey: p.ID + ":approved", EventType: "memory_promotion_approved", Payload: json.RawMessage(`{"schema_version":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != PromotionStatusApproved {
		t.Fatal(approved.Status)
	}
	if _, err = repo.UpdatePromotionDraft(ctx, p.ID, "p", "team", "again", HashPromotionContent("again"), PromotionOutboxEvent{IdempotencyKey: "bad", EventType: "memory_promotion_edited", Payload: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("approved proposal must not be editable")
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	persisted, err := repo.GetPromotion(ctx, p.ID, "p", "team")
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != PromotionStatusApproved || persisted.Draft != "changed" || len(persisted.Sources) != 1 {
		t.Fatalf("persisted=%#v", persisted)
	}
	events, err := repo.PendingPromotionEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("outbox events=%d want 3", len(events))
	}
	_, created, err = repo.CreatePromotion(ctx, p, event)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("deterministic rerun created duplicate")
	}
	again, err := repo.GetPromotion(ctx, p.ID, "p", "team")
	if err != nil {
		t.Fatal(err)
	}
	if again.Draft != "changed" || again.Status != PromotionStatusApproved {
		t.Fatal("rerun overwrote operator state")
	}
}
