package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestFixDirectAnalysisIsDeterministicWithoutSidecar(t *testing.T) {
	result, err := runFixAnalysisDirect(context.Background(), "why did verification fail", "repair the service", &fixData{Reliability: "verification_failed=1"}, "team", "model")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Deterministic Fix Analysis", "no model was called", "verification outcomes"} {
		if !strings.Contains(result, want) {
			t.Fatalf("deterministic result missing %q: %s", want, result)
		}
	}
}

func TestFixAnalysisFailsClosedWithoutContextBoundary(t *testing.T) {
	_, err := runFixAnalysis(context.Background(), &teamContext{
		coordinator: &team.Coordinator{},
		session:     &team.TeamSession{Config: agent.TeamConfig{Name: "fix-test"}},
	}, "why", "task", nil)
	if err == nil || !strings.Contains(err.Error(), "provider boundary") {
		t.Fatalf("fix analysis error = %v, want explicit provider-boundary failure", err)
	}
}
