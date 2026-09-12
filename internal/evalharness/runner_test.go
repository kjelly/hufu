package evalharness

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return dir
}

func TestRunSuiteCoreLifecycleSingleTaskUnverified(t *testing.T) {
	fixturePath := filepath.Join(repoRoot(t), "evals", "core-lifecycle", "cases.yaml")
	fixture, err := LoadSuiteFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadSuiteFixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := RunSuite(ctx, fixture, "single-task-unverified")
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if len(result.Cases) != 1 {
		t.Fatalf("len(result.Cases) = %d, want 1", len(result.Cases))
	}
	c := result.Cases[0]
	if !c.Passed {
		t.Fatalf("case %s did not pass; findings: %+v", c.CaseID, c.Findings)
	}
	if c.RunOutcome != "unverified" {
		t.Errorf("RunOutcome = %q, want %q", c.RunOutcome, "unverified")
	}
}

// TestRunAllEvalSuites runs every case in every suite fixture under evals/,
// so a new suite added under evals/<name>/cases.yaml is exercised by `go
// test` automatically without a new hand-written test function -- this is
// the Go-level equivalent of `hufu eval run ./evals/<name>` per suite.
func TestRunAllEvalSuites(t *testing.T) {
	evalsRoot := filepath.Join(repoRoot(t), "evals")
	entries, err := os.ReadDir(evalsRoot)
	if err != nil {
		t.Fatalf("read evals root %s: %v", evalsRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		suiteDir := filepath.Join(evalsRoot, entry.Name())
		t.Run(entry.Name(), func(t *testing.T) {
			paths, err := DiscoverSuiteFixtures(suiteDir)
			if err != nil {
				t.Fatalf("DiscoverSuiteFixtures(%s): %v", suiteDir, err)
			}
			if len(paths) == 0 {
				t.Fatalf("no suite fixtures found in %s", suiteDir)
			}
			for _, path := range paths {
				fixture, err := LoadSuiteFixture(path)
				if err != nil {
					t.Fatalf("LoadSuiteFixture(%s): %v", path, err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				result, err := RunSuite(ctx, fixture, "")
				cancel()
				if err != nil {
					t.Fatalf("RunSuite(%s): %v", path, err)
				}
				for _, c := range result.Cases {
					if !c.Passed {
						t.Errorf("case %s/%s did not pass; findings: %+v", fixture.Name, c.CaseID, c.Findings)
					}
				}
			}
		})
	}
}

// blockingHandler never responds; it only unblocks when the request's own
// context is cancelled (which happens when the client -- runCaseWithHandler's
// context.WithTimeout -- gives up), so it deterministically exercises the
// harness's own per-case timeout without waiting out the real default.
type blockingHandler struct{}

func (blockingHandler) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

func TestEvalTimeout(t *testing.T) {
	original := defaultCaseTimeout
	defaultCaseTimeout = 200 * time.Millisecond
	t.Cleanup(func() { defaultCaseTimeout = original })

	fixturePath := filepath.Join(repoRoot(t), "evals", "core-lifecycle", "cases.yaml")
	fixture, err := LoadSuiteFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadSuiteFixture: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	result, err := runCaseWithHandler(context.Background(), fixture, fixture.Cases[0], blockingHandler{}, nil)
	if err != nil {
		t.Fatalf("runCaseWithHandler: %v", err)
	}
	if time.Now().After(deadline) {
		t.Fatal("runCaseWithHandler took far longer than defaultCaseTimeout; the per-case timeout did not bound the run")
	}
	found := false
	for _, f := range result.Findings {
		if f.Dimension == "timeout" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a \"timeout\" finding for a case whose provider never responds; findings: %+v", result.Findings)
	}
	if result.Passed {
		t.Error("a timed-out case must not report Passed: true")
	}
}

func TestEvalNeverCallsNetworkDriverInOfflineMode(t *testing.T) {
	fixturePath := filepath.Join(repoRoot(t), "evals", "core-lifecycle", "cases.yaml")
	fixture, err := LoadSuiteFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadSuiteFixture: %v", err)
	}
	session, err := team.LoadTeam(fixture.TeamDir(), nil, nil, nil)
	if err != nil {
		t.Fatalf("load team: %v", err)
	}
	// A bundled eval team must never declare its own named providers: those
	// are resolved independently of the scripted default provider URL
	// runCase wires in, so one could route a "deterministic" case at a real
	// network host. The harness's only sanctioned egress point is the
	// per-case httptest.Server.
	if len(session.Config.Providers) != 0 {
		t.Errorf("fixture team %s declares %d named provider(s); an offline eval team must rely solely on the harness's scripted default provider URL", fixture.TeamDir(), len(session.Config.Providers))
	}
}

func TestAssertMemoryAggregatesDistinguishesExposureFromCredit(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	item := contextstore.ContextItem{
		ID:         "retrieved-only",
		Kind:       contextstore.ContextPattern,
		Content:    "calibrate quartz beacon safely",
		Scope:      contextstore.Scope{ProjectID: "project", TeamID: "team"},
		Authority:  contextstore.AuthorityRepository,
		TrustLevel: contextstore.TrustTrusted,
		Priority:   contextstore.PriorityHigh,
		Confidence: 1,
		Lifecycle:  contextstore.LifecycleConfirmed,
	}
	if err := repo.Append(t.Context(), item); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := repo.ApplyExperienceObservation(t.Context(), contextstore.ExperienceObservation{
		IdempotencyKey: "retrieval-1",
		ContextItemID:  item.ID,
		PolicyVersion:  "memory-policy-v1",
		ExposureDelta:  1,
	}); err != nil {
		t.Fatalf("ApplyExperienceObservation: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expect := MemoryAggregateExpect{
		ContextItemID:    item.ID,
		PolicyVersion:    "memory-policy-v1",
		MinExposureCount: new(1),
		ConsultedCount:   new(0),
		AppliedCount:     new(0),
		PositiveWeight:   new(0.0),
		NegativeWeight:   new(0.0),
	}
	if findings := assertMemoryAggregates(t.Context(), workspace, []MemoryAggregateExpect{expect}); len(findings) != 0 {
		t.Fatalf("assertMemoryAggregates returned findings for retrieved-only memory: %+v", findings)
	}

	expect.PositiveWeight = new(1.0)
	findings := assertMemoryAggregates(t.Context(), workspace, []MemoryAggregateExpect{expect})
	if len(findings) != 1 || findings[0].Dimension != "memory-aggregate.retrieved-only.positive-weight" {
		t.Fatalf("credit mismatch findings = %+v, want positive-weight finding", findings)
	}
}
