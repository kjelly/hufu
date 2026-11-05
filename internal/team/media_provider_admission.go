package team

import (
	"context"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

// nativeProviderMedia is metadata, never a raw byte grant. A compiler must
// reserve resources and recheck the current lease before it reads private CAS.
type nativeProviderMedia struct {
	Identity    submitResultRuntimeIdentity
	Binding     *media.TaskBinding
	Target      execution.ExecutionTarget
	Grant       media.Target
	Limits      media.Limits
	Locations   []historyReferenceLocation
	ImageTokens int64
}

func providerMediaMessages(request agent.ProviderRequest) ([]fantasy.Message, error) {
	if request.Call != nil && request.ObjectCall != nil || len(request.Messages) > 0 && (request.Call != nil || request.ObjectCall != nil) {
		return nil, media.Reject(media.BindingConflict, "native provider request has ambiguous message sources")
	}
	if request.Call != nil {
		return request.Call.Prompt, nil
	}
	if request.ObjectCall != nil {
		return request.ObjectCall.Prompt, nil
	}
	return request.Messages, nil
}

func (c *Coordinator) resolveNativeProviderMedia(ctx context.Context, request agent.ProviderRequest) (nativeProviderMedia, error) {
	var admission nativeProviderMedia
	if err := ctx.Err(); err != nil {
		return admission, err
	}
	if c == nil || c.providerManager == nil || !request.AdmissionContext.IsBound() || request.AdmissionContext.ModelID != request.ModelID {
		return admission, media.Reject(media.EgressDenied, "image provider is not bound to a resolved model")
	}
	identity, item, err := c.nativeProviderOccurrence(ctx)
	if err != nil {
		return admission, err
	}
	if ctx.Value(leafExecutionKey{}) != nil {
		return admission, media.Reject(media.Unsupported, "native image invocation cannot execute a model fanout leaf")
	}
	if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
		return admission, err
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return admission, err
	}
	if err := validateNativeMediaSourceScope(item.MediaBinding, view.events); err != nil {
		return admission, err
	}
	target, grant, limits, err := c.nativeProviderTarget(ctx, request, item)
	if err != nil {
		return admission, err
	}
	messages, err := providerMediaMessages(request)
	if err != nil {
		return admission, err
	}
	if _, err := agent.ValidatePersistentMessages(messages); err != nil {
		return admission, err
	}
	locations := historyReferences(messages)
	if err := validateNativeProviderReferences(messages, locations, item.MediaBinding, limits); err != nil {
		return admission, err
	}
	return nativeProviderMedia{Identity: identity, Binding: item.MediaBinding.Clone(), Target: target, Grant: grant, Limits: limits, Locations: locations,
		ImageTokens: int64(len(locations)) * grant.ImageTokenUpperBound}, nil
}

func (c *Coordinator) nativeProviderOccurrence(ctx context.Context) (submitResultRuntimeIdentity, *TodoItem, error) {
	identity, ok := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity)
	if !ok || identity.RunID != c.executionRunID {
		return identity, nil, media.Reject(media.AccessDenied, "image provider has no current task lease")
	}
	item := c.todoItemByID(identity.TaskID)
	if item == nil || item.MediaBinding == nil {
		return identity, nil, media.Reject(media.AccessDenied, "image provider has no canonical media occurrence")
	}
	if err := c.validateMediaAttempt(ctx, taskDefFromTodoItem(item), item.ID); err != nil {
		return identity, nil, err
	}
	purpose, err := c.providerCostIdentity(ctx, "media-preflight")
	if err != nil || (purpose.Purpose != cost.PurposeWorker && purpose.Purpose != cost.PurposeDirectAgent) ||
		!nativeProviderPurposeMatches(purpose, identity) || currentResultCollection(item) != nil {
		return identity, nil, media.Reject(media.AccessDenied, "image provider is outside the worker execution purpose")
	}
	return identity, item, nil
}

func nativeProviderPurposeMatches(purpose cost.InvocationIdentity, identity submitResultRuntimeIdentity) bool {
	return purpose.TaskID == identity.TaskID && purpose.OccurrenceAttempt == identity.Attempt && strings.EqualFold(purpose.Agent, identity.Agent)
}

func (c *Coordinator) nativeProviderTarget(ctx context.Context, request agent.ProviderRequest, item *TodoItem) (execution.ExecutionTarget, media.Target, media.Limits, error) {
	target, err := c.resolveCanonicalTaskTarget(request.ModelID, "")
	if err != nil || target != item.ExecutionTarget {
		return target, media.Target{}, media.Limits{}, media.Reject(media.EgressDenied, "image provider differs from the canonical task target")
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return target, media.Target{}, media.Limits{}, err
	}
	backend, found := c.executionPolicy.backendByName[target.Backend]
	if !found || backend.Kind != string(execution.BackendKindLLM) {
		return target, media.Target{}, media.Limits{}, media.Reject(media.EgressDenied, "image provider backend is not a frozen LLM")
	}
	grant, err := policy.Policy.Allow(target.Backend, backend.ProviderKey, target.Model)
	if err != nil {
		return target, grant, media.Limits{}, err
	}
	if grant.ImageTokenUpperBound <= 0 {
		return target, grant, media.Limits{}, media.Reject(media.Unsupported, "image provider has no conservative token bound")
	}
	if err := c.validateNativeProviderIdentity(ctx, request, target, grant); err != nil {
		return target, grant, media.Limits{}, err
	}
	return target, grant, policy.Policy.Image, nil
}

func (c *Coordinator) validateNativeProviderIdentity(ctx context.Context, request agent.ProviderRequest, target execution.ExecutionTarget, grant media.Target) error {
	bound := request.AdmissionContext
	if bound.ProviderIdentity != grant.ProviderKey || bound.ProviderConfigurationHash == "" || bound.ProviderConfigurationHash != grant.IdentityHash {
		return media.Reject(media.EgressDenied, "image provider configuration differs from the frozen grant")
	}
	actual, err := c.providerManager.ResolveProviderExecutionIdentity(request.ModelID)
	if err != nil || actual.ProviderKey != bound.ProviderIdentity || actual.ConfigurationHash != bound.ProviderConfigurationHash {
		return media.Reject(media.EgressDenied, "image provider configuration changed after profile resolution")
	}
	if bound.ImageInputCapability != providerintrospection.CapabilityYes || !trustedImageCapabilitySource(bound.ImageInputSource) {
		return media.Reject(media.Unsupported, "model image input capability is unknown or denied")
	}
	backend, _, err := c.gatedAgentBackendForTarget(target)
	if err != nil || !backend.Capabilities().NativeImageInput {
		return media.Reject(media.Unsupported, "backend does not declare native image input")
	}
	provider, err := backend.AgentProvider(ctx, target)
	if err != nil || !provider.SupportsNativeImageInput() {
		return media.Reject(media.Unsupported, "adapter does not declare native image input")
	}
	return nil
}

func trustedImageCapabilitySource(source string) bool {
	switch modelprofile.MetadataSource(source) {
	case modelprofile.SourceOperator, modelprofile.SourceProviderRuntime, modelprofile.SourceProviderMetadata, modelprofile.SourceCatalog:
		return true
	}
	return false
}

func validateNativeProviderReferences(messages []fantasy.Message, locations []historyReferenceLocation, binding *media.TaskBinding, limits media.Limits) error {
	if len(locations) == 0 || len(locations) != len(binding.Selections) || len(locations) != len(binding.Refs) {
		return media.Reject(media.BindingConflict, "image provider input differs from the bound image count")
	}
	for index, location := range locations {
		reference := location.Reference
		if messages[location.Message].Role != fantasy.MessageRoleUser || reference.Source != binding.Selections[index] || reference.Ref != binding.Refs[index] || reference.PolicyHash != binding.PolicyHash {
			return media.Reject(media.AccessDenied, "image provider input is outside the ordered task selection")
		}
		if reference.TaskID != "" && (reference.TaskID != binding.TaskID || reference.BindingHash != binding.Hash) {
			return media.Reject(media.AccessDenied, "image provider input belongs to another task occurrence")
		}
	}
	return media.CheckBundle(binding.Refs, limits)
}
