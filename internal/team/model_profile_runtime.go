package team

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

// ModelProfileRuntime owns provider-bound profile resolution for one
// coordinator while its resolver uses the process-shared runtime cache. It is
// intentionally separate from coordinator task execution:
// admission receives an immutable projection, while the cache retains all
// lower-authority runtime evidence for later refreshes.
type ModelProfileRuntime struct {
	manager  *agent.ProviderManager
	noNet    bool
	resolver *modelprofile.RuntimeResolver
}

// ResolvedModelProfile keeps the profile, effective provider reference, and
// immutable admission projection from one resolution. Provider-bound callers
// must use this joint result so admission cannot silently perform a second
// provider lookup with different state.
type ResolvedModelProfile struct {
	Profile           modelprofile.ModelProfile
	Provider          providerintrospection.ProviderRef
	Admission         agent.ProviderAdmissionContext
	ProfileResolution modelprofile.TelemetryProjection
}

type providerBoundInvocationContext struct {
	ModelID          string
	AdmissionContext agent.ProviderAdmissionContext
	ModelContext     ModelContextSpec
}

type providerBoundInvocationContextKey struct{}

func withProviderBoundInvocationContext(ctx context.Context, bound providerBoundInvocationContext) context.Context {
	return context.WithValue(ctx, providerBoundInvocationContextKey{}, bound)
}

func providerBoundInvocationContextFromContext(ctx context.Context, modelID string) (providerBoundInvocationContext, bool) {
	if ctx == nil {
		return providerBoundInvocationContext{}, false
	}
	bound, ok := ctx.Value(providerBoundInvocationContextKey{}).(providerBoundInvocationContext)
	if !ok {
		return providerBoundInvocationContext{}, false
	}
	if strings.TrimSpace(modelID) != "" && !strings.EqualFold(strings.TrimSpace(bound.ModelID), strings.TrimSpace(modelID)) {
		return providerBoundInvocationContext{}, false
	}
	return bound, true
}

// WarmModelProfiles performs best-effort startup profile resolution for all
// statically reachable models. Dynamic first-use paths call admissionContextFor
// and resolve through the same cache when they are encountered later.
func (c *Coordinator) WarmModelProfiles(ctx context.Context, modelIDs []string, operatorContext int) {
	if c == nil || c.modelProfileRuntime == nil {
		return
	}
	c.modelProfileRuntime.WarmModels(ctx, modelIDs, operatorContext)
}

func NewModelProfileRuntime(manager *agent.ProviderManager, noNet bool) *ModelProfileRuntime {
	catalog, err := modelcatalog.NewDefaultStore().Load()
	if err != nil {
		return NewModelProfileRuntimeWithCatalog(manager, noNet, nil)
	}
	return NewModelProfileRuntimeWithCatalog(manager, noNet, catalog)
}

// NewModelProfileRuntimeWithCatalog constructs a runtime using caller-supplied
// offline catalog evidence. The catalog is never updated by this constructor.
func NewModelProfileRuntimeWithCatalog(manager *agent.ProviderManager, noNet bool, catalog modelcatalog.Reader) *ModelProfileRuntime {
	runtime := &ModelProfileRuntime{manager: manager, noNet: noNet}
	runtime.resolver = modelprofile.NewProcessRuntimeResolverWithCatalog(func(ref providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		if strings.EqualFold(ref.Type, "ollama") {
			return providerintrospection.NewOllamaIntrospector(ref.BaseURL, "")
		}
		return providerintrospection.NewOpenAICompatibleIntrospector(ref.BaseURL, "")
	}, modelprofile.ProfileCacheOptions{}, catalog)
	return runtime
}

// Profile resolves one model against the exact provider selected by
// ProviderManager. Introspection failures are best-effort; invalid provider
// identity is returned so callers cannot accidentally bind another provider.
func (r *ModelProfileRuntime) Profile(ctx context.Context, modelID string, operatorContext, maxOutputTokens int) (modelprofile.ModelProfile, error) {
	if r == nil || r.manager == nil {
		return modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{ModelID: modelID}), fmt.Errorf("model profile runtime unavailable")
	}
	ref, err := r.manager.ResolveProviderRef(modelID)
	if err != nil {
		return modelprofile.ModelProfile{}, err
	}
	ref.NoNet = r.noNet
	return r.profileForProvider(ctx, modelID, ref, operatorContext, maxOutputTokens, false, "")
}

func (r *ModelProfileRuntime) profileForProvider(ctx context.Context, modelID string, ref providerintrospection.ProviderRef, operatorContext, maxOutputTokens int, noRuntime bool, catalogProvider string) (modelprofile.ModelProfile, error) {
	legacy := GlobalModelSpecRegistry().GetSpec(modelID)
	input := modelprofile.ModelProfileInput{
		ModelID:   modelID,
		Provider:  ref.Provider,
		Estimator: modelprofile.EstimatorEvidence{Fallback: legacy.Estimator, FallbackProvenance: "legacy_model_config"},
		Context: modelprofile.ContextResolutionInput{
			Provider:        ref.Type,
			FallbackContext: legacy.ContextWindow,
		},
	}
	if strings.TrimSpace(catalogProvider) != "" {
		input.Provider, _ = modelprofile.ResolveDiagnosticCatalogIdentity(catalogProvider, ref, modelID)
	}
	if operatorContext > 0 {
		input.Context.OperatorContext = operatorContext
	}
	if maxOutputTokens > 0 {
		input.MaxOutputTokens = modelprofile.ResolvedValue[int]{Value: maxOutputTokens, Source: modelprofile.SourceOperator}
	} else if legacy.MaxOutputTokens > 0 {
		input.MaxOutputTokens = modelprofile.ResolvedValue[int]{Value: legacy.MaxOutputTokens, Source: modelprofile.SourceFallback}
	}
	profile, err := r.resolver.Resolve(ctx, modelprofile.RuntimeResolutionRequest{Provider: ref, ModelID: modelID, Profile: input, NoRuntime: noRuntime, CatalogProvider: catalogProvider})
	if err != nil {
		return profile, err
	}
	return profile, nil
}

// Diagnostic resolves a model profile for inspection. noRuntime is an
// inspection-scoped guarantee: the resolver skips the introspector entirely,
// including loopback providers, and uses only catalog/fallback evidence.
func (r *ModelProfileRuntime) Diagnostic(ctx context.Context, providerName, modelID string, operatorContext, maxOutputTokens int, noRuntime bool) (modelprofile.ModelProfile, error) {
	if r == nil || r.manager == nil {
		return modelprofile.ModelProfile{}, fmt.Errorf("model profile runtime unavailable")
	}
	qualifiedModelID := modelID
	if strings.TrimSpace(providerName) != "" {
		qualifiedModelID = providerName + "/" + modelID
	}
	ref, err := r.manager.ResolveProviderRef(qualifiedModelID)
	if err != nil {
		return modelprofile.ModelProfile{}, err
	}
	ref.NoNet = r.noNet || noRuntime
	return r.profileForProvider(ctx, modelID, ref, operatorContext, maxOutputTokens, noRuntime, providerName)
}

// ResolveAdmission resolves the effective provider, profile, and immutable
// admission context together. Profile failures retain the provider binding and
// use the same conservative context behavior as the legacy admission path.
func (r *ModelProfileRuntime) ResolveAdmission(ctx context.Context, modelID string, operatorContext, maxOutputTokens, safetyMargin int) ResolvedModelProfile {
	result := ResolvedModelProfile{}
	if r == nil || r.manager == nil || strings.TrimSpace(modelID) == "" {
		result.Admission = legacyAdmissionContext(modelID, operatorContext, maxOutputTokens, safetyMargin)
		result.Profile = modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{ModelID: modelID})
		result.ProfileResolution = modelprofile.TelemetryFromProfile(result.Profile)
		return result
	}
	ref, err := r.manager.ResolveProviderRef(modelID)
	if err != nil {
		result.Admission = legacyAdmissionContext(modelID, operatorContext, maxOutputTokens, safetyMargin)
		result.Profile = modelprofile.ResolveModelProfile(modelprofile.ModelProfileInput{ModelID: modelID})
		result.ProfileResolution = modelprofile.TelemetryFromProfile(result.Profile)
		return result
	}
	ref.NoNet = r.noNet
	result.Provider = ref
	profile, profileErr := r.profileForProvider(ctx, modelID, ref, operatorContext, maxOutputTokens, false, "")
	result.Profile = profile
	legacy := GlobalModelSpecRegistry().GetSpec(modelID)
	if safetyMargin <= 0 {
		safetyMargin = legacy.SafetyMarginTokens
	}
	if profileErr != nil {
		result.Admission = agent.ProviderAdmissionContext{
			ModelID:             modelID,
			ProviderIdentity:    ref.Provider,
			ProviderBaseURL:     ref.BaseURL,
			Bound:               true,
			ContextWindow:       operatorContext,
			MaxOutputTokens:     maxOutputTokens,
			SafetyMarginTokens:  safetyMargin,
			Estimator:           conservativeTokenEstimator,
			ContextWindowSource: "unavailable",
			IsEstimated:         operatorContext <= 0,
		}
	} else {
		result.Admission = admissionContextFromProfile(modelID, ref, profile, safetyMargin)
	}
	result.ProfileResolution = modelprofile.TelemetryFromProfile(profile)
	return result
}

func admissionContextFromProfile(modelID string, ref providerintrospection.ProviderRef, profile modelprofile.ModelProfile, safetyMargin int) agent.ProviderAdmissionContext {
	return agent.ProviderAdmissionContext{
		ModelID: modelID, ProviderIdentity: ref.Provider, ProviderBaseURL: ref.BaseURL, Bound: true,
		ContextWindow: profile.EffectiveContext, MaxOutputTokens: profile.MaxOutputTokens,
		SafetyMarginTokens: safetyMargin, Estimator: profile.Estimator,
		ContextWindowSource: string(profile.Sources.EffectiveContext.Source),
		IsEstimated:         profile.Sources.EffectiveContext.Confidence == "estimated",
	}
}

// AdmissionContext returns the secret-free immutable request projection used
// by all Generate/Stream/GenerateObject/StreamObject wrappers.
func (r *ModelProfileRuntime) AdmissionContext(ctx context.Context, modelID string, operatorContext, maxOutputTokens, safetyMargin int) agent.ProviderAdmissionContext {
	return r.ResolveAdmission(ctx, modelID, operatorContext, maxOutputTokens, safetyMargin).Admission
}

func legacyAdmissionContext(modelID string, operatorContext, maxOutputTokens, safetyMargin int) agent.ProviderAdmissionContext {
	spec := GlobalModelSpecRegistry().GetSpec(modelID)
	if operatorContext > 0 {
		spec.ContextWindow, spec.ContextWindowSource, spec.IsEstimated = operatorContext, "operator", false
	}
	if maxOutputTokens > 0 {
		spec.MaxOutputTokens = maxOutputTokens
	}
	if safetyMargin <= 0 {
		safetyMargin = spec.SafetyMarginTokens
	}
	return agent.ProviderAdmissionContext{
		ModelID: modelID, ContextWindow: spec.ContextWindow, MaxOutputTokens: spec.MaxOutputTokens,
		SafetyMarginTokens: safetyMargin, ContextWindowSource: spec.ContextWindowSource, IsEstimated: spec.IsEstimated,
	}
}

// WarmModels resolves the complete startup model set. This does not preload
// models; Ollama /api/ps only reports currently loaded state.
func (r *ModelProfileRuntime) WarmModels(ctx context.Context, modelIDs []string, operatorContext int) {
	seen := make(map[string]struct{}, len(modelIDs))
	var wg sync.WaitGroup
	for _, modelID := range modelIDs {
		modelID = strings.TrimSpace(modelID)
		key := strings.ToLower(modelID)
		if modelID == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		modelID := modelID
		wg.Go(func() {
			_, _ = r.Profile(ctx, modelID, operatorContext, 0)
		})
	}
	wg.Wait()
}

func (r *ModelProfileRuntime) ObserveOverflow(ctx context.Context, providerModel string, window int) error {
	if r == nil || r.manager == nil || r.resolver == nil || window <= 0 {
		return nil
	}
	ref, err := r.manager.ResolveProviderRef(providerModel)
	if err != nil {
		return err
	}
	ref.NoNet = r.noNet
	return r.resolver.ObserveContextWithContext(ctx, ref, providerModel, window)
}

// InvalidateProviders clears runtime evidence for every effective provider
// after a successful provider execution boundary is established.
func (r *ModelProfileRuntime) InvalidateProviders(refs []providerintrospection.ProviderRef) {
	if r == nil || r.resolver == nil {
		return
	}
	for _, ref := range refs {
		_ = r.resolver.InvalidateProvider(ref)
	}
}

func (c *Coordinator) admissionContextFor(ctx context.Context, modelID string, def *agent.AgentDef) agent.ProviderAdmissionContext {
	return c.admissionContextForWithOutput(ctx, modelID, def, 0)
}

func (c *Coordinator) admissionContextForWithOutput(ctx context.Context, modelID string, def *agent.AgentDef, outputReservation int) agent.ProviderAdmissionContext {
	if outputReservation <= 0 {
		if bound, ok := providerBoundInvocationContextFromContext(ctx, modelID); ok {
			return bound.AdmissionContext
		}
	}
	operatorContext := 0
	maxOutput := outputReservation
	margin := 0
	if def != nil {
		operatorContext = def.Generation.ContextWindow
		if maxOutput <= 0 {
			maxOutput = c.resolveAgentMaxOutputTokens(def)
		}
	}
	if operatorContext <= 0 && c != nil && c.session != nil {
		operatorContext = c.session.Config.Generation.ContextWindow
	}
	if maxOutput <= 0 && c != nil {
		// Auxiliary definitions are intentionally synthetic and have no agent
		// frontmatter. They still use the same team-level generation settings
		// as the concrete request created below.
		maxOutput = c.resolveAgentMaxOutputTokens(nil)
	}
	if c != nil && c.modelProfileRuntime != nil {
		return c.modelProfileRuntime.AdmissionContext(ctx, modelID, operatorContext, maxOutput, margin)
	}
	return legacyAdmissionContext(modelID, operatorContext, maxOutput, margin)
}

// resolveProviderBoundInvocationContext is the one production-path binding
// step for capacity-dependent invocation work. It resolves the selected
// model's profile once, fails closed before compilation when capacity is
// unavailable, and stores one immutable projection for all downstream
// consumers of this invocation.
func (c *Coordinator) resolveProviderBoundInvocationContext(ctx context.Context, modelID string, def *agent.AgentDef) (context.Context, providerBoundInvocationContext, error) {
	return c.resolveProviderBoundInvocationContextWithOutput(ctx, modelID, def, 0)
}

func (c *Coordinator) resolveProviderBoundInvocationContextWithOutput(ctx context.Context, modelID string, def *agent.AgentDef, outputReservation int) (context.Context, providerBoundInvocationContext, error) {
	if strings.TrimSpace(modelID) == "" {
		return ctx, providerBoundInvocationContext{}, nil
	}
	if outputReservation <= 0 {
		if bound, ok := providerBoundInvocationContextFromContext(ctx, modelID); ok {
			return ctx, bound, nil
		}
	}
	if c == nil || c.modelProfileRuntime == nil {
		return ctx, providerBoundInvocationContext{}, nil
	}
	operatorContext := 0
	maxOutput := outputReservation
	if def != nil {
		operatorContext = def.Generation.ContextWindow
		if maxOutput <= 0 {
			maxOutput = c.resolveAgentMaxOutputTokens(def)
		}
	}
	if operatorContext <= 0 && c.session != nil {
		operatorContext = c.session.Config.Generation.ContextWindow
	}
	if maxOutput <= 0 {
		maxOutput = c.resolveAgentMaxOutputTokens(nil)
	}
	resolved := c.modelProfileRuntime.ResolveAdmission(ctx, modelID, operatorContext, maxOutput, 0)
	bound := resolved.Admission
	if bound.IsBound() {
		c.reportModelProfileResolved(resolved.ProfileResolution)
	}
	if !bound.IsBound() {
		return ctx, providerBoundInvocationContext{}, fmt.Errorf("provider-bound context unavailable for model %q", modelID)
	}
	if bound.ContextWindow <= 0 {
		return ctx, providerBoundInvocationContext{}, &ContextWindowMetadataUnavailableError{ModelID: modelID}
	}
	invocation := providerBoundInvocationContext{
		ModelID:          modelID,
		AdmissionContext: bound,
		ModelContext:     modelContextSpecForProviderRequest(agent.ProviderRequest{ModelID: modelID, AdmissionContext: bound}),
	}
	return withProviderBoundInvocationContext(ctx, invocation), invocation, nil
}

func (c *Coordinator) modelContextSpecForInvocation(ctx context.Context, modelID string, def *agent.AgentDef) ModelContextSpec {
	if bound, ok := providerBoundInvocationContextFromContext(ctx, modelID); ok {
		return bound.ModelContext
	}
	return globalRegistry.GetSpec(modelID).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(def))
}
