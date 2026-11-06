package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"reflect"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

func taskHasMedia(task TaskDef) bool { return task.MediaBinding != nil || task.MediaInputDigest != "" }

// A text-starting worker can acquire image evidence from an authorized MCP
// call. Such work owns its receipts and cannot share a cached prose result.
func (c *Coordinator) taskMayProduceToolMedia(ctx context.Context, task TaskDef) bool {
	if c == nil || c.executionPolicy == nil || c.executionPolicy.snapshot == nil || c.executionPolicy.snapshot.Media == nil || task.Action != nil {
		return false
	}
	if c.session == nil || len(c.managerMCPDescriptors()) == 0 {
		return false
	}
	def := c.session.Agents[strings.ToLower(task.Agent)]
	if def == nil {
		return true // Unresolved authority is never proof that reuse is safe.
	}
	authorization, err := c.resolveNewTaskToolAuthorization(ctx, task, def)
	return err != nil || authorization != nil && len(authorization.Targets) != 0
}

func (c *Coordinator) taskNeedsOwnMediaExecution(ctx context.Context, task TaskDef) bool {
	return taskHasMedia(task) || c.taskMayProduceToolMedia(ctx, task)
}

func validateTaskMediaShape(item *TodoItem) error {
	if item.MediaBinding == nil {
		if item.MediaInputDigest != "" {
			return media.Reject(media.BindingConflict, "media digest has no binding")
		}
		return nil
	}
	b := item.MediaBinding
	if err := b.Validate(media.Limits{}); err != nil {
		return err
	}
	if b.TaskID != item.ID || b.Agent != strings.ToLower(strings.TrimSpace(item.Agent)) || b.InputDigest != item.MediaInputDigest {
		return media.Reject(media.BindingConflict, "media binding differs from its task occurrence")
	}
	return nil
}

func (c *Coordinator) validateTaskMediaCreation(ctx context.Context, item *TodoItem) error {
	if err := validateTaskMediaShape(item); err != nil {
		return err
	}
	if item.MediaBinding == nil {
		return nil
	}
	if !c.hasDurableEventJournal() {
		return media.Reject(media.DurabilityRequired, "media task requires durable occurrence binding")
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return err
	}
	b := item.MediaBinding
	if b.RunID != c.executionRunID || b.BranchID != c.activeBranchID() || b.PolicyHash != policy.Hash {
		return media.Reject(media.AccessDenied, "media task identity differs from this admission")
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	if view.policyHash != policy.Hash {
		return media.Reject(media.DurabilityRequired, "media policy is not committed in this lineage")
	}
	backend, ok := c.executionPolicy.backendByName[item.ExecutionTarget.Backend]
	if !ok {
		return media.Reject(media.EgressDenied, "media task backend is not frozen")
	}
	if _, err := policy.Policy.Allow(backend.Backend, backend.ProviderKey, item.ExecutionTarget.Model); err != nil {
		return err
	}
	if err := mediaBindingSources(b, view, policy.Policy.Image); err != nil {
		return err
	}
	if b.InvocationMode != "" {
		if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
			return err
		}
		return validateNativeMediaSourceScope(b, view.events)
	}
	return validateMediaInvocationSourceOwner(b, view.events)
}

// finalizeMediaTaskSpec assigns the runtime-reserved owner before decision
// admission hashes the occurrence. A pre-bound task cannot be silently copied
// into a new task, even if its image content happens to be the same.
func (c *Coordinator) finalizeMediaTaskSpec(ctx context.Context, spec *TodoSpec, id string) error {
	if spec.MediaBinding == nil {
		if spec.MediaInputDigest != "" {
			return media.Reject(media.BindingConflict, "media digest has no binding")
		}
		return nil
	}
	b := spec.MediaBinding.Clone()
	if b.TaskID != "" || b.Agent != strings.ToLower(strings.TrimSpace(spec.Agent)) || spec.MediaInputDigest != b.InputDigest {
		return media.Reject(media.AccessDenied, "new task cannot inherit an existing media owner")
	}
	b.TaskID = id
	sealed, err := b.Seal()
	if err != nil {
		return err
	}
	spec.MediaBinding = &sealed
	spec.MediaInputDigest = sealed.InputDigest
	return c.validateTaskMediaCreation(ctx, todoItemFromSpec(*spec, id))
}

func (c *Coordinator) appendMediaTaskCreation(ctx context.Context, event RunEvent, item *TodoItem) error {
	if item.MediaBinding == nil {
		_, err := c.EventJournal().Append(ctx, event)
		return err
	}
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok {
		return media.Reject(media.DurabilityRequired, "media task requires atomic occurrence binding")
	}
	payload, err := json.Marshal(TaskMediaBoundPayload{Version: media.Version, OccurrenceRevision: item.OccurrenceRevision, Binding: *item.MediaBinding.Clone()})
	if err != nil {
		return err
	}
	bound := RunEvent{Type: string(EventTaskMediaBound), Actor: item.Agent, RunID: item.MediaBinding.RunID, BranchID: item.MediaBinding.BranchID, TaskID: item.ID,
		IdempotencyKey: "task-media:" + item.MediaBinding.BranchID + ":" + item.ID + ":" + item.MediaBinding.Hash, Payload: payload}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return err
	}
	for range 16 {
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return err
		}
		if view.policyHash != policy.Hash || item.MediaBinding.BranchID != view.branch || item.MediaBinding.RunID != c.executionRunID {
			return media.Reject(media.AccessDenied, "media task admission changed before commit")
		}
		if err := mediaBindingSources(item.MediaBinding, view, policy.Policy.Image); err != nil {
			return err
		}
		if item.MediaBinding.InvocationMode != "" {
			if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
				return err
			}
			if err := validateNativeMediaSourceScope(item.MediaBinding, view.events); err != nil {
				return err
			}
		} else if err := validateMediaInvocationSourceOwner(item.MediaBinding, view.events); err != nil {
			return err
		}
		_, err = journal.AppendBatchAtHead(ctx, []RunEvent{event, bound}, view.head)
		if errors.Is(err, ErrEventHeadChanged) {
			continue
		}
		return err
	}
	return ErrEventHeadChanged
}

// validateMediaAttempt checks the current durable owner and the exact lease.
// It never fills missing lease fields from a newer occurrence. Provider byte
// access and transport require their additional destination/capability checks.
func (c *Coordinator) validateMediaAttempt(ctx context.Context, task TaskDef, taskID string) error {
	item := c.todoItemByID(taskID)
	if item == nil {
		return media.Reject(media.AccessDenied, "media occurrence is unavailable")
	}
	if !reflect.DeepEqual(task.MediaBinding, item.MediaBinding) || task.MediaInputDigest != item.MediaInputDigest {
		return media.Reject(media.BindingConflict, "attempt media assertion differs from canonical task")
	}
	if err := validateTaskMediaShape(item); err != nil {
		return err
	}
	if item.MediaBinding == nil {
		return nil
	}
	identity, ok := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity)
	active, opened := c.activeTaskResultOccurrence(taskID)
	if !ok || !opened || item.Status != TaskInProgress || !validSubmitResultIdentity(identity) || !sameTaskResultOccurrence(active, identity) || identity.OccurrenceRevision != item.OccurrenceRevision || identity.DispatchID != item.DispatchID || identity.Agent != item.MediaBinding.Agent {
		return media.Reject(media.AccessDenied, "media attempt lease is missing or stale")
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return err
	}
	if policy.Hash != item.MediaBinding.PolicyHash {
		return media.Reject(media.BindingConflict, "media execution policy changed")
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	if view.policyHash != policy.Hash {
		return media.Reject(media.AccessDenied, "media execution policy is not committed in this lineage")
	}
	if err := mediaBindingSources(item.MediaBinding, view, policy.Policy.Image); err != nil {
		return err
	}
	return validateMediaOccurrenceBinding(item, view.events)
}

func validateMediaOccurrenceBinding(item *TodoItem, events []RunEvent) error {
	created, bound := false, false
	for index, e := range events {
		if e.TaskID != item.ID {
			continue
		}
		if e.Type == string(EventTaskCreated) {
			_, reset, err := decodeMediaTaskReset(e)
			if err != nil {
				return err
			}
			if reset {
				if err := verifyMediaTaskReset(e, events[:index]); err != nil {
					return err
				}
				continue
			}
			var projection struct {
				Binding *media.TaskBinding `json:"media_binding"`
				Digest  string             `json:"media_input_digest"`
			}
			if err := json.Unmarshal(e.Payload, &projection); err != nil {
				return err
			}
			created = reflect.DeepEqual(projection.Binding, item.MediaBinding) && projection.Digest == item.MediaInputDigest && e.RunID == item.MediaBinding.RunID && e.BranchID == item.MediaBinding.BranchID
		}
		if e.Type == string(EventTaskMediaBound) {
			payload, err := decodeTaskMediaEvent(e)
			if err != nil {
				return err
			}
			bound = created && reflect.DeepEqual(&payload.Binding, item.MediaBinding)
		}
	}
	if !created || !bound {
		return media.Reject(media.AccessDenied, "media occurrence lacks canonical binding events")
	}
	return nil
}
