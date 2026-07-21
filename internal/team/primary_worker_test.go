package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

// TestPrimaryWorkerName verifies the fast-path agent selection rule: a team
// with exactly one worker returns that worker's name; zero or multiple
// workers return "" (fall through to the team path so the coordinator picks
// the right specialist rather than guessing).
func TestPrimaryWorkerName(t *testing.T) {
	cases := []struct {
		name   string
		agents map[string]*agent.AgentDef
		want   string
	}{
		{
			name: "single worker (default team shape)",
			agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator"},
				"helper":      {Name: "Helper", Role: "worker"},
			},
			want: "Helper",
		},
		{
			name: "multiple workers",
			agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator"},
				"researcher":  {Name: "researcher", Role: "worker"},
				"coder":       {Name: "coder", Role: "worker"},
			},
			want: "",
		},
		{
			name: "no workers",
			agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator"},
			},
			want: "",
		},
		{
			name:   "no agents at all",
			agents: map[string]*agent.AgentDef{},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coordinator{session: &TeamSession{Agents: tc.agents}}
			if got := c.PrimaryWorkerName(); got != tc.want {
				t.Errorf("PrimaryWorkerName() = %q, want %q", got, tc.want)
			}
		})
	}
}
