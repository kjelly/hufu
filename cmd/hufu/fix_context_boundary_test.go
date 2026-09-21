package main

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
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

func TestBuildFixPromptBoundsLargeSessionDeterministically(t *testing.T) {
	large := strings.Repeat("execution evidence ", 2000)
	data := &fixData{
		TeamYAML:      large,
		CoordinatorMD: large,
		AgentMDs: map[string]string{
			"zeta":  large,
			"alpha": large,
		},
		STM:         large,
		LTM:         large,
		SessionJSON: large,
		Reliability: large,
		SessionLog:  large,
		SessionMD:   large,
		TaskHistory: map[string]string{
			"worker-z": large,
			"worker-a": large,
		},
	}
	first, err := buildFixPrompt(strings.Repeat("why ", 2000), strings.Repeat("task ", 2000), data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildFixPrompt(strings.Repeat("why ", 2000), strings.Repeat("task ", 2000), data)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("fix prompt changed across identical builds")
	}
	if got, want := utf8.RuneCountInString(first), sidecar.CompactorProfile.InputRuneLimit(); got > want {
		t.Fatalf("fix prompt = %d runes, want at most %d", got, want)
	}
	for _, want := range []string{
		"## Problem to Investigate",
		"## Original Task",
		"## Team Configuration",
		"## Coordinator Instructions",
		"## Agent Definition: alpha.md",
		"## Agent Definition: zeta.md",
		"## Session History",
		"## Execution Log",
		"## Worker Task History: worker-a",
		"## Worker Task History: worker-z",
		"runes omitted",
		"Provide your analysis now",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("bounded fix prompt missing %q", want)
		}
	}
	if strings.Index(first, "alpha.md") > strings.Index(first, "zeta.md") {
		t.Fatal("agent definitions are not sorted deterministically")
	}
	if strings.Index(first, "worker-a") > strings.Index(first, "worker-z") {
		t.Fatal("task histories are not sorted deterministically")
	}
}
