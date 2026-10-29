package team

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// historicalResultGrantProjection folds only the visible consumer lineage.
// The checkpoint retains a reference, never a second copy of grant bodies.
// Initial workflows have no WorkflowCycleID, so their first run_started (or
// grant in an imported history) supplies the cycle identity across resumes.
func historicalResultGrantProjection(events []RunEvent) (*runtimestore.HistoricalGrantCycleRef, *HistoricalResultGrantsPayload, error) {
	if !slices.ContainsFunc(events, func(event RunEvent) bool { return event.Type == string(EventHistoricalResultsGranted) }) {
		return nil, nil, nil
	}
	var ref *runtimestore.HistoricalGrantCycleRef
	var admitted *HistoricalResultGrantsPayload
	var cycleID string
	tasksCreated := false
	for _, event := range events {
		switch EventType(event.Type) {
		case EventRunStarted:
			if cycleID == "" {
				cycleID = event.RunID
			}
		case EventWorkflowCycleStarted:
			if _, err := decodeWorkflowCycleStarted(event); err != nil {
				return nil, nil, err
			}
			cycleID, tasksCreated = event.RunID, false
			ref, admitted = nil, nil
		case EventTaskCreated:
			tasksCreated = true
		case EventHistoricalResultsGranted:
			payload, err := decodeHistoricalResultGrants(event)
			if err != nil {
				return nil, nil, err
			}
			if !historicalGrantIdentity(event.ID) || !historicalGrantSHA256(event.Hash) {
				return nil, nil, fmt.Errorf("historical result grant has no durable event identity")
			}
			if ref != nil || tasksCreated || (cycleID != "" && cycleID != payload.CycleID) {
				return nil, nil, fmt.Errorf("historical result grants must be admitted once before the cycle's tasks")
			}
			cycleID = payload.CycleID
			ref = &runtimestore.HistoricalGrantCycleRef{BranchID: event.BranchID, CycleID: event.RunID, EventID: event.ID, EventHash: event.Hash}
			admitted = &payload
		}
	}
	return ref, admitted, nil
}

func cloneHistoricalGrantCycleRef(ref *runtimestore.HistoricalGrantCycleRef) *runtimestore.HistoricalGrantCycleRef {
	if ref == nil {
		return nil
	}
	return new(*ref)
}

// validateHistoricalResultGrantProjection authenticates the active reference
// against both authorities: verified branch-visible events and their original
// transactional index. A fork may retain an ancestor's grant only when that
// exact event is visible before its fork cutoff. No child index is fabricated.
// Callers obtain events from ReadEvents and the checked branch projector.
func (es *EventStore) validateHistoricalResultGrantProjection(ctx context.Context, events []RunEvent) (*HistoricalResultGrantsPayload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ref, payload, err := historicalResultGrantProjection(events)
	if err != nil || ref == nil {
		return nil, err
	}
	policy, err := executionPolicySnapshotFromEvents(events)
	if err != nil {
		return nil, err
	}
	if policy == nil || policy.ConfigurationHash != payload.PolicyFingerprint {
		return nil, fmt.Errorf("historical result grant policy differs from the canonical execution policy")
	}
	for index, event := range events {
		if event.ID != ref.EventID {
			continue
		}
		priorPolicy, err := executionPolicySnapshotFromEvents(events[:index])
		if err != nil || priorPolicy == nil || priorPolicy.ConfigurationHash != payload.PolicyFingerprint {
			return nil, fmt.Errorf("historical result grant precedes its canonical execution policy")
		}
		break
	}
	if es == nil {
		return nil, fmt.Errorf("historical result grant event store is unavailable")
	}
	if err := es.acquire(ctx); err != nil {
		return nil, err
	}
	defer es.release()
	if es.store == nil || filepath.Dir(es.path) != payload.Workspace {
		return nil, fmt.Errorf("historical result grant control workspace differs from its event store")
	}
	indexed, found, err := runtimestore.HistoricalGrantCycle(ctx, es.store.DB(), ref.BranchID, ref.CycleID)
	if err != nil {
		return nil, err
	}
	if !found || indexed != *ref {
		return nil, runtimestore.ErrHistoricalGrantSource
	}
	return payload, nil
}
