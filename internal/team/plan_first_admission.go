package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/execution"
)

// planFirstExternalBackendCode rejects a planning occurrence that could run on
// an external agent backend.
const planFirstExternalBackendCode = "plan_first_external_backend"

// validatePlanFirstExecutionTarget rejects a task that must plan first when any
// target its planning attempt could run on is an external agent backend. Such
// a backend runs its own tools, cannot call submit_plan, and does not honor
// the plan-submission stop, so the planning attempt would do the work, and
// finish the task, before any plan was reviewed. An approved plan's execution
// (PlanID set) is not a planning attempt and is unaffected.
func (c *Coordinator) validatePlanFirstExecutionTarget(task TaskDef) error {
	if !task.PlanFirst || task.PlanID != "" {
		return nil
	}
	targets := append([]execution.ExecutionTarget{task.ResolvedExecutionTarget}, task.ExecutionTopology...)
	if task.ExecutionRoute != nil {
		targets = append(targets, task.ExecutionRoute.Candidates...)
	}
	for _, target := range targets {
		if target.IsZero() {
			continue
		}
		backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
		if err == nil && backend.Kind() == execution.BackendKindAgent {
			return fmt.Errorf("%s: agent %q would plan on the external agent backend %q, which cannot submit a plan for review; dispatch it without plan-first or on a Hufu model", planFirstExternalBackendCode, task.Agent, target.Backend)
		}
	}
	return nil
}
