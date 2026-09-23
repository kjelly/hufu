package improve

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func seedAppliedSkillPromotion(t *testing.T, workspace, teamID, skill string) time.Time {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	draft := "---\nname: " + skill + "\ndescription: Promoted.\n---\n1. One.\n2. Two."
	p := contextstore.PromotionProposal{ProjectID: "project", TeamID: teamID, Type: contextstore.PromotionTypeSkill, TargetPath: "skills/" + skill + "/SKILL.md", Draft: draft, DraftHash: contextstore.HashPromotionContent(draft), PolicyVersion: "memory-policy-v1", Sources: []contextstore.PromotionSourceSnapshot{{ContextItemID: "source", ContentHash: "hash", AggregateRevision: 1}}, Status: contextstore.PromotionStatusProposed}
	p.ID = contextstore.PromotionProposalID(p)
	event := func(key string) contextstore.PromotionOutboxEvent {
		return contextstore.PromotionOutboxEvent{IdempotencyKey: key, EventType: "fixture", Payload: []byte(`{}`)}
	}
	if _, _, err = repo.CreatePromotion(ctx, p, event("create")); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.TransitionPromotion(ctx, p.ID, "project", teamID, contextstore.PromotionStatusApproved, "", event("approve")); err != nil {
		t.Fatal(err)
	}
	applied, err := repo.TransitionPromotion(ctx, p.ID, "project", teamID, contextstore.PromotionStatusApplied, "", event("apply"))
	if err != nil || applied.AppliedAt == nil {
		t.Fatalf("apply transition = %+v err=%v", applied, err)
	}
	return *applied.AppliedAt
}

func skillEvent(runID, taskID, status string, at string, skills ...string) team.ExecutionEvent {
	return team.ExecutionEvent{Timestamp: at, RunID: runID, Team: "dev", TaskID: taskID, Attempt: 1, Status: status, Skills: skills}
}

func TestPromotedSkillUsageCountsOnlyTasksAfterApply(t *testing.T) {
	workspace := t.TempDir()
	teamDir := writeCacheAnalyticsTeam(t)
	appliedAt := seedAppliedSkillPromotion(t, workspace, "dev", "review-helper")
	before := appliedAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	after := appliedAt.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		skillEvent("run-1", "before", "done", before, "review-helper"),
		skillEvent("run-1", "after-done", "done", after, "review-helper"),
		skillEvent("run-1", "after-error", "error", after, "review-helper"),
		skillEvent("run-1", "other-skill", "done", after, "other"),
		skillEvent("run-1", "untimed", "done", "not-a-time", "review-helper"),
	})
	report, err := AnalyzeRecent(workspace, "dev", teamDir, 10)
	if err != nil {
		t.Fatal(err)
	}
	if report.PromotedSkillsUnavailable != "" || len(report.PromotedSkills) != 1 {
		t.Fatalf("promoted skills = %+v unavailable=%q", report.PromotedSkills, report.PromotedSkillsUnavailable)
	}
	got := report.PromotedSkills[0]
	if got.SkillName != "review-helper" || got.TasksSinceApplied != 2 || got.Done != 1 || got.Error != 1 || got.UntimedTasks != 1 {
		t.Fatalf("usage = %+v", got)
	}
	markdown := Markdown(report)
	if !strings.Contains(markdown, "## Promoted skills (association only)") || !strings.Contains(markdown, "| review-helper |") {
		t.Fatalf("markdown missing promoted skills section:\n%s", markdown)
	}
}

func TestPromotedSkillsOmittedWithoutContextStore(t *testing.T) {
	workspace := t.TempDir()
	teamDir := writeCacheAnalyticsTeam(t)
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{skillEvent("run-1", "task", "done", "2026-07-12T10:00:00Z", "review-helper")})
	report, err := AnalyzeRecent(workspace, "dev", teamDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.PromotedSkills != nil || report.PromotedSkillsUnavailable != "" || strings.Contains(Markdown(report), "Promoted skills") {
		t.Fatalf("report without context store = %+v / %q", report.PromotedSkills, report.PromotedSkillsUnavailable)
	}
}

func TestPromotedSkillsUnavailableOnQueryFailure(t *testing.T) {
	workspace := t.TempDir()
	teamDir := writeCacheAnalyticsTeam(t)
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{skillEvent("run-1", "task", "done", "2026-07-12T10:00:00Z")})
	if err := os.WriteFile(filepath.Join(workspace, "context.sqlite"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := AnalyzeRecent(workspace, "dev", teamDir, 1)
	if err != nil {
		t.Fatalf("a broken context store must not fail the report: %v", err)
	}
	if report.PromotedSkills != nil || report.PromotedSkillsUnavailable != "query_failed" {
		t.Fatalf("report = %+v / %q, want query_failed", report.PromotedSkills, report.PromotedSkillsUnavailable)
	}
}
