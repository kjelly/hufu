package team

import (
	"context"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/execution"
)

type resultRepairModelKey struct{}

// WithResultRepairModel selects an auxiliary submission model already present
// in the source policy. It never changes the worker's original target/receipt.
func WithResultRepairModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, resultRepairModelKey{}, model)
}

func admittedResultRepairTarget(frozen, current *ExecutionPolicySnapshot, model string) (execution.ExecutionTarget, error) {
	selector, err := execution.ParseExecutionSelector(model)
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	target := execution.ExecutionTarget{Backend: selector.Backend, Model: selector.Model}
	if err := target.Validate(); err != nil {
		return target, fmt.Errorf("repair model requires an exact backend-qualified target: %w", err)
	}
	for _, policy := range []*ExecutionPolicySnapshot{frozen, current} {
		if policy == nil || !slices.ContainsFunc(policy.ModelRoutes, func(route ExecutionModelRouteSnapshot) bool {
			return route.Model == model && route.Backend == target.Backend
		}) || !slices.ContainsFunc(policy.Backends, func(backend ExecutionBackendPolicySnapshot) bool {
			return backend.Backend == target.Backend && backend.Kind == string(execution.BackendKindLLM)
		}) {
			return target, fmt.Errorf("repair model %q is not an admitted LLM route in both the original and current policy", model)
		}
	}
	return target, nil
}

func (c *Coordinator) resultRepairTarget(model string) (execution.ExecutionTarget, error) {
	frozen, err := c.canonicalExecutionPolicySnapshot()
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	var current *ExecutionPolicySnapshot
	if c.executionPolicy != nil {
		current = c.executionPolicy.snapshot
	}
	return admittedResultRepairTarget(frozen, current, model)
}

func grantedResultRepairModel(item *TodoItem) string {
	if item == nil || item.ExecutionReceipt == nil || item.ExecutionReceipt.RepairProvenance == nil {
		return ""
	}
	grants := item.ExecutionReceipt.RepairProvenance.OperatorGrants
	if len(grants) == 0 {
		return ""
	}
	return grants[len(grants)-1].Model
}
