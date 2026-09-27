package team

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

const replayTestPolicy = "memory-policy-explain"

// replayL3Fixture mirrors the fixed L3 benchmark cases on a corpus large
// enough for BUG-01: positive transfer (a verified procedure that is also the
// strongest match), irrelevant high utility (a weak match with strong
// evidence), and stale/harmful memory (a strong match with causal failures).
func replayL3Fixture(t *testing.T) (*Coordinator, *contextstore.SQLiteRepository) {
	t.Helper()
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	var items []contextstore.ContextItem
	for i := range 300 {
		item := rankingItem(fmt.Sprintf("filler-%03d", i), 10)
		item.Content = fmt.Sprintf("generic filler note %d about coordinator workers", i)
		items = append(items, item)
	}
	add := func(id, content string) {
		item := rankingItem(id, 10)
		item.Content = content
		items = append(items, item)
	}
	add("transfer-verified", "rollback deploy procedure")
	add("transfer-plain-1", "rollback deploy notes for the api service")
	add("transfer-plain-2", "rollback deploy when health checks fail on the canary fleet")
	add("relevant-1", "sqlite schema version table")
	add("relevant-2", "sqlite schema readers check the version")
	add("relevant-3", "sqlite schema checksum mismatch aborts the open")
	add("relevant-4", "sqlite schema migrations are append only")
	add("relevant-5", "sqlite schema backups happen before migrating")
	add("irrelevant-high-utility", "a long unrelated operations note about cache warming dashboards alerting rotations and on call handoffs that mentions sqlite once and schema once")
	add("harmful", "sqlite schema shortcut that skips the migration backup")
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	evidence := func(id string, verified, failures int, positive, negative float64) {
		for i := range max(1, verified) {
			if _, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{
				IdempotencyKey: fmt.Sprintf("%s-%d", id, i), ContextItemID: id, PolicyVersion: replayTestPolicy, ProjectID: "project", TaskID: fmt.Sprintf("task-%d", i),
				AppliedDelta: 1, VerifiedSupportDelta: min(1, verified), CausalFailureDelta: failures, PositiveWeight: positive, NegativeWeight: negative, PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.1,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	evidence("transfer-verified", 3, 0, 3, 0)
	evidence("irrelevant-high-utility", 4, 0, 6, 0)
	evidence("harmful", 0, 2, 0, 3)
	adoptExplainTestPolicy(t, c, repo, agent.MemoryLearningOff, nil)
	return c, repo
}

func replayInput(queries ...string) RankingReplayInput {
	in := RankingReplayInput{ProjectID: "project", TeamID: "team", CandidateFusion: contextstore.FusionRRFNormalized}
	for _, query := range queries {
		in.Queries = append(in.Queries, RankingReplayQuery{Text: query, Source: "flag"})
	}
	return in
}

func TestReplayPersistentRankingOnL3Fixture(t *testing.T) {
	_, repo := replayL3Fixture(t)
	before, err := repo.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report, err := ReplayPersistentRanking(context.Background(), repo, replayInput("rollback deploy", "sqlite schema\nphase:execute", "   "))
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := repo.Revision(context.Background()); after != before {
		t.Fatalf("replay wrote context events: %d -> %d", before, after)
	}
	if report.QueryCount != 2 || !report.OutcomeMetricsAvailable || report.ItemsWithAggregates != 3 || report.BaselineFusion != contextstore.FusionLegacy || report.CandidateFusion != contextstore.FusionRRFNormalized {
		t.Fatalf("report header = %+v", report)
	}
	for _, query := range report.Queries {
		for name, comparison := range map[string]RankingReplayComparison{"relevance": query.Relevance, "reinforced": query.Reinforced} {
			if comparison.Jaccard < 0 || comparison.Jaccard > 1 || comparison.BaselineOutcome == nil || comparison.CandidateOutcome == nil {
				t.Fatalf("%s %s comparison = %+v", query.QueryHash, name, comparison)
			}
			if comparison.CandidateMaxBaseRelevance > 1 {
				t.Fatalf("%s %s: normalized base relevance %v above 1", query.QueryHash, name, comparison.CandidateMaxBaseRelevance)
			}
		}
		// The reinforced ranker never selects memory with a harm penalty,
		// whichever fusion ranks it.
		for _, selected := range [][]string{query.Reinforced.BaselineSelected, query.Reinforced.CandidateSelected} {
			if slices.Contains(selected, "harmful") {
				t.Fatalf("reinforced ranker selected harmful memory: %v", selected)
			}
		}
	}
	sqlite := report.Queries[1]
	if sqlite.Relevance.BaselineMaxBaseRelevance <= 1 {
		t.Fatalf("fixture should expose BUG-01 on the legacy scale: %+v", sqlite.Relevance)
	}
	// L3 irrelevant-high-utility: the legacy scale keeps the weak match out,
	// while rank-only normalized RRF lets MMR diversity and the utility
	// multiplier select it. The replay must surface that change; outcome
	// counts alone would read it as an improvement.
	if slices.Contains(sqlite.Reinforced.BaselineSelected, "irrelevant-high-utility") || !slices.Contains(sqlite.Reinforced.Added, "irrelevant-high-utility") {
		t.Fatalf("replay did not surface the L3 irrelevant-high-utility change: %+v", sqlite.Reinforced)
	}
	if sqlite.Reinforced.CandidateOutcome.WithVerifiedSupport <= sqlite.Reinforced.BaselineOutcome.WithVerifiedSupport {
		t.Fatalf("expected the candidate's outcome counts to look better despite the irrelevant pick: %+v", sqlite.Reinforced)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"rollback deploy", "sqlite schema", "cache warming"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("replay report leaked %q: %s", secret, encoded)
		}
	}
	t.Logf("relevance summary: %+v baseline=%+v candidate=%+v", report.Relevance, *report.Relevance.BaselineOutcome, *report.Relevance.CandidateOutcome)
	t.Logf("reinforced summary: %+v baseline=%+v candidate=%+v", report.Reinforced, *report.Reinforced.BaselineOutcome, *report.Reinforced.CandidateOutcome)
	for _, query := range report.Queries {
		t.Logf("query %s relevance base=%v cand=%v reinforced base=%v cand=%v", query.QueryHash[:12], query.Relevance.BaselineSelected, query.Relevance.CandidateSelected, query.Reinforced.BaselineSelected, query.Reinforced.CandidateSelected)
	}
}

func TestReplayPersistentRankingWithoutAggregates(t *testing.T) {
	_, repo := fusionRankingCoordinator(t, agent.MemoryLearningOff, "")
	report, err := ReplayPersistentRanking(context.Background(), repo, replayInput("sqlite schema"))
	if err != nil {
		t.Fatal(err)
	}
	if report.OutcomeMetricsAvailable || report.Relevance.BaselineOutcome != nil || report.Queries[0].Reinforced.CandidateOutcome != nil {
		t.Fatalf("outcome metrics reported without aggregates: %+v", report)
	}
}

func TestReplayPersistentRankingValidatesInput(t *testing.T) {
	_, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	for name, in := range map[string]RankingReplayInput{
		"no queries":      replayInput(),
		"blank queries":   replayInput(" ", "\n"),
		"unknown fusion":  {ProjectID: "project", TeamID: "team", CandidateFusion: "rrf", Queries: []RankingReplayQuery{{Text: "x"}}},
		"missing fusion":  {ProjectID: "project", TeamID: "team", Queries: []RankingReplayQuery{{Text: "x"}}},
		"missing team":    {ProjectID: "project", CandidateFusion: contextstore.FusionRRFNormalized, Queries: []RankingReplayQuery{{Text: "x"}}},
		"missing project": {TeamID: "team", CandidateFusion: contextstore.FusionRRFNormalized, Queries: []RankingReplayQuery{{Text: "x"}}},
	} {
		if _, err := ReplayPersistentRanking(context.Background(), repo, in); err == nil {
			t.Fatalf("%s: replay accepted invalid input", name)
		}
	}
}

func TestSessionReplayQueriesUseTaskGoals(t *testing.T) {
	workspace := t.TempDir()
	session := NewSession()
	session.Tasks = []*TodoItem{
		{ID: "a", Goal: "rollback the deploy"},
		{ID: "b", Desc: "fix the sqlite schema", Phase: PhaseVerify},
		{ID: "c"},
		{ID: "d", Goal: "rollback the deploy"},
	}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	queries := SessionReplayQueries(workspace)
	if len(queries) != 2 || queries[0].Source != "session_task" {
		t.Fatalf("queries = %+v", queries)
	}
	if goal, _, _ := strings.Cut(queries[1].Text, "\n"); goal != "fix the sqlite schema" || !strings.Contains(queries[1].Text, "phase:verify") {
		t.Fatalf("second query = %q", queries[1].Text)
	}
}
