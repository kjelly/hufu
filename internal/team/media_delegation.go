package team

import (
	"context"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

// bindDelegatedImageInputs resolves model selections before deduplication or
// cache lookup. Only the caller's live metadata view or native task binding
// may contribute sources; source IDs supplied by a model never grant access.
func (c *Coordinator) bindDelegatedImageInputs(ctx context.Context, tasks []TaskDef) ([]TaskDef, error) {
	if !slices.ContainsFunc(tasks, func(task TaskDef) bool { return len(task.ImageInputs) > 0 }) {
		return tasks, nil
	}
	sources, err := c.delegatedImageSources(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return nil, err
	}
	available := make(map[media.ImageInputSelector]media.HistoryReference)
	ambiguous := make(map[media.ImageInputSelector]bool)
	for _, source := range sources {
		key := media.ImageInputSelector{TurnID: source.Source.TurnID, PartIndex: source.Source.PartIndex}
		if previous, exists := available[key]; exists && (previous.Source != source.Source || previous.Ref != source.Ref || previous.PolicyHash != source.PolicyHash) {
			ambiguous[key] = true
		}
		available[key] = source
	}
	tasks = slices.Clone(tasks)
	for index := range tasks {
		task := &tasks[index]
		if len(task.ImageInputs) == 0 {
			continue
		}
		if taskHasMedia(*task) || len(task.ImageInputs) > policy.Policy.Image.MaxCount || task.Sidecar || task.Action != nil || task.CatalogAction != nil {
			return nil, media.Reject(media.BindingConflict, "image selection conflicts with the delegated task contract")
		}
		worker, err := c.nativeImageWorkerDefinition(task.Agent)
		if err != nil {
			return nil, err
		}
		binding := media.TaskBinding{Version: media.Version, RunID: c.executionRunID, BranchID: c.activeBranchID(),
			Agent: strings.ToLower(strings.TrimSpace(worker.Name)), Purpose: "image_analysis", InvocationMode: media.InvocationDelegatedTask, PolicyHash: policy.Hash}
		for _, selector := range task.ImageInputs {
			if err := selector.Validate(); err != nil {
				return nil, err
			}
			source, found := available[selector]
			if !found || ambiguous[selector] || source.PolicyHash != policy.Hash {
				return nil, media.Reject(media.AccessDenied, "image selection is unavailable, ambiguous or outside the caller's grant")
			}
			binding.Selections = append(binding.Selections, source.Source)
			binding.Refs = append(binding.Refs, source.Ref)
		}
		if err := media.CheckBundle(binding.Refs, policy.Policy.Image); err != nil {
			return nil, err
		}
		binding, err = binding.Seal()
		if err != nil {
			return nil, err
		}
		task.Agent = worker.Name
		task.MediaBinding, task.MediaInputDigest, task.ImageInputs = &binding, binding.InputDigest, nil
	}
	return tasks, nil
}

func (c *Coordinator) delegatedImageSources(ctx context.Context) ([]media.HistoryReference, error) {
	taskID, _ := ctx.Value(todoIDKey{}).(string)
	if taskID == CoordTodoID {
		scope, _ := ctx.Value(coordinatorReferenceMetadataKey{}).(*coordinatorReferenceMetadata)
		if scope == nil {
			return nil, media.Reject(media.AccessDenied, "image delegation requires an explicit coordinator metadata grant")
		}
		message := fantasy.Message{Role: fantasy.MessageRoleUser}
		for _, reference := range scope.references {
			message.Content = append(message.Content, agent.MediaReferencePart{Reference: reference})
		}
		request := agent.ProviderRequest{ModelID: scope.bound.ModelID, AdmissionContext: scope.bound, Messages: []fantasy.Message{message}}
		allowed, err := c.resolveProviderReferenceMetadata(ctx, request)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, media.Reject(media.AccessDenied, "coordinator metadata grant no longer permits delegation")
		}
		return slices.Clone(scope.references), nil
	}
	_, parent, err := c.nativeProviderOccurrence(ctx)
	if err != nil {
		return nil, err
	}
	sources := make([]media.HistoryReference, len(parent.MediaBinding.Selections))
	for index, selection := range parent.MediaBinding.Selections {
		sources[index] = nativeWorkerReference(parent.MediaBinding, index, selection).Reference
	}
	return sources, nil
}
