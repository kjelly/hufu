package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestPreflightExecutionTargetsChecksRouteCandidatesForBoundWorkers(t *testing.T) {
	tests := []struct {
		name         string
		reviewer     agent.AgentDef
		routes       map[string]config.ExecutionRouteConfig
		wantErr      string
		wantBinaries []string
	}{
		{
			name:     "language-model candidates pass without a worker target",
			reviewer: agent.AgentDef{Name: "reviewer", Role: "worker"},
			routes:   map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/a", "ollama/b"}}},
		},
		{
			name:     "agent-backend fallback candidate is rejected",
			reviewer: agent.AgentDef{Name: "reviewer", Role: "worker"},
			routes:   map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/a", "codex/gpt-6-sol"}}},
			wantErr:  "agent reviewer route review candidate 2",
		},
		{
			name:     "undefined team route is rejected before setup",
			reviewer: agent.AgentDef{Name: "reviewer", Role: "worker"},
			wantErr:  `execution route "review" is not defined`,
		},
		{
			name:     "agent route wins over the team route",
			reviewer: agent.AgentDef{Name: "reviewer", Role: "worker", ExecutionRoute: "own"},
			routes:   map[string]config.ExecutionRouteConfig{"own": {Candidates: []string{"ollama/own"}}},
		},
		{
			name:         "agent with its own model ignores the team route",
			reviewer:     agent.AgentDef{Name: "reviewer", Role: "worker", Generation: agent.GenerationParams{Model: "codex/gpt-6-sol"}},
			wantBinaries: []string{"codex"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reviewer := tt.reviewer
			session := &team.TeamSession{
				// No worker target: the team route is the worker default.
				Config: agent.TeamConfig{ExecutionRoute: "review"},
				Agents: map[string]*agent.AgentDef{"reviewer": &reviewer},
			}
			var binaries []string
			err := preflightExecutionTargets(session, &config.Config{ExecutionRoutes: tt.routes}, team.RoleModels{}, func(binary string) (string, error) {
				binaries = append(binaries, binary)
				if binary == "codex" {
					return "/test/codex", nil
				}
				return "", errors.New("not found")
			})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("preflightExecutionTargets() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("preflightExecutionTargets() error = %v, want %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(binaries, tt.wantBinaries) {
				t.Fatalf("LookPath binaries = %v, want %v", binaries, tt.wantBinaries)
			}
		})
	}
}
