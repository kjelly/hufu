package inspect

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func governanceProposal(name string) contextstore.PromotionProposal {
	draft := "---\nname: " + name + "\ndescription: Governance fixture.\n---\n1. One.\n2. Two."
	p := contextstore.PromotionProposal{ProjectID: "project", TeamID: "team", Type: contextstore.PromotionTypeSkill, TargetPath: "skills/" + name + "/SKILL.md", Draft: draft, DraftHash: contextstore.HashPromotionContent(draft), PolicyVersion: "policy-v1", Sources: []contextstore.PromotionSourceSnapshot{{ContextItemID: name, ContentHash: "hash", AggregateRevision: 1}}, Status: contextstore.PromotionStatusProposed}
	p.ID = contextstore.PromotionProposalID(p)
	return p
}

func governanceEvent(key string) contextstore.PromotionOutboxEvent {
	return contextstore.PromotionOutboxEvent{IdempotencyKey: key, EventType: "fixture", Payload: []byte(`{}`)}
}

func TestInspectLearningCountsRejectedStaleAndEdited(t *testing.T) {
	ctx := t.Context()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	policy.PolicyVersion = "policy-v1"
	snapshot, err := json.Marshal(map[string]any{"id": "policy-v1", "revision_hash": "revision-1", "learning": policy})
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.SaveMemoryPolicyVersion(ctx, "policy-v1", snapshot, "revision-1", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	create := func(name string) contextstore.PromotionProposal {
		p, _, createErr := repo.CreatePromotion(ctx, governanceProposal(name), governanceEvent(name+":create"))
		if createErr != nil {
			t.Fatal(createErr)
		}
		return p
	}
	transition := func(p contextstore.PromotionProposal, to contextstore.PromotionStatus) {
		if _, transitionErr := repo.TransitionPromotion(ctx, p.ID, "project", "team", to, "reason", governanceEvent(p.ID+":"+string(to))); transitionErr != nil {
			t.Fatal(transitionErr)
		}
	}
	create("still-proposed")
	transition(create("rejected-one"), contextstore.PromotionStatusRejected)
	transition(create("stale-one"), contextstore.PromotionStatusStale)
	unedited := create("applied-unedited")
	transition(unedited, contextstore.PromotionStatusApproved)
	transition(unedited, contextstore.PromotionStatusApplied)
	edited := create("applied-edited")
	editedDraft := edited.Draft + "\n3. Three."
	if _, err = repo.UpdatePromotionDraft(ctx, edited.ID, "project", "team", editedDraft, contextstore.HashPromotionContent(editedDraft), governanceEvent("edit")); err != nil {
		t.Fatal(err)
	}
	transition(edited, contextstore.PromotionStatusApproved)
	transition(edited, contextstore.PromotionStatusApplied)
	legacy := create("applied-legacy")
	transition(legacy, contextstore.PromotionStatusApproved)
	transition(legacy, contextstore.PromotionStatusApplied)
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	// A proposal created before migration 10 has no generated draft hash.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE promotion_proposals SET generated_draft_hash='' WHERE id=?", legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	view := InspectLearning(ctx, workspace, "project", "team", "observe")
	checks := []struct {
		name string
		got  *int64
		want int64
	}{
		{name: "proposed", got: view.ProposedPromotions, want: 1},
		{name: "applied", got: view.AppliedPromotions, want: 3},
		{name: "rejected", got: view.RejectedPromotions, want: 1},
		{name: "stale", got: view.StalePromotions, want: 1},
		{name: "applied edited", got: view.AppliedEditedPromotions, want: 1},
		{name: "applied edit unknown", got: view.AppliedEditUnknownPromotions, want: 1},
	}
	for _, check := range checks {
		if value(check.got) != check.want {
			t.Fatalf("%s = %d, want %d (view %#v)", check.name, value(check.got), check.want, view)
		}
	}
}

func TestInspectLearningCountsOpenConflicts(t *testing.T) {
	ctx := t.Context()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	var items []contextstore.ContextItem
	for _, id := range []string{"a", "b", "c"} {
		item := contextstore.ContextItem{ID: id, Kind: contextstore.ContextDecision, Content: "memory " + id, Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
		if err = repo.Append(ctx, item); err != nil {
			t.Fatal(err)
		}
		stored, getErr := repo.Get(ctx, id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		items = append(items, stored)
	}
	for _, pair := range [][2]int{{0, 1}, {0, 2}} {
		a, b := items[pair[0]], items[pair[1]]
		j := contextstore.PairJudgment{ProjectID: "project", TeamID: "team", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: contextstore.PairVerdictContradicts, JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
		contextstore.NormalizePairJudgment(&j)
		if _, _, err = repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
			t.Fatal(err)
		}
	}
	// Superseding c resolves the a/c conflict.
	if err = repo.MarkSuperseded(ctx, []string{"c"}, "b"); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	view := InspectLearning(ctx, workspace, "project", "team", "")
	if value(view.OpenConflicts) != 1 {
		t.Fatalf("open conflicts = %d, want 1 (view %#v)", value(view.OpenConflicts), view)
	}

	// A store older than migration 11 reports conflicts as unknown.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "DROP TABLE context_pair_judgments; DELETE FROM schema_migrations WHERE version >= 11"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	old := InspectLearning(ctx, workspace, "project", "team", "")
	if old.OpenConflicts != nil || old.Status != "available" || old.UnavailableReason != "" {
		t.Fatalf("pre-migration view = %#v, want unknown conflicts without degrading status", old)
	}
}
