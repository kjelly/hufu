package evalharness

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

type querySignalFixture struct {
	ID                   string   `json:"id"`
	Signal               string   `json:"signal"`
	Hint                 string   `json:"hint"`
	Content              string   `json:"content"`
	LegacyExpectedIDs    []string `json:"legacy_expected_ids"`
	CandidateExpectedIDs []string `json:"candidate_expected_ids"`
}

func loadQuerySignalFixtures(t *testing.T) []querySignalFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "evals/context-routing/fixtures/query-signals-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion int                  `json:"schema_version"`
		OracleSource  string               `json:"oracle_source"`
		Cases         []querySignalFixture `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 || fixture.OracleSource != "synthetic_contract" {
		t.Fatal("invalid baseline oracle")
	}
	return fixture.Cases
}

// These cases freeze the known primary-lane misses before query planning.
// Hints here are synthetic data, not evidence that a runtime dependency or
// workset was accepted. The resolver's authorization is checked separately.
func TestContextQueryBaselineSignalMisses(t *testing.T) {
	for _, fixture := range loadQuerySignalFixtures(t) {
		t.Run(fixture.ID, func(t *testing.T) {
			workspace := t.TempDir()
			ctx := contextstore.WithEvaluationTime(t.Context(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
			if err := seedCanonicalContext(ctx, workspace, "project", "team", []ContextItemFixture{{ID: fixture.ID, Content: fixture.Content}}, false, agent.DefaultMemoryLearningPolicy()); err != nil {
				t.Fatal(err)
			}
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			request := team.ContextRequest{Goal: "inspect router", Phase: team.PhaseExecute, Trigger: team.ContextTriggerTaskDispatch, AgentRole: "worker"}
			switch fixture.Signal {
			case "constraints":
				request.Constraints = fixture.Hint
			case "verification":
				request.VerificationCriteria = fixture.Hint
			case "paths":
				request.TouchedPaths = []string{fixture.Hint}
			case "dependency":
				request.DependencyIDs = []string{"accepted-task"}
			default:
				t.Fatalf("unknown fixed fixture signal %q", fixture.Signal)
			}
			search := contextstore.SearchRequest{Query: request.RetrievalQuery(), Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Limit: 8}
			legacy, _, err := contextstore.HybridRetrieve(ctx, repo, nil, search)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, result := range legacy {
				ids = append(ids, result.Item.ID)
			}
			if !slices.Equal(ids, fixture.LegacyExpectedIDs) {
				t.Fatalf("legacy selection %v, want %v", ids, fixture.LegacyExpectedIDs)
			}
			search.Query = fixture.Hint
			positive, _, err := contextstore.HybridRetrieve(ctx, repo, nil, search)
			if err != nil || !slices.ContainsFunc(positive, func(item contextstore.SearchResult) bool { return item.Item.ID == fixture.ID }) {
				t.Fatalf("signal positive control failed: %v, %v", positive, err)
			}
		})
	}
}
