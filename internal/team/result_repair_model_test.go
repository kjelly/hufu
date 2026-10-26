package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/execution"
)

func TestResultRepairModelRequiresOriginalAndCurrentLLMAdmission(t *testing.T) {
	for _, mode := range []string{"accepted", "unknown", "bare", "missing_original", "missing_current", "agent_backend"} {
		t.Run(mode, func(t *testing.T) {
			policy := &ExecutionPolicySnapshot{
				ModelRoutes: []ExecutionModelRouteSnapshot{{Model: "provider/submitter", Backend: "provider"}},
				Backends:    []ExecutionBackendPolicySnapshot{{Backend: "provider", Kind: string(execution.BackendKindLLM)}},
			}
			frozen, current := cloneExecutionPolicySnapshot(policy), cloneExecutionPolicySnapshot(policy)
			model := "provider/submitter"
			switch mode {
			case "unknown":
				model = "provider/other"
			case "bare":
				model = "submitter"
			case "missing_original":
				frozen.ModelRoutes = nil
			case "missing_current":
				current.ModelRoutes = nil
			case "agent_backend":
				frozen.Backends[0].Kind = string(execution.BackendKindAgent)
			}
			target, err := admittedResultRepairTarget(frozen, current, model)
			if (err == nil) != (mode == "accepted") {
				t.Fatalf("repair model %s target=%+v error=%v", mode, target, err)
			}
			if mode == "accepted" && target.String() != model {
				t.Fatal("repair silently retargeted the admitted model")
			}
		})
	}
}
