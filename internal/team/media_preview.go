package team

import (
	"context"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

// NativeImagePreview describes local validation, never durable admission or a
// byte-access grant. Provider/model capability remains unknown without I/O.
type NativeImagePreview struct {
	Target               execution.ExecutionTarget
	ImageCount           int
	TotalBytes           int64
	ModelImageCapability providerintrospection.CapabilityState
	MetadataOnly         bool
}

// PreviewNativeImageTask validates selected local images and the frozen target
// policy without creating storage, history, events, or provider requests.
func (c *Coordinator) PreviewNativeImageTask(ctx context.Context, agentName, goal string, request MediaTurnRequest) (NativeImagePreview, error) {
	var preview NativeImagePreview
	if err := ctx.Err(); err != nil {
		return preview, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return preview, err
	}
	if strings.TrimSpace(goal) == "" || len(goal) > 256<<10 || len(request.Images) == 0 || len(request.Images) > policy.Policy.Image.MaxCount {
		return preview, media.Reject(media.LimitExceeded, "invalid native image preview input")
	}
	def, err := c.nativeImageWorkerDefinition(agentName)
	if err != nil {
		return preview, err
	}
	legacyProvider := def.SubagentProvider
	if legacyProvider == "" {
		legacyProvider = c.session.Config.SubagentProviderDefault
	}
	modelID := c.resolveAgentModel(def, "")
	target, err := c.resolveCanonicalTaskTarget(modelID, legacyProvider)
	if err != nil {
		return preview, err
	}
	backend, err := c.ExecutionRegistry().LanguageModelBackend(target)
	if err != nil || !backend.Capabilities().NativeImageInput {
		return preview, media.Reject(media.Unsupported, "image preview requires a native image backend")
	}
	frozen, ok := c.executionPolicy.backendByName[target.Backend]
	if !ok {
		return preview, media.Reject(media.EgressDenied, "image preview has no frozen backend")
	}
	grant, err := policy.Policy.Allow(target.Backend, frozen.ProviderKey, target.Model)
	if err != nil {
		return preview, err
	}
	identity, err := c.providerManager.ResolveProviderExecutionIdentity(modelID)
	if err != nil || identity.ProviderKey != frozen.ProviderKey || identity.ConfigurationHash != frozen.IdentityHash || grant.IdentityHash != frozen.IdentityHash {
		return preview, media.Reject(media.EgressDenied, "image preview provider differs from frozen policy")
	}
	preview, err = previewMediaTurnFiles(ctx, request, policy.Policy.Image)
	if err != nil {
		return preview, err
	}
	preview.Target = target
	preview.ModelImageCapability = providerintrospection.CapabilityUnknown
	return preview, nil
}

// PreviewCoordinatorImageInput validates local inputs for a metadata-only
// coordinator. A later selected worker still needs independent vision gates.
func (c *Coordinator) PreviewCoordinatorImageInput(ctx context.Context, goal string, request MediaTurnRequest) (NativeImagePreview, error) {
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return NativeImagePreview{}, err
	}
	if strings.TrimSpace(goal) == "" || len(goal) > 256<<10 || c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return NativeImagePreview{}, media.Reject(media.Unsupported, "invalid or unsupported coordinator image preview")
	}
	modelID := c.resolveAgentModel(c.GetOrchestratorDef(), "")
	target, err := c.resolveCanonicalTaskTarget(modelID, "")
	if err != nil {
		return NativeImagePreview{}, err
	}
	if _, err := c.ExecutionRegistry().LanguageModelBackend(target); err != nil {
		return NativeImagePreview{}, err
	}
	frozen, ok := c.executionPolicy.backendByName[target.Backend]
	if !ok {
		return NativeImagePreview{}, media.Reject(media.EgressDenied, "coordinator preview has no frozen backend")
	}
	identity, err := c.providerManager.ResolveProviderExecutionIdentity(modelID)
	if err != nil || identity.ProviderKey != frozen.ProviderKey || identity.ConfigurationHash != frozen.IdentityHash {
		return NativeImagePreview{}, media.Reject(media.EgressDenied, "coordinator preview provider differs from frozen policy")
	}
	preview, err := previewMediaTurnFiles(ctx, request, policy.Policy.Image)
	preview.Target, preview.MetadataOnly = target, true
	return preview, err
}

func previewMediaTurnFiles(ctx context.Context, request MediaTurnRequest, maxLimits media.Limits) (NativeImagePreview, error) {
	var preview NativeImagePreview
	if len(request.Images) == 0 || len(request.Images) > maxLimits.MaxCount {
		return preview, media.Reject(media.LimitExceeded, "invalid image preview count")
	}
	var refs []media.Ref
	for _, path := range request.Images {
		remaining := maxLimits.MaxBytesPerBundle - preview.TotalBytes
		if remaining <= 0 {
			return preview, media.Reject(media.LimitExceeded, "image preview exceeds bundle budget")
		}
		limits := maxLimits
		if remaining < limits.MaxBytesPerImage {
			limits.MaxBytesPerImage = remaining
		}
		data, err := media.ReadFile(ctx, path, limits)
		if err != nil {
			return preview, err
		}
		ref, err := media.Validate(ctx, data, limits)
		clear(data)
		if err != nil {
			return preview, err
		}
		refs = append(refs, ref)
		preview.TotalBytes += ref.ByteSize
	}
	if err := media.CheckBundle(refs, maxLimits); err != nil {
		return preview, err
	}
	preview.ImageCount = len(refs)
	preview.ModelImageCapability = providerintrospection.CapabilityUnknown
	return preview, nil
}

func (c *Coordinator) nativeImageWorkerDefinition(name string) (*agent.AgentDef, error) {
	def, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil || def == nil {
		return nil, media.Reject(media.AccessDenied, "native image input requires a known worker")
	}
	switch strings.ToLower(strings.TrimSpace(def.Role)) {
	case "coordinator", "orchestrator", "sidecar":
		return nil, media.Reject(media.Unsupported, "native image input requires an explicit worker")
	}
	return def, nil
}
