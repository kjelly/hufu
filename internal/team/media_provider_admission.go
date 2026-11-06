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
	ModelFanout MediaProviderFanoutGrant
	References  []media.Ref
	Consumer    *MediaConsumerBoundPayload
}

func providerMediaMessages(request agent.ProviderRequest) ([]fantasy.Message, error) {
	var err error
	request, err = agent.ExpandToolMediaRequest(request)
	if err != nil {
		return nil, err
	}
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
	if item.MediaBinding != nil {
		if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
			return admission, err
		}
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		return admission, err
	}
	if item.MediaBinding != nil {
		if err := validateNativeMediaSourceScope(item.MediaBinding, view.events); err != nil {
			return admission, err
		}
	}
	expectedTarget, fanout, err := c.nativeMediaProviderTarget(ctx, item, identity, view.events)
	if err != nil {
		return admission, err
	}
	target, grant, limits, err := c.nativeProviderTarget(ctx, request, expectedTarget)
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
	toolSources := make(map[string]bool)
	for _, event := range view.events {
		if event.Type == string(EventToolMediaPublished) {
			producer, err := decodeToolMediaPublication(event)
			if err != nil {
				return admission, err
			}
			toolSources[producer.Hash] = true
		}
	}
	hasToolSource := false
	for _, location := range locations {
		hasToolSource = hasToolSource || toolSources[location.Reference.BindingHash]
	}
	if hasToolSource {
		if err := validateToolPairs(messages); err != nil {
			return admission, err
		}
		consumer := MediaConsumerBoundPayload{Version: media.Version, RunID: identity.RunID, TaskID: identity.TaskID, Agent: identity.Agent,
			ProviderInvocationID: request.InvocationID, OccurrenceRevision: identity.OccurrenceRevision, Attempt: identity.Attempt, DispatchID: identity.DispatchID,
			PolicyHash: view.policyHash, Target: target, ModelFanout: fanout}
		if item.MediaBinding != nil {
			consumer.InputBindingHash = item.MediaBinding.Hash
		}
		for _, location := range locations {
			if messages[location.Message].Role != fantasy.MessageRoleUser {
				return admission, media.Reject(media.AccessDenied, "tool image projection is not a complete user media bundle")
			}
			consumer.References = append(consumer.References, location.Reference)
		}
		consumer.Hash, err = consumer.digest()
		if err != nil {
			return admission, err
		}
		if err := verifyMediaConsumer(RunEvent{RunID: identity.RunID, TaskID: identity.TaskID, Actor: identity.Agent}, consumer, view.events); err != nil {
			return admission, err
		}
		return nativeProviderMedia{Identity: identity, Binding: item.MediaBinding.Clone(), Target: target, Grant: grant, Limits: limits, Locations: locations,
			ImageTokens: int64(len(locations)) * grant.ImageTokenUpperBound, ModelFanout: fanout, References: consumer.refs(), Consumer: &consumer}, nil
	}
	if item.MediaBinding == nil {
		return admission, media.Reject(media.AccessDenied, "image provider has no canonical media source")
	}
	if err := validateNativeProviderReferences(messages, locations, item.MediaBinding, limits); err != nil {
		return admission, err
	}
	return nativeProviderMedia{Identity: identity, Binding: item.MediaBinding.Clone(), Target: target, Grant: grant, Limits: limits, Locations: locations,
		ImageTokens: int64(len(locations)) * grant.ImageTokenUpperBound, ModelFanout: fanout, References: item.MediaBinding.Refs}, nil
}

func (c *Coordinator) nativeMediaProviderTarget(ctx context.Context, item *TodoItem, identity submitResultRuntimeIdentity, events []RunEvent) (execution.ExecutionTarget, MediaProviderFanoutGrant, error) {
	if ctx.Value(leafExecutionKey{}) != nil || c.isModelFanoutLeaf(item.ID) {
		if leaf, _ := ctx.Value(leafExecutionKey{}).(bool); !leaf {
			return execution.ExecutionTarget{}, MediaProviderFanoutGrant{}, media.Reject(media.AccessDenied, "image provider has no runtime leaf context")
		}
		grant, target, err := c.nativeMediaProviderFanoutGrant(ctx, item)
		return target, grant, err
	}
	if nativeMediaHasModelFanout(item) {
		return execution.ExecutionTarget{}, MediaProviderFanoutGrant{}, media.Reject(media.AccessDenied, "image model fanout must dispatch an admitted leaf")
	}
	target, err := nativeMediaAttemptTarget(item, identity.RunID, identity.Attempt, events)
	return target, MediaProviderFanoutGrant{}, err
}

func (c *Coordinator) nativeProviderOccurrence(ctx context.Context) (submitResultRuntimeIdentity, *TodoItem, error) {
	return c.toolMediaOccurrence(ctx)
}

func nativeProviderPurposeMatches(purpose cost.InvocationIdentity, identity submitResultRuntimeIdentity) bool {
	return purpose.TaskID == identity.TaskID && purpose.OccurrenceAttempt == identity.Attempt && strings.EqualFold(purpose.Agent, identity.Agent)
}

func (c *Coordinator) nativeProviderTarget(ctx context.Context, request agent.ProviderRequest, expected execution.ExecutionTarget) (execution.ExecutionTarget, media.Target, media.Limits, error) {
	target, err := c.resolveCanonicalTaskTarget(request.ModelID, "")
	if err != nil || target != expected {
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
