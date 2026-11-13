package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

type readOnlyActionRepairKey struct{}

// WithReadOnlyActionRepair explicitly selects a failed native action and its
// changed Go program. A shared resolver program may also change, but its frozen
// values and schema remain pinned by the separate input-policy proof.
func WithReadOnlyActionRepair(ctx context.Context, taskID string) context.Context {
	return context.WithValue(WithFrozenInputPolicyRecovery(ctx), readOnlyActionRepairKey{}, taskID)
}

func readOnlyActionRepairID(ctx context.Context) string {
	id, _ := ctx.Value(readOnlyActionRepairKey{}).(string)
	return id
}

// ReadOnlyActionRepair binds the approved source replacement to the original
// failed action event. It grants no execution during preview or publication.
type ReadOnlyActionRepair struct {
	TaskID             string            `json:"task_id"`
	Capability         string            `json:"capability"`
	ActionID           string            `json:"action_id"`
	FailureEventID     string            `json:"failure_event_id"`
	FailureEventHash   string            `json:"failure_event_hash"`
	OriginalProvider   string            `json:"original_provider"`
	AcceptedProvider   string            `json:"accepted_provider"`
	SharedCapabilities []string          `json:"shared_capabilities"`
	ProviderBindings   map[string]string `json:"provider_bindings"`
}

func goProgramIdentity(name string) bool {
	return strings.HasPrefix(name, "golang:") && runInputHashPattern.MatchString(strings.TrimPrefix(name, "golang:"))
}

func actionRepairFailure(events []RunEvent, item *TodoItem) (*RunEvent, *LifecycleEventPayload, error) {
	if item == nil || item.Action == nil || item.TypedResult != nil || item.ExecutionReceipt != nil ||
		(item.Status != TaskError && item.Status != TaskBlocked) || item.FailureEvent == nil ||
		item.FailureEvent.FailureClass != FailureExecution || item.FailureEvent.ReceiptID == "" ||
		!metadataReadOnlyTask(item.SideEffect, item.Execution) || item.WorkerWorkspace != nil || item.Recovery != RecoveryRetry {
		return nil, nil, fmt.Errorf("action repair requires a settled failed read-only native action")
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := &events[i]
		if event.Type != "action_failed" || event.TaskID != item.ID {
			continue
		}
		var failure LifecycleEventPayload
		if err := json.Unmarshal(event.Payload, &failure); err != nil {
			return nil, nil, err
		}
		if failure.ActionID != item.FailureEvent.ReceiptID {
			continue
		}
		if failure.ActionStatus != "failure" || failure.Capability != item.Action.Capability || failure.ToolName != item.Action.Type ||
			failure.RunInputSnapshotID != item.RunInputSnapshotID || failure.RunInputSnapshotHash != item.RunInputSnapshotHash ||
			failure.MaterializedActionPayloadHash != item.MaterializedActionPayloadHash || !sameCanonicalEvidence(failure.BoundInputs, item.BoundInputs) || !goProgramIdentity(failure.Provider) {
			return nil, nil, fmt.Errorf("action repair failed receipt does not match its frozen action")
		}
		return event, &failure, nil
	}
	return nil, nil, fmt.Errorf("action repair lacks a canonical failed Go action event")
}

func (c *Coordinator) assessReadOnlyActionRepair(ctx context.Context, source *ResumeSource, taskID string) (*ReadOnlyActionRepair, error) {
	if taskID == "" || strings.TrimSpace(taskID) != taskID || contractResultRepairID(ctx) != "" {
		return nil, fmt.Errorf("action repair requires one exact task ID and cannot repair results")
	}
	if err := c.validateReadOnlyRecoveryContracts(source.Session, "action repair"); err != nil {
		return nil, err
	}
	item := todoItemByID(source.Session.Tasks, taskID)
	event, failure, err := actionRepairFailure(source.lineage, item)
	if err != nil {
		return nil, err
	}
	accepted := c.session.ProviderRegistry.ProviderName(item.Action.Capability)
	if !goProgramIdentity(accepted) || accepted == failure.Provider {
		return nil, fmt.Errorf("action repair requires a changed trusted Go provider source")
	}
	proof := &ReadOnlyActionRepair{TaskID: taskID, Capability: item.Action.Capability, ActionID: failure.ActionID, FailureEventID: event.ID, FailureEventHash: event.Hash, OriginalProvider: failure.Provider, AcceptedProvider: accepted}
	proof.ProviderBindings = map[string]string{}
	for capability := range c.session.Config.ActionProviders {
		name := c.session.ProviderRegistry.ProviderName(capability)
		if !goProgramIdentity(name) {
			return nil, fmt.Errorf("action repair requires known Go provider families: %q", capability)
		}
		proof.ProviderBindings[capability] = name
		if name == accepted {
			proof.SharedCapabilities = append(proof.SharedCapabilities, capability)
		}
	}
	slices.Sort(proof.SharedCapabilities)
	if err := validateActionRepairProviders(source.lineage, proof); err != nil {
		return nil, err
	}
	if !slices.Contains(proof.SharedCapabilities, proof.Capability) {
		return nil, fmt.Errorf("action repair provider is not statically configured")
	}
	return proof, nil
}

func validateActionRepairProof(events []RunEvent, tasks []*TodoItem, plan *ContractRecoveryPlan) error {
	proof := plan.ActionRepair
	if proof == nil {
		return nil
	}
	if plan.WorkRetry != nil || plan.Repaired != 0 || len(plan.ContractChanges) != 0 || !goProgramIdentity(proof.AcceptedProvider) || proof.AcceptedProvider == proof.OriginalProvider ||
		!slices.IsSorted(proof.SharedCapabilities) || !slices.Contains(proof.SharedCapabilities, proof.Capability) {
		return fmt.Errorf("invalid read-only action repair proof")
	}
	item := todoItemByID(tasks, proof.TaskID)
	event, failure, err := actionRepairFailure(events, item)
	if err != nil {
		return err
	}
	if event.ID != proof.FailureEventID || event.Hash != proof.FailureEventHash || failure.Provider != proof.OriginalProvider || failure.ActionID != proof.ActionID || failure.Capability != proof.Capability {
		return fmt.Errorf("action repair failed action identity changed")
	}
	if err := validateActionRepairProviders(events, proof); err != nil {
		return err
	}
	affected := map[string]bool{proof.TaskID: true}
	for changed := true; changed; {
		changed = false
		for _, task := range tasks {
			if task == nil || affected[task.ID] {
				continue
			}
			for _, source := range append(slices.Clone(task.DependsOn), task.EvidenceFrom...) {
				if affected[source] {
					affected[task.ID] = true
					changed = true
					break
				}
			}
		}
	}
	selected := false
	for _, patch := range plan.Tasks {
		if patch.ID == proof.TaskID {
			selected = patch.Action == "retry"
		}
		if patch.Action == "retry" && !affected[patch.ID] {
			return fmt.Errorf("action repair cannot retry unrelated task %s", patch.ID)
		}
	}
	if !selected {
		return fmt.Errorf("action repair must retry the selected failed action")
	}
	return nil
}

// Rebuild provider bindings in lineage order so subsequent repairs honor earlier
// approved replacements instead of comparing current code to obsolete events.
func observedActionProviders(events []RunEvent) (map[string]string, error) {
	providers := map[string]string{}
	for _, event := range events {
		switch event.Type {
		case "action_completed", "action_failed":
			var payload LifecycleEventPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, err
			}
			if goProgramIdentity(payload.Provider) {
				providers[payload.Capability] = payload.Provider
			}
		case string(EventContractRecovery):
			plan, err := decodeContractRecovery(event)
			if err != nil {
				return nil, err
			}
			if plan.ActionRepair != nil {
				for capability, provider := range plan.ActionRepair.ProviderBindings {
					providers[capability] = provider
				}
			}
		}
	}
	return providers, nil
}

func validateActionRepairProviders(events []RunEvent, proof *ReadOnlyActionRepair) error {
	observed, err := observedActionProviders(events)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for capability, previous := range observed {
		known[previous] = true
		accepted := proof.ProviderBindings[capability]
		if accepted != previous && (previous != proof.OriginalProvider || accepted != proof.AcceptedProvider) {
			return fmt.Errorf("action repair changed unrelated provider %q", capability)
		}
	}
	shared := []string{}
	if len(proof.ProviderBindings) == 0 || len(proof.ProviderBindings) > 128 {
		return fmt.Errorf("invalid action repair provider bindings")
	}
	for capability, provider := range proof.ProviderBindings {
		if capability == "" || normalizeCapability(capability) != capability || !goProgramIdentity(provider) || (!known[provider] && provider != proof.AcceptedProvider) {
			return fmt.Errorf("action repair cannot verify provider family %q", capability)
		}
		if provider == proof.AcceptedProvider {
			shared = append(shared, capability)
		}
	}
	slices.Sort(shared)
	if !slices.Equal(shared, proof.SharedCapabilities) || proof.ProviderBindings[proof.Capability] != proof.AcceptedProvider {
		return fmt.Errorf("action repair shared provider bindings differ")
	}
	return nil
}

func (c *Coordinator) validateRestoredActionProviders(bindings map[string]string) error {
	if len(bindings) != len(c.session.Config.ActionProviders) {
		return fmt.Errorf("recovered action provider set changed")
	}
	for capability := range c.session.Config.ActionProviders {
		if bindings[capability] == "" || c.session.ProviderRegistry.ProviderName(capability) != bindings[capability] {
			return fmt.Errorf("recovered action provider %q changed", capability)
		}
	}
	return nil
}
