package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func staticGateSession(t *testing.T) *team.TeamSession {
	t.Helper()
	return &team.TeamSession{
		Dir: t.TempDir(), Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:             "static-gate",
			WorkerModel:      "ollama/fixture-model",
			CoordinatorModel: "ollama/fixture-model",
		},
		Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}},
	}
}

func breakRoleTargets(session *team.TeamSession) { session.Config.WorkerModel = "" }

func breakExecutionPreflight(session *team.TeamSession) {
	session.Config.WorkerModel = "nope/fixture-model"
}

func breakEffectiveContract(session *team.TeamSession) {
	session.Config.Requirements.Environment = []string{"HUFU_TEST_STATIC_GATE_UNSET"}
}

func TestRunStartupStaticGateReportsIndependentFailuresTogether(t *testing.T) {
	tests := []struct {
		name     string
		breakers []func(*team.TeamSession)
		// wantLines are the reported checks, in order; empty means one
		// failure reported through its original error.
		wantLines  []string
		wantSingle string
		wantCauses int
	}{
		{name: "all checks pass"},
		{
			name:       "one failure keeps its original error",
			breakers:   []func(*team.TeamSession){breakEffectiveContract},
			wantSingle: "effective team contract validation failed",
		},
		{
			name:     "role failure skips the dependent target check",
			breakers: []func(*team.TeamSession){breakRoleTargets, breakEffectiveContract},
			wantLines: []string{
				"static.effective_contract [failed, effective_contract_invalid]",
				"static.execution_preflight [skipped, dependency_unavailable]",
				"static.role_targets [failed, role_target_invalid]: no worker execution target specified",
			},
			wantCauses: 2,
		},
		{
			name:     "target preflight and contract failures",
			breakers: []func(*team.TeamSession){breakExecutionPreflight, breakEffectiveContract},
			wantLines: []string{
				"static.effective_contract [failed, effective_contract_invalid]",
				`static.execution_preflight [failed, execution_target_invalid]: unknown execution backend "nope"`,
			},
			wantCauses: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			opts = runOptions{}
			session := staticGateSession(t)
			for _, breakSession := range tt.breakers {
				breakSession(session)
			}

			gate, err := runStartupStaticGate(session, nil, &config.Config{}, team.ExecutionProfile{}, false)
			var aggregate *startupCheckError
			switch {
			case len(tt.breakers) == 0:
				if err != nil {
					t.Fatalf("runStartupStaticGate error = %v", err)
				}
				if gate.RoleModels == (team.RoleModels{}) || len(gate.AllowedPaths) == 0 {
					t.Fatalf("gate = %#v, want resolved roles and allowed paths", gate)
				}
			case tt.wantSingle != "":
				if err == nil || errors.As(err, &aggregate) || !strings.HasPrefix(err.Error(), tt.wantSingle) {
					t.Fatalf("error = %v, want the original %q error", err, tt.wantSingle)
				}
			default:
				if !errors.As(err, &aggregate) {
					t.Fatalf("error = %v, want a startupCheckError", err)
				}
				if got := len(aggregate.Unwrap()); got != tt.wantCauses {
					t.Fatalf("unwrapped causes = %d, want %d", got, tt.wantCauses)
				}
				lines := strings.Split(err.Error(), "\n")[1:]
				if len(lines) != len(tt.wantLines) {
					t.Fatalf("reported checks = %q, want %d lines", lines, len(tt.wantLines))
				}
				for index, want := range tt.wantLines {
					if !strings.Contains(lines[index], want) {
						t.Fatalf("check line %d = %q, want it to contain %q", index, lines[index], want)
					}
				}
			}
		})
	}
}
