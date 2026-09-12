package evalharness

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

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
