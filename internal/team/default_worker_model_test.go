package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestDefaultWorkerModelPrefersTeamRoute(t *testing.T) {
	route := &ExecutionRouteDefinition{Name: "review", Candidates: []execution.ExecutionTarget{
		{Backend: "ollama", Model: "primary"}, {Backend: "ollama", Model: "fallback"},
	}}
	tests := []struct {
		name   string
		config agent.TeamConfig
		routes map[string]*ExecutionRouteDefinition
		want   string
	}{
		{
			name:   "team route first candidate",
			config: agent.TeamConfig{ExecutionRoute: "review", WorkerModel: "ollama/explicit"},
			routes: map[string]*ExecutionRouteDefinition{"review": route},
			want:   "ollama/primary",
		},
		{
			name:   "worker model without route",
			config: agent.TeamConfig{WorkerModel: "ollama/explicit", Generation: agent.GenerationParams{Model: "ollama/legacy"}},
			want:   "ollama/explicit",
		},
		{
			name:   "unbound route falls through",
			config: agent.TeamConfig{ExecutionRoute: "review", WorkerModel: "ollama/explicit"},
			want:   "ollama/explicit",
		},
		{
			name:   "legacy team model",
			config: agent.TeamConfig{Generation: agent.GenerationParams{Model: "ollama/legacy"}},
			want:   "ollama/legacy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{session: &TeamSession{Config: tt.config, ExecutionRoutes: tt.routes}}
			if got := c.defaultWorkerModel(); got != tt.want {
				t.Errorf("defaultWorkerModel() = %q, want %q", got, tt.want)
			}
			// A worker the team never bound resolves to the same default.
			if got := c.resolveAgentModel(&agent.AgentDef{Name: "unbound", Role: "worker"}, ""); got != tt.want {
				t.Errorf("resolveAgentModel(unbound) = %q, want %q", got, tt.want)
			}
		})
	}
}
