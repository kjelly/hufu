package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/execution"
)

type fanoutBackendSession struct {
	SlotSHA256 string          `json:"slot_sha256"`
	Binding    ProviderBinding `json:"binding"`
}

func (c *Coordinator) fanoutBackendSessionDocument(item *TodoItem, index int) (string, string, error) {
	if item == nil || index < 0 || index >= min(len(item.ExecutionTopology), maxConcurrentModels) {
		return "", "", errResultCollectionStale
	}
	// Completion belongs to one occurrence. The session belongs to the slot
	// across occurrences and resumes on this branch.
	slot, err := collectionDigest(struct {
		Branch, Task string
		Index        int
		Target       execution.ExecutionTarget
	}{c.activeBranchID(), item.ID, index, item.ExecutionTopology[index]})
	return "model-fanout-backend-session:" + slot, slot, err
}

// Reserve the external session before its first turn. The canonical parent
// session field describes the primary execution target, so a private fanout
// slot cannot publish a task-level session binding under that field.
func (c *Coordinator) persistFanoutBackendSession(ctx context.Context, taskID string, binding ProviderBinding) error {
	slot := c.modelFanout
	target, err := c.modelFanoutTarget(taskID)
	if err != nil {
		return err
	}
	if !execution.BackendNamesEqual(binding.Provider, target.Backend) || binding.SessionID == "" {
		return errResultCollectionStale
	}
	parentItem := slot.parent.todoItemByID(taskID)
	prior, err := slot.parent.readFanoutBackendSession(parentItem, slot.index)
	if err != nil {
		return err
	}
	if err := backendSessionConflict(taskID, prior, target.Backend, binding.SessionID); err != nil {
		return err
	}
	if prior == nil {
		legacy, err := slot.parent.legacyFanoutBackendSession(ctx, parentItem, slot.index)
		if err != nil {
			return err
		}
		if err := backendSessionConflict(taskID, legacy, target.Backend, binding.SessionID); err != nil {
			return err
		}
		name, digest, err := slot.parent.fanoutBackendSessionDocument(parentItem, slot.index)
		if err != nil {
			return err
		}
		body, err := json.Marshal(fanoutBackendSession{SlotSHA256: digest, Binding: binding}, json.Deterministic(true))
		if err != nil {
			return err
		}
		if err := slot.parent.sealCollectionDocument(ctx, name, body); err != nil {
			return withFailureClassOverride(fmt.Errorf("persist external fanout session: %w", err), FailureEnvironment)
		}
	}
	return c.applyProviderSessionBinding(taskID, target, binding)
}

func (c *Coordinator) fanoutBackendSessionForDispatch(ctx context.Context, item *TodoItem, index int) (*durableSessionBinding, error) {
	saved, err := c.readFanoutBackendSession(item, index)
	if err != nil || saved != nil {
		return saved, err
	}
	return c.legacyFanoutBackendSession(ctx, item, index)
}

// Older journals use one task-wide session binding. A unique compatible
// topology slot may adopt it before its next turn. Reject ambiguity instead
// of starting a replacement thread or attaching two slots to one session.
func (c *Coordinator) legacyFanoutBackendSession(ctx context.Context, item *TodoItem, index int) (*durableSessionBinding, error) {
	binding, err := c.durableBackendSessionBinding(ctx, item.ID)
	if err != nil || binding == nil {
		return binding, err
	}
	matched := -1
	for i, target := range item.ExecutionTopology[:min(len(item.ExecutionTopology), maxConcurrentModels)] {
		if !execution.BackendNamesEqual(target.Backend, binding.Binding.Backend) {
			continue
		}
		if matched >= 0 {
			return nil, &ExecutionIdentityConflictError{TaskID: item.ID, EventID: binding.EventID, Reason: "legacy backend session matches multiple fanout slots"}
		}
		matched = i
	}
	if matched < 0 {
		return nil, &ExecutionIdentityConflictError{TaskID: item.ID, EventID: binding.EventID, Reason: "legacy backend session matches no fanout slot"}
	}
	if index != matched {
		return nil, nil
	}
	return binding, nil
}

func (c *Coordinator) readFanoutBackendSession(item *TodoItem, index int) (*durableSessionBinding, error) {
	name, digest, err := c.fanoutBackendSessionDocument(item, index)
	if err != nil {
		return nil, err
	}
	body, found, err := workspaceDocs(c.artifactStoreRootPath()).getDocument(name)
	if err != nil || !found {
		return nil, err
	}
	target := item.ExecutionTopology[index]
	var saved fanoutBackendSession
	if err := json.Unmarshal(body, &saved, json.RejectUnknownMembers(true)); err != nil ||
		saved.SlotSHA256 != digest || !execution.BackendNamesEqual(saved.Binding.Provider, target.Backend) || saved.Binding.SessionID == "" {
		return nil, errResultCollectionStale
	}
	return &durableSessionBinding{EventID: name, Binding: *backendBindingFromProviderBinding(&saved.Binding, target)}, nil
}
