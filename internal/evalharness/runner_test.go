package evalharness

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
