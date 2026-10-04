package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/evalharness"
)

func TestEvalListDefaultsToRepositoryEvalRoot(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	t.Chdir(repoRoot)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runEvalList(cmd, nil); err != nil {
		t.Fatalf("runEvalList: %v", err)
	}
	lines := strings.Fields(output.String())
	if len(lines) != 33 {
		t.Fatalf("listed %d cases, want 33: %s", len(lines), output.String())
	}
	if !strings.Contains(output.String(), "core-lifecycle/single-task-unverified") {
		t.Fatalf("default eval listing omitted core lifecycle case: %s", output.String())
	}
}

func TestEvalPerformanceGateCommand(t *testing.T) {
	baseline := evalCommandPerformanceSamples(100, 100, 2)
	candidate := evalCommandPerformanceSamples(90, 90, 2)
	baselinePath := writeEvalPerformanceSamples(t, "baseline.json", baseline)
	candidatePath := writeEvalPerformanceSamples(t, "candidate.json", candidate)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runEvalPerformanceGate(cmd, []string{baselinePath, candidatePath}); err != nil {
		t.Fatalf("runEvalPerformanceGate: %v", err)
	}
	if !strings.Contains(output.String(), `"passed": true`) {
		t.Fatalf("passing report = %s", output.String())
	}

	regressed := evalCommandPerformanceSamples(200, 200, 3)
	regressedPath := writeEvalPerformanceSamples(t, "regressed.json", regressed)
	output.Reset()
	if err := runEvalPerformanceGate(cmd, []string{baselinePath, regressedPath}); err == nil {
		t.Fatal("regressed candidate did not fail the gate")
	}
	if !strings.Contains(output.String(), `"passed": false`) {
		t.Fatalf("failing report = %s", output.String())
	}
}

func evalCommandPerformanceSamples(duration, tokens, calls int64) []evalharness.PerformanceSample {
	samples := make([]evalharness.PerformanceSample, 0, len(evalharness.RequiredPerformanceTaskClasses)*5)
	for _, taskClass := range evalharness.RequiredPerformanceTaskClasses {
		for range 5 {
			samples = append(samples, evalharness.PerformanceSample{
				TaskClass: taskClass, Duration: time.Duration(duration), Accepted: true,
				TotalTokens: tokens, ProviderCalls: calls,
			})
		}
	}
	return samples
}

func writeEvalPerformanceSamples(t *testing.T, name string, samples []evalharness.PerformanceSample) string {
	t.Helper()
	encoded, err := json.Marshal(samples)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
