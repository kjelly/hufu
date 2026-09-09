package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestResolveAndCheckModelSeparatesWorkerAndCoordinatorTargets(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{
		WorkerModel:      "codex/gpt-5.6-luna",
		CoordinatorModel: "local/qwen3",
		Generation:       agent.GenerationParams{Model: "legacy"},
	}}
	if err := resolveAndCheckModel(session, &config.Config{}); err != nil {
		t.Fatalf("resolveAndCheckModel() error = %v", err)
	}
	if session.Config.WorkerModel != "codex/gpt-5.6-luna" || session.Config.CoordinatorModel != "local/qwen3" {
		t.Fatalf("targets = worker %q coordinator %q", session.Config.WorkerModel, session.Config.CoordinatorModel)
	}
}

func TestResolveAndCheckModelRejectsLegacyAgentTargetForCoordinator(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{Generation: agent.GenerationParams{Model: "codex/gpt-5.6-luna"}}}
	err := resolveAndCheckModel(session, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "coordinator") {
		t.Fatalf("resolveAndCheckModel() error = %v, want coordinator target error", err)
	}
}
