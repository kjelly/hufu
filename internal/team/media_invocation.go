package team

import (
	"context"
	"slices"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

// This preflight reads canonical metadata before opening an attempt. Byte
// access additionally requires the live lease checked by validateMediaAttempt.
func (c *Coordinator) validateNativeMediaTaskExecution(ctx context.Context, task TaskDef, taskID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	item := c.todoItemByID(taskID)
	if item == nil || item.MediaBinding == nil || c.executionPolicy == nil {
		return media.Reject(media.DurabilityRequired, "native image task has no canonical frozen occurrence")
	}
	if ctx.Value(leafExecutionKey{}) != nil || c.workerAgentOverride != nil || c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return media.Reject(media.Unsupported, "native image execution requires one canonical worker")
	}
	if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
		return err
	}
	if _, err := c.nativeImageWorkerDefinition(item.Agent); err != nil {
		return err
	}
	if task.MediaBinding == nil || task.MediaBinding.Hash != item.MediaBinding.Hash || task.MediaInputDigest != item.MediaInputDigest {
		return media.Reject(media.BindingConflict, "native image execution differs from its canonical binding")
	}
	if err := validateTaskMediaShape(item); err != nil {
		return err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return err
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	if item.MediaBinding.PolicyHash != policy.Hash || view.policyHash != policy.Hash {
		return media.Reject(media.AccessDenied, "native image task is outside the current frozen policy")
	}
	if err := mediaBindingSources(item.MediaBinding, view, policy.Policy.Image); err != nil {
		return err
	}
	if err := validateNativeMediaSourceScope(item.MediaBinding, view.events); err != nil {
		return err
	}
	// A resumed invocation gets a new run ID. Its input and owner retain their
	// original run/branch, proven by the visible canonical creation pair.
	return validateMediaOccurrenceBinding(item, view.events)
}

func validateNativeMediaInvocation(item *TodoItem, policy *ExecutionPolicySnapshot) error {
	if item == nil || item.MediaBinding == nil {
		return media.Reject(media.AccessDenied, "native image invocation has no canonical owner")
	}
	switch item.MediaBinding.InvocationMode {
	case media.InvocationSingleTask, media.InvocationDirectAgent, media.InvocationDelegatedTask:
	default:
		return media.Reject(media.Unsupported, "image input has no supported invocation mode")
	}
	if item.Phase != "" || item.Sidecar || item.Action != nil || item.CatalogAction != nil ||
		item.InvariantVerification != "" || len(item.Execution.Steps) != 0 || item.Escalate || len(item.ModelTopology) > 1 || len(item.ExecutionTopology) > 1 {
		return media.Reject(media.Unsupported, "image invocation has unsupported action, workflow or model routing")
	}
	if item.MediaBinding.InvocationMode != media.InvocationDelegatedTask &&
		(item.ParentID != "" || len(item.DependsOn) != 0 || len(item.OrderAfter) != 0 || item.OnFailure != "") {
		return media.Reject(media.Unsupported, "direct image invocation requires one root worker without a task graph")
	}
	if policy == nil {
		return media.Reject(media.DurabilityRequired, "native image invocation has no frozen policy")
	}
	if err := validateNativeMediaRoute(item, policy); err != nil {
		return err
	}
	for _, backend := range policy.Backends {
		if backend.Backend == item.ExecutionTarget.Backend && backend.Kind == string(execution.BackendKindLLM) {
			return nil
		}
	}
	return media.Reject(media.Unsupported, "image invocation requires a native LLM backend")
}

// validateNativeMediaSourceScope keeps the M1 input turn attached to one root
// occurrence, in its full original order. Generic legacy bindings remain
// readable; they do not gain permission to use the M1 transport.
func validateNativeMediaSourceScope(binding *media.TaskBinding, lineage []RunEvent) error {
	if binding == nil || len(binding.Selections) == 0 {
		return media.Reject(media.AccessDenied, "native image invocation has no input source")
	}
	if binding.InvocationMode == media.InvocationDelegatedTask {
		sources := make(map[string]media.InputSnapshot)
		for _, event := range lineage {
			if event.Type != string(EventMediaInputAdmitted) {
				continue
			}
			snapshot, err := decodeMediaInputEvent(event)
			if err != nil {
				return err
			}
			sources[snapshot.ID] = snapshot
		}
		return mediaBindingSources(binding, mediaLineageView{snapshots: sources}, media.Limits{})
	}
	if err := validateMediaInvocationSourceOwner(binding, lineage); err != nil {
		return err
	}
	var snapshot *media.InputSnapshot
	for _, event := range lineage {
		if event.Type != string(EventMediaInputAdmitted) {
			continue
		}
		input, err := decodeMediaInputEvent(event)
		if err != nil {
			return err
		}
		if input.ID == binding.Selections[0].SnapshotID {
			snapshot = &input
		}
	}
	if snapshot == nil {
		return media.Reject(media.AccessDenied, "native image source is unavailable")
	}
	var expected []media.Selection
	for index, part := range snapshot.Input.Parts {
		if part.Type == "image" {
			expected = append(expected, media.Selection{SnapshotID: snapshot.ID, SnapshotHash: snapshot.Hash, TurnID: snapshot.TurnID, PartIndex: index})
		}
	}
	if !slices.Equal(binding.Selections, expected) {
		return media.Reject(media.AccessDenied, "native image invocation changed its complete ordered input")
	}
	return nil
}

func validateMediaInvocationSourceOwner(binding *media.TaskBinding, lineage []RunEvent) error {
	ownerIndex := -1
	for index, event := range lineage {
		if event.Type != string(EventTaskMediaBound) || event.TaskID != binding.TaskID {
			continue
		}
		bound, err := decodeTaskMediaEvent(event)
		if err != nil {
			return err
		}
		if bound.Binding.Hash == binding.Hash {
			ownerIndex = index
		}
	}
	for index, event := range lineage {
		if event.Type != string(EventTaskMediaBound) || event.TaskID == binding.TaskID {
			continue
		}
		bound, err := decodeTaskMediaEvent(event)
		if err != nil {
			return err
		}
		if binding.InvocationMode == "" && bound.Binding.InvocationMode == "" {
			continue
		}
		// An explicit delegated grant does not revoke the source owner's
		// existing input. It has its own canonical task binding and lease.
		if bound.Binding.InvocationMode == media.InvocationDelegatedTask && ownerIndex >= 0 && index > ownerIndex {
			continue
		}
		for _, source := range binding.Selections {
			for _, previous := range bound.Binding.Selections {
				if source.SnapshotID == previous.SnapshotID {
					return media.Reject(media.AccessDenied, "native image input already belongs to another occurrence")
				}
			}
		}
	}
	return nil
}

func validateNativeMediaBatch(tasks []TaskDef) error {
	for _, task := range tasks {
		if task.MediaBinding == nil || task.MediaBinding.InvocationMode == "" || task.MediaBinding.InvocationMode == media.InvocationDelegatedTask {
			continue
		}
		if len(tasks) != 1 || task.Pipeline || task.FanOut != nil || len(task.DependsOnContracts) != 0 || task.OnFailureContract != "" {
			return media.Reject(media.Unsupported, "native image invocation cannot expand into a task graph")
		}
	}
	return nil
}

func verifyNativeMediaInvocationCreation(event RunEvent, prior []RunEvent) error {
	items, err := ReplayTodoList(slices.Concat(prior, []RunEvent{event}))
	if err != nil {
		return err
	}
	item := todoItemByID(items, event.TaskID)
	policy, err := mediaProviderPolicyFromLineage(prior)
	if err != nil {
		return err
	}
	if err := validateNativeMediaInvocation(item, &policy); err != nil {
		return err
	}
	return validateNativeMediaSourceScope(item.MediaBinding, prior)
}
