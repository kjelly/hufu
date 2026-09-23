package context

import (
	"context"
	"path/filepath"
	"testing"
)

func editTrackingProposal() PromotionProposal {
	draft := "---\nname: tracked\ndescription: Tracked skill.\n---\n1. One.\n2. Two."
	p := PromotionProposal{ProjectID: "p", TeamID: "t", Type: PromotionTypeSkill, TargetPath: "skills/tracked/SKILL.md", Draft: draft, DraftHash: HashPromotionContent(draft), PolicyVersion: "memory-policy-v1", Sources: []PromotionSourceSnapshot{{ContextItemID: "item", ContentHash: "hash", AggregateRevision: 1}}, Status: PromotionStatusProposed}
	p.ID = PromotionProposalID(p)
	return p
}

func TestCreatePromotionRecordsGeneratedDraftHash(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	p := editTrackingProposal()
	stored, created, err := repo.CreatePromotion(ctx, p, PromotionOutboxEvent{IdempotencyKey: "create", EventType: "memory_promotion_proposed", Payload: []byte(`{}`)})
	if err != nil || !created {
		t.Fatalf("CreatePromotion created=%v err=%v", created, err)
	}
	if stored.GeneratedDraftHash != p.DraftHash {
		t.Fatalf("generated draft hash = %q, want %q", stored.GeneratedDraftHash, p.DraftHash)
	}
	edited := stored.Draft + "\n3. Three."
	updated, err := repo.UpdatePromotionDraft(ctx, p.ID, "p", "t", edited, HashPromotionContent(edited), PromotionOutboxEvent{IdempotencyKey: "edit", EventType: "memory_promotion_edited", Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if wasEdited, known := updated.DraftEdited(); !known || !wasEdited || updated.GeneratedDraftHash != p.DraftHash {
		t.Fatalf("after edit DraftEdited=%v/%v generated=%q", wasEdited, known, updated.GeneratedDraftHash)
	}
}

func TestDraftEditedStates(t *testing.T) {
	cases := []struct {
		name                string
		draftHash           string
		generated           string
		wantEdited, wantKnw bool
	}{
		{name: "before migration 10", draftHash: "a", generated: "", wantEdited: false, wantKnw: false},
		{name: "unedited", draftHash: "a", generated: "a", wantEdited: false, wantKnw: true},
		{name: "edited", draftHash: "b", generated: "a", wantEdited: true, wantKnw: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited, known := PromotionProposal{DraftHash: tc.draftHash, GeneratedDraftHash: tc.generated}.DraftEdited()
			if edited != tc.wantEdited || known != tc.wantKnw {
				t.Fatalf("DraftEdited = %v/%v, want %v/%v", edited, known, tc.wantEdited, tc.wantKnw)
			}
		})
	}
}

func TestReadOnlyPromotionsBeforeMigration10(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repo, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	p := editTrackingProposal()
	if _, _, err = repo.CreatePromotion(ctx, p, PromotionOutboxEvent{IdempotencyKey: "create", EventType: "memory_promotion_proposed", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Downgrade the store to schema 9 as an older hufu would have left it.
	if _, err = repo.db.ExecContext(ctx, `ALTER TABLE promotion_proposals DROP COLUMN generated_draft_hash; DELETE FROM schema_migrations WHERE version >= 10`); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	list, err := readOnly.ListPromotions(ctx, "p", "t")
	if err != nil {
		t.Fatalf("ListPromotions on schema 9: %v", err)
	}
	got, err := readOnly.GetPromotion(ctx, p.ID, "p", "t")
	if err != nil {
		t.Fatalf("GetPromotion on schema 9: %v", err)
	}
	for _, proposal := range append(list, got) {
		if _, known := proposal.DraftEdited(); known {
			t.Fatalf("proposal %s edit state should be unknown before migration 10", proposal.ID)
		}
	}
}

func TestListAppliedSkillPromotionsFiltersTypeAndStatus(t *testing.T) {
	ctx := context.Background()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	event := func(key string) PromotionOutboxEvent {
		return PromotionOutboxEvent{IdempotencyKey: key, EventType: "fixture", Payload: []byte(`{}`)}
	}
	create := func(name, teamID string, typ PromotionType, target string) PromotionProposal {
		draft := "---\nname: " + name + "\ndescription: d\n---\n1. One.\n2. Two."
		if typ != PromotionTypeSkill {
			draft = "## Policy\n\n- Rule."
		}
		p := PromotionProposal{ProjectID: "p", TeamID: teamID, Type: typ, TargetPath: target, Draft: draft, DraftHash: HashPromotionContent(draft), PolicyVersion: "v1", Sources: []PromotionSourceSnapshot{{ContextItemID: name, ContentHash: "h", AggregateRevision: 1}}, Status: PromotionStatusProposed}
		p.ID = PromotionProposalID(p)
		if _, _, err := repo.CreatePromotion(ctx, p, event(name)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	apply := func(p PromotionProposal) {
		for _, to := range []PromotionStatus{PromotionStatusApproved, PromotionStatusApplied} {
			if _, err := repo.TransitionPromotion(ctx, p.ID, "p", p.TeamID, to, "", event(p.ID+string(to))); err != nil {
				t.Fatal(err)
			}
		}
	}
	apply(create("applied-skill", "t", PromotionTypeSkill, "skills/applied-skill/SKILL.md"))
	create("proposed-skill", "t", PromotionTypeSkill, "skills/proposed-skill/SKILL.md")
	apply(create("policy", "t", PromotionTypeTeamPolicy, "coordinator.md"))
	apply(create("other-team-skill", "other", PromotionTypeSkill, "skills/other-team-skill/SKILL.md"))
	got, err := repo.ListAppliedSkillPromotions(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TargetPath != "skills/applied-skill/SKILL.md" || got[0].Draft != "" || got[0].AppliedAt == nil {
		t.Fatalf("applied skill promotions = %+v", got)
	}
}
