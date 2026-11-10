package team

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

const mediaProviderFanoutGrantVersion = 1

// MediaProviderFanoutGrant connects a private leaf attempt to the live parent
// occurrence that owns its immutable input. All fields are runtime-owned.
// Keep it comparable so the final HTTP gate compares complete permissions.
type MediaProviderFanoutGrant struct {
	Version          int    `json:"version"`
	TopologyIndex    int    `json:"topology_index"`
	ModelExecutionID string `json:"model_execution_id"`
	ParentRevision   int    `json:"parent_occurrence_revision"`
	ParentAttempt    int    `json:"parent_attempt"`
	ParentDispatchID string `json:"parent_dispatch_id"`
}

func (g MediaProviderFanoutGrant) present() bool { return g != (MediaProviderFanoutGrant{}) }

func (g MediaProviderFanoutGrant) validate() error {
	if g.Version != mediaProviderFanoutGrantVersion || g.TopologyIndex < 0 || g.TopologyIndex >= maxConcurrentModels ||
		g.ModelExecutionID == "" || len(g.ModelExecutionID) > 256 || g.ParentRevision < 1 || g.ParentAttempt < 1 ||
		strings.TrimSpace(g.ParentDispatchID) == "" || len(g.ParentDispatchID) > 256 {
		return media.Reject(media.AccessDenied, "image fanout grant has an invalid runtime identity")
	}
	return nil
}

func nativeMediaHasModelFanout(item *TodoItem) bool {
	return item != nil && (len(item.ExecutionTopology) > 1 || len(item.ModelTopology) > 1)
}

func validateNativeMediaModelTopology(item *TodoItem, policy *ExecutionPolicySnapshot) error {
	if !nativeMediaHasModelFanout(item) {
		return nil
	}
	if policy == nil {
		return media.Reject(media.DurabilityRequired, "image model fanout has no frozen policy")
	}
	if len(item.ExecutionTopology) < 2 || item.ExecutionTopology[0] != item.ExecutionTarget ||
		len(item.ModelTopology) > 0 && len(item.ModelTopology) != len(item.ExecutionTopology) ||
		item.ExecutionRoute != nil && len(item.ExecutionRoute.Candidates) > 1 {
		return media.Reject(media.Unsupported, "image model fanout differs from its canonical topology")
	}
	for _, target := range item.ExecutionTopology {
		if target.Validate() != nil || !nativeMediaTopologyTargetFrozen(target, policy) {
			return media.Reject(media.Unsupported, "image model fanout requires frozen LLM targets")
		}
	}
	return nil
}

func nativeMediaTopologyTargetFrozen(target execution.ExecutionTarget, policy *ExecutionPolicySnapshot) bool {
	for _, backend := range policy.Backends {
		if backend.Backend != target.Backend || backend.Kind != string(execution.BackendKindLLM) {
			continue
		}
		for _, route := range policy.ModelRoutes {
			if route.Backend == target.Backend && route.ProviderKey == backend.ProviderKey &&
				(route.Model == target.Model || mediaProfileMatchesTarget(route.Model, target)) {
				return true
			}
		}
	}
	return false
}

func nativeMediaFanoutExecutionID(item *TodoItem, runID string, index int) (string, error) {
	if !nativeMediaHasModelFanout(item) || index < 0 || index >= len(item.ExecutionTopology) || index >= maxConcurrentModels {
		return "", media.Reject(media.AccessDenied, "image fanout slot is outside the canonical topology")
	}
	slot := "main"
	if index > 0 {
		slot = fmt.Sprintf("extra-%d", index)
	}
	return modelFanoutExecutionID(runID, item.ID, item.Agent, item.ExecutionTopology[index].String(), slot, item.Retries), nil
}

// nativeMediaFanoutParent validates immutable parent/leaf input identity. The
// early metadata preflight needs no parent byte lease; provider admission and
// the final transport gate separately require the parent's live lease below.
func (c *Coordinator) nativeMediaFanoutParent(item *TodoItem) (*TodoItem, execution.ExecutionTarget, error) {
	if item == nil || !c.isModelFanoutLeaf(item.ID) || c.modelFanout.parent == nil {
		return nil, execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image leaf has no runtime fanout binding")
	}
	parent := c.modelFanout.parent.todoItemByID(item.ID)
	if parent == nil || !reflect.DeepEqual(parent.MediaBinding, item.MediaBinding) || parent.MediaInputDigest != item.MediaInputDigest ||
		!strings.EqualFold(parent.Agent, item.Agent) {
		return nil, execution.ExecutionTarget{}, media.Reject(media.BindingConflict, "image leaf differs from its canonical parent input")
	}
	target, err := c.modelFanoutTarget(item.ID)
	if err != nil {
		return nil, execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image leaf target differs from its admitted parent slot")
	}
	return parent, target, nil
}

func (c *Coordinator) nativeMediaProviderFanoutGrant(ctx context.Context, item *TodoItem) (MediaProviderFanoutGrant, execution.ExecutionTarget, error) {
	parentItem, target, err := c.nativeMediaFanoutParent(item)
	if err != nil {
		return MediaProviderFanoutGrant{}, target, err
	}
	parent := c.modelFanout.parent
	identity, opened := parent.activeTaskResultOccurrence(item.ID)
	if !opened || !validSubmitResultIdentity(identity) || identity.RunID != c.executionRunID || identity.RunID != parent.executionRunID ||
		identity.TaskID != item.ID || !strings.EqualFold(identity.Agent, parentItem.Agent) || parentItem.Status != TaskInProgress ||
		identity.OccurrenceRevision != parentItem.OccurrenceRevision || identity.DispatchID != parentItem.DispatchID {
		return MediaProviderFanoutGrant{}, target, media.Reject(media.AccessDenied, "image leaf parent lease is missing or stale")
	}
	expected, err := nativeMediaFanoutExecutionID(parentItem, identity.RunID, c.modelFanout.index)
	metadata, found := invocationMetadataFromContext(ctx)
	if err != nil || c.modelExecutionID != expected || !found || metadata.ModelExecutionID != expected {
		return MediaProviderFanoutGrant{}, target, media.Reject(media.AccessDenied, "image leaf model execution identity differs from its parent slot")
	}
	grant := MediaProviderFanoutGrant{Version: mediaProviderFanoutGrantVersion, TopologyIndex: c.modelFanout.index, ModelExecutionID: expected,
		ParentRevision: identity.OccurrenceRevision, ParentAttempt: identity.Attempt, ParentDispatchID: identity.DispatchID}
	return grant, target, grant.validate()
}

func verifyNativeMediaProviderOwner(event RunEvent, payload MediaProviderAdmittedPayload, item *TodoItem, prior []RunEvent) (execution.ExecutionTarget, error) {
	grant := payload.ModelFanout
	if !grant.present() {
		if nativeMediaHasModelFanout(item) || item.OccurrenceRevision != payload.OccurrenceRevision || item.DispatchID != "" && item.DispatchID != payload.DispatchID {
			return execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image provider admission has no matching worker lease")
		}
		return nativeMediaAttemptTarget(item, event.RunID, payload.Attempt, prior)
	}
	if err := grant.validate(); err != nil {
		return execution.ExecutionTarget{}, err
	}
	expectedID, err := nativeMediaFanoutExecutionID(item, event.RunID, grant.TopologyIndex)
	if err != nil || expectedID != grant.ModelExecutionID || grant.ParentRevision != item.OccurrenceRevision || grant.ParentAttempt != item.Retries+1 ||
		item.DispatchID != "" && item.DispatchID != grant.ParentDispatchID || payload.OccurrenceRevision <= grant.ParentRevision {
		return execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image leaf admission differs from its canonical parent or execution slot")
	}
	return item.ExecutionTopology[grant.TopologyIndex], nil
}
