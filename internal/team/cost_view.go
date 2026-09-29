package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
)

// ProjectCostView reduces canonical cost events and the frozen policy from one
// already-validated lineage. It performs no I/O and never repairs state.
func ProjectCostView(events []RunEvent, runID, taskID string) (cost.View, error) {
	decoded, err := costEventsFromRunEvents(events)
	if err != nil {
		return cost.View{}, err
	}
	projection, err := cost.Reduce(decoded)
	if err != nil {
		return cost.View{}, fmt.Errorf("reduce cost projection: %w", err)
	}
	policy, err := executionPolicySnapshotFromEvents(events)
	if err != nil {
		return cost.View{}, fmt.Errorf("project cost policy: %w", err)
	}
	var costPolicy *cost.PolicySnapshot
	if policy != nil {
		costPolicy = policy.Cost
	}
	view, err := cost.BuildView(projection, costPolicy, runID, taskID)
	if err != nil {
		return cost.View{}, err
	}
	for _, event := range events {
		if event.RunID != runID || !isCostEventType(EventType(event.Type)) {
			continue
		}
		view.Freshness = cost.Freshness{EventID: event.ID, EventHash: event.Hash}
	}
	return view, nil
}

func (m *CostManager) View(runID, taskID string) (cost.View, error) {
	if m == nil {
		return cost.View{}, fmt.Errorf("cost manager is disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.integrity != nil {
		return cost.View{}, fmt.Errorf("cost ledger integrity is degraded: %w", m.integrity)
	}
	return cost.BuildView(m.projection.Clone(), m.policy, runID, taskID)
}

// CostView returns the root coordinator's current generation-cost projection.
// A nil view means cost governance is not configured for this coordinator.
func (c *Coordinator) CostView(runID, taskID string) (*cost.View, error) {
	if c == nil || c.costManager == nil {
		return nil, nil
	}
	if strings.TrimSpace(runID) == "" {
		runID = strings.TrimSpace(c.executionRunID)
	}
	view, err := c.costManager.View(runID, taskID)
	if err != nil {
		return nil, err
	}
	return &view, nil
}

func isCostEventType(eventType EventType) bool {
	switch eventType {
	case EventCostPriceSnapshotResolved, EventCostReservationCommitted, EventCostSettled, EventCostBudgetWarning, EventCostBudgetDenied:
		return true
	default:
		return false
	}
}
