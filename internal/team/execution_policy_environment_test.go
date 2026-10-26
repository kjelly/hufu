package team

import (
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
)

func TestUnusedAgentEnvironmentCompatibilityRevokesAgentExecution(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 2)
	delete(c.session.Agents, "codex-worker")
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	frozen := cloneExecutionPolicySnapshot(state.snapshot)
	for _, mode := range []string{"unused_env", "active_env", "executable", "network", "contract"} {
		t.Run(mode, func(t *testing.T) {
			current := cloneExecutionPolicySnapshot(frozen)
			index := slices.IndexFunc(current.ExecutionWorlds, func(world ExecutionWorldPolicySnapshot) bool { return world.Backend == "codex" })
			current.ExecutionWorlds[index].InheritEnv[0].ValueHash = "changed-environment"
			switch mode {
			case "active_env":
				current.ModelRoutes = append(current.ModelRoutes, ExecutionModelRouteSnapshot{Model: "codex/worker", Backend: "codex"})
			case "executable":
				current.Backends[0].IdentityHash = "changed-command"
			case "network":
				current.ExecutionWorlds[index].CoordinatorNetworkDisabled = !current.ExecutionWorlds[index].CoordinatorNetworkDisabled
			case "contract":
				current.StaticWorkflowHash = "changed-workflow"
			}
			matched, err := compatibleUnusedExecutionEnvironment(frozen, current)
			if err != nil || matched != (mode == "unused_env") {
				t.Fatalf("policy compatibility mode=%s matched=%v error=%v", mode, matched, err)
			}
		})
	}
	if _, allowed := state.backendPolicy(execution.ExecutionTarget{Backend: "codex", Model: "worker"}); allowed {
		t.Fatal("unused backend could be dynamically dispatched after ignoring its environment")
	}
	if _, _, err := state.codexExecutionWorld("codex", nil); err == nil {
		t.Fatal("unused backend could open an execution world")
	}
	if _, allowed := state.providerPolicyForModel("remote/worker-model"); !allowed {
		t.Fatal("LLM continuation lost its frozen provider")
	}
}
