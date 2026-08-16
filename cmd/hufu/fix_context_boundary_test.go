package main

import (
	"context"
	"strings"
	"testing"
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
