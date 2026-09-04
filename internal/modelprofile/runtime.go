package modelprofile

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

const (
	defaultShowTTL              = 10 * time.Minute
	defaultPSTTL                = 15 * time.Second
	DefaultProfileCacheCapacity = 256
)

// DefaultProfileCacheCapacity is the number of provider/model identities a
// process-shared profile cache retains when no capacity is configured.

// CacheIdentity is the secret-free identity of one provider/model runtime.
// API keys are deliberately not part of this type.
type CacheIdentity struct {
	Provider string
	Type     string
	BaseURL  string
	ModelID  string
}

type runtimeCacheKey struct {
	CacheIdentity
}

type runtimeCacheEntry struct {
	show      providerintrospection.RuntimeModelInfo
	showAt    time.Time
	showOK    bool
	process   providerintrospection.RuntimeModelInfo
	psAt      time.Time
	psOK      bool
	psFetched bool
}

type refreshResult struct {
	info  providerintrospection.RuntimeModelInfo
	found bool
	err   error
}

type refreshFlight struct {
	done       chan struct{}
	result     refreshResult
	generation uint64
	residency  *cacheResidency
}

// cacheResidency identifies one admission of an identity. Unlike generation,
// it is never reused after eviction, so a re-admitted identity cannot satisfy
// an old flight or resolver snapshot through an ABA match.
type cacheResidency struct {
	id uint64
}

type refreshFlights struct {
	show *refreshFlight
	ps   *refreshFlight
}

func (f *refreshFlights) get(kind string) *refreshFlight {
	if f == nil {
		return nil
	}
	switch kind {
	case "show":
		return f.show
	case "ps":
		return f.ps
	default:
		return nil
	}
}

func (f *refreshFlights) set(kind string, flight *refreshFlight) {
	switch kind {
	case "show":
		f.show = flight
	case "ps":
		f.ps = flight
	}
}

func (f *refreshFlights) empty() bool {
	return f.show == nil && f.ps == nil
}

// ProfileCache stores independent show and ps observations. It never holds a
// cache lock while the introspector performs HTTP and coalesces concurrent
// refreshes for the same provider/model/source.
type ProfileCache struct {
	mu              sync.Mutex
	entries         map[runtimeCacheKey]runtimeCacheEntry
	flights         map[runtimeCacheKey]*refreshFlights
	observed        map[runtimeCacheKey]int
	generation      map[runtimeCacheKey]uint64
	residency       map[runtimeCacheKey]*cacheResidency
	nextResidency   uint64
	active          map[runtimeCacheKey]int
	residentOrder   []runtimeCacheKey
	capacityChanged chan struct{}
	capacity        int
	now             func() time.Time
	showTTL         time.Duration
	psTTL           time.Duration
}

// ProfileCacheOptions controls the clock, capacity, and freshness windows.
// Capacity is the maximum number of provider/model identities retained by the
// cache. Non-positive capacities use DefaultProfileCacheCapacity. Zero TTLs
// use the safe defaults; a custom clock makes expiry deterministic in tests.
type ProfileCacheOptions struct {
	Now      func() time.Time
	Capacity int
	ShowTTL  time.Duration
	PSTTL    time.Duration
}

func NewProfileCache(options ProfileCacheOptions) *ProfileCache {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	showTTL := options.ShowTTL
	if showTTL <= 0 {
		showTTL = defaultShowTTL
	}
	psTTL := options.PSTTL
	if psTTL <= 0 {
		psTTL = defaultPSTTL
	}
	capacity := options.Capacity
	if capacity <= 0 {
		capacity = DefaultProfileCacheCapacity
	}
	return &ProfileCache{
		entries:         make(map[runtimeCacheKey]runtimeCacheEntry),
		flights:         make(map[runtimeCacheKey]*refreshFlights),
		observed:        make(map[runtimeCacheKey]int),
		generation:      make(map[runtimeCacheKey]uint64),
		residency:       make(map[runtimeCacheKey]*cacheResidency),
		active:          make(map[runtimeCacheKey]int),
		capacityChanged: make(chan struct{}),
		capacity:        capacity,
		now:             now,
		showTTL:         showTTL,
		psTTL:           psTTL,
	}
}

var processProfileCache struct {
	sync.Mutex
	cache *ProfileCache
}

func sharedProfileCache(options ProfileCacheOptions) *ProfileCache {
	processProfileCache.Lock()
	defer processProfileCache.Unlock()
	if processProfileCache.cache == nil {
		processProfileCache.cache = NewProfileCache(options)
	}
	return processProfileCache.cache
}

// NewProcessRuntimeResolver returns a resolver backed by the process-wide
// runtime cache. Production coordinators use this boundary so independently
// constructed coordinators share provider observations and refresh flights.
func NewProcessRuntimeResolver(factory IntrospectorFactory) *RuntimeResolver {
	return NewProcessRuntimeResolverWithCatalog(factory, ProfileCacheOptions{}, nil)
}

// NewProcessRuntimeResolverWithOptions returns a resolver backed by the
// process-wide runtime cache, initializing that cache with options when it is
// first created. All process-scoped resolvers continue to share the same
// cache, including its configured capacity.
func NewProcessRuntimeResolverWithOptions(factory IntrospectorFactory, options ProfileCacheOptions) *RuntimeResolver {
	return NewProcessRuntimeResolverWithCatalog(factory, options, nil)
}

// NewProcessRuntimeResolverWithCatalog returns a process-cache resolver that
// also consults the supplied offline catalog.
func NewProcessRuntimeResolverWithCatalog(factory IntrospectorFactory, options ProfileCacheOptions, catalog modelcatalog.Reader) *RuntimeResolver {
	return newRuntimeResolver(factory, sharedProfileCache(options), catalog)
}

// Identity returns the normalized, secret-free cache identity for a provider
// reference and model ID.
func Identity(provider providerintrospection.ProviderRef, modelID string) (CacheIdentity, error) {
	baseURL, err := providerintrospection.NormalizeBaseURL(provider.BaseURL)
	if err != nil {
		return CacheIdentity{}, err
	}
	name := strings.ToLower(strings.TrimSpace(provider.Provider))
	if name == "" {
		name = strings.ToLower(strings.TrimSpace(provider.Name))
	}
	adapterType := strings.ToLower(strings.TrimSpace(provider.Type))
	return CacheIdentity{
		Provider: name,
		Type:     adapterType,
		BaseURL:  baseURL,
		ModelID:  canonicalModelID(provider, modelID),
	}, nil
}

func canonicalModelID(provider providerintrospection.ProviderRef, modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return ""
	}
	providerName := strings.ToLower(strings.TrimSpace(provider.Provider))
	if providerName == "" {
		providerName = strings.ToLower(strings.TrimSpace(provider.Name))
	}
	adapterType := strings.ToLower(strings.TrimSpace(provider.Type))
	qualifier, remainder, hasQualifier := strings.Cut(modelID, "/")
	if !hasQualifier {
		return modelID
	}
	qualifier = strings.ToLower(qualifier)
	if qualifier == providerName {
		return remainder
	}
	// The default local provider is addressed both as "local" and through
	// its historical Ollama qualifier. Strip only those provider qualifiers;
	// a namespace inside the model ID remains opaque.
	if providerName == "local" && (qualifier == "local" || (adapterType == "ollama" && qualifier == "ollama")) {
		return remainder
	}
	return modelID
}

func (c *ProfileCache) snapshotContext(ctx context.Context, key runtimeCacheKey) (runtimeCacheEntry, time.Time, uint64, *cacheResidency, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureResidentLocked(ctx, key); err != nil {
		return runtimeCacheEntry{}, time.Time{}, 0, nil, err
	}
	c.touchLocked(key)
	return c.entries[key], c.now(), c.generation[key], c.residency[key], nil
}

func (c *ProfileCache) refresh(ctx context.Context, key runtimeCacheKey, kind string, fetch func() (refreshResult, error)) refreshResult {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		c.mu.Lock()
		if err := c.ensureResidentLocked(ctx, key); err != nil {
			c.mu.Unlock()
			return refreshResult{err: err}
		}
		generation := c.generation[key]
		residency := c.residency[key]
		flightSet := c.flights[key]
		if waiting := flightSet.get(kind); waiting != nil {
			c.mu.Unlock()
			select {
			case <-waiting.done:
				c.mu.Lock()
				current := c.residency[key] == waiting.residency && c.generation[key] == waiting.generation
				result := waiting.result
				c.mu.Unlock()
				if current {
					return result
				}
				if err := ctx.Err(); err != nil {
					return refreshResult{err: err}
				}
			case <-ctx.Done():
				return refreshResult{err: ctx.Err()}
			}
			continue
		}
		waiting := &refreshFlight{done: make(chan struct{}), generation: generation, residency: residency}
		if flightSet == nil {
			flightSet = &refreshFlights{}
			c.flights[key] = flightSet
		}
		flightSet.set(kind, waiting)
		c.active[key]++
		c.touchLocked(key)
		c.mu.Unlock()

		result, err := fetch()
		if err != nil && result.err == nil {
			result.err = err
		}
		c.mu.Lock()
		current := c.residency[key] == waiting.residency && c.generation[key] == waiting.generation
		entry := c.entries[key]
		if result.err == nil && current {
			switch kind {
			case "show":
				entry.show, entry.showOK = result.info, true
				entry.showAt = c.now()
			case "ps":
				entry.process, entry.psOK, entry.psFetched = result.info, result.found, true
				// A successful missing-model observation is still fresh evidence.
				entry.psAt = c.now()
			}
			c.entries[key] = entry
		}
		if currentFlights := c.flights[key]; currentFlights == flightSet && currentFlights.get(kind) == waiting {
			currentFlights.set(kind, nil)
			if currentFlights.empty() {
				delete(c.flights, key)
			}
		}
		c.active[key]--
		if c.active[key] == 0 {
			delete(c.active, key)
			c.signalCapacityChangedLocked()
		}
		waiting.result = result
		close(waiting.done)
		c.mu.Unlock()
		if current {
			return result
		}
		if err := ctx.Err(); err != nil {
			return refreshResult{err: err}
		}
	}
}

func (c *ProfileCache) isResidentLocked(key runtimeCacheKey) bool {
	_, ok := c.generation[key]
	return ok
}

func (c *ProfileCache) ensureResidentLocked(ctx context.Context, key runtimeCacheKey) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.isResidentLocked(key) {
			return nil
		}
		if len(c.generation) < c.capacity {
			c.addResidentLocked(key)
			return nil
		}
		if victim, ok := c.inactiveVictimLocked(); ok {
			c.evictLocked(victim)
			c.addResidentLocked(key)
			return nil
		}
		wait := c.capacityChanged
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			c.mu.Lock()
			return ctx.Err()
		}
		c.mu.Lock()
	}
}

func (c *ProfileCache) addResidentLocked(key runtimeCacheKey) {
	c.entries[key] = runtimeCacheEntry{}
	c.observed[key] = 0
	c.generation[key] = 0
	c.nextResidency++
	c.residency[key] = &cacheResidency{id: c.nextResidency}
	c.residentOrder = append(c.residentOrder, key)
}

func (c *ProfileCache) inactiveVictimLocked() (runtimeCacheKey, bool) {
	for _, key := range c.residentOrder {
		if c.active[key] == 0 {
			return key, true
		}
	}
	return runtimeCacheKey{}, false
}

func (c *ProfileCache) evictLocked(key runtimeCacheKey) {
	delete(c.entries, key)
	delete(c.observed, key)
	delete(c.generation, key)
	delete(c.residency, key)
	for index, resident := range c.residentOrder {
		if resident == key {
			copy(c.residentOrder[index:], c.residentOrder[index+1:])
			c.residentOrder = c.residentOrder[:len(c.residentOrder)-1]
			break
		}
	}
}

func (c *ProfileCache) touchLocked(key runtimeCacheKey) {
	for index, resident := range c.residentOrder {
		if resident != key {
			continue
		}
		copy(c.residentOrder[index:], c.residentOrder[index+1:])
		c.residentOrder = c.residentOrder[:len(c.residentOrder)-1]
		break
	}
	c.residentOrder = append(c.residentOrder, key)
}

func (c *ProfileCache) signalCapacityChangedLocked() {
	close(c.capacityChanged)
	c.capacityChanged = make(chan struct{})
}

// Invalidate drops only runtime observations for one identity. Operator and
// configured evidence lives in the caller's input and cannot be erased by a
// provider overflow or a lower-authority refresh.
func (c *ProfileCache) Invalidate(identity CacheIdentity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	key := runtimeCacheKey{identity}
	if !c.isResidentLocked(key) {
		c.mu.Unlock()
		return
	}
	c.generation[key]++
	entry := c.entries[key]
	entry.process = providerintrospection.RuntimeModelInfo{}
	entry.psAt = time.Time{}
	entry.psOK = false
	entry.psFetched = false
	c.entries[key] = entry
	c.removeFlightsLocked(key)
	c.touchLocked(key)
	c.mu.Unlock()
}

func (c *ProfileCache) observeContext(ctx context.Context, identity CacheIdentity, window int) error {
	if c == nil || window <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	key := runtimeCacheKey{identity}
	if err := c.ensureResidentLocked(ctx, key); err != nil {
		c.mu.Unlock()
		return err
	}
	c.generation[key]++
	c.observed[key] = window
	entry := c.entries[key]
	entry.process = providerintrospection.RuntimeModelInfo{}
	entry.psAt = time.Time{}
	entry.psOK = false
	entry.psFetched = false
	c.entries[key] = entry
	c.removeFlightsLocked(key)
	c.touchLocked(key)
	c.mu.Unlock()
	return nil
}

func (c *ProfileCache) removeFlightsLocked(key runtimeCacheKey) {
	delete(c.flights, key)
}

// RuntimeResolutionRequest supplies the canonical non-runtime evidence for a
// single model. Runtime observations are merged into this input by Resolver.
type RuntimeResolutionRequest struct {
	Provider providerintrospection.ProviderRef
	ModelID  string
	Profile  ModelProfileInput
}

// IntrospectorFactory permits provider-specific introspection while keeping
// provider selection outside the profile resolver.
type IntrospectorFactory func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector

// RuntimeResolver merges cached provider evidence into ModelProfile. It is
// best-effort: an unavailable provider leaves configured/catalog/fallback
// evidence intact and never blocks a safe legacy fallback.
type RuntimeResolver struct {
	cache   *ProfileCache
	factory IntrospectorFactory
	catalog modelcatalog.Reader
}

func maxOutputSourceRank(source MetadataSource) int {
	switch source {
	case SourceOperator:
		return 6
	case SourceProviderRuntime, SourceModelConfig:
		return 5
	case SourceProviderMetadata:
		return 4
	case SourceObserved:
		return 3
	case SourceCatalog:
		return 2
	case SourceFallback:
		return 1
	default:
		return 0
	}
}

func mergeMaxOutputTokens(current ResolvedValue[int], providerMetadata int) ResolvedValue[int] {
	if providerMetadata <= 0 || maxOutputSourceRank(current.Source) >= maxOutputSourceRank(SourceProviderMetadata) {
		return current
	}
	return ResolvedValue[int]{Value: providerMetadata, Source: SourceProviderMetadata}
}

func NewRuntimeResolver(factory IntrospectorFactory, options ProfileCacheOptions) *RuntimeResolver {
	return NewRuntimeResolverWithCatalog(factory, options, nil)
}

// NewRuntimeResolverWithCatalog returns an isolated resolver with offline
// catalog evidence in addition to provider runtime evidence.
func NewRuntimeResolverWithCatalog(factory IntrospectorFactory, options ProfileCacheOptions, catalog modelcatalog.Reader) *RuntimeResolver {
	return newRuntimeResolver(factory, NewProfileCache(options), catalog)
}

func newRuntimeResolver(factory IntrospectorFactory, cache *ProfileCache, catalog modelcatalog.Reader) *RuntimeResolver {
	return &RuntimeResolver{
		cache: cache, factory: factory, catalog: catalog,
	}
}

func (r *RuntimeResolver) Resolve(ctx context.Context, request RuntimeResolutionRequest) (ModelProfile, error) {
	if r == nil || r.cache == nil {
		return ResolveModelProfile(request.Profile), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if request.ModelID == "" {
		request.ModelID = request.Profile.ModelID
	}
	identity, err := Identity(request.Provider, request.ModelID)
	if err != nil {
		return ResolveModelProfile(request.Profile), err
	}
	introspector := providerintrospection.ModelIntrospector(nil)
	if r.factory != nil {
		introspector = r.factory(request.Provider)
	}
	var show, process providerintrospection.RuntimeModelInfo
	var showOK, processOK bool
	resolveKey := runtimeCacheKey{identity}
	for {
		entry, now, generation, residency, err := r.cache.snapshotContext(ctx, resolveKey)
		if err != nil {
			return ResolveModelProfile(request.Profile), err
		}
		if entry.showOK && now.Sub(entry.showAt) < r.cache.showTTL {
			show, showOK = entry.show, true
		}
		if entry.psFetched && now.Sub(entry.psAt) < r.cache.psTTL {
			process, processOK = entry.process, true
		}
		if introspector != nil && !showOK {
			if split, ok := introspector.(providerintrospection.ModelShowIntrospector); ok {
				result := r.cache.refresh(ctx, runtimeCacheKey{identity}, "show", func() (refreshResult, error) {
					info, fetchErr := split.InspectShow(ctx, request.Provider, request.ModelID)
					return refreshResult{info: info}, fetchErr
				})
				if result.err == nil {
					show, showOK = result.info, true
				}
			} else {
				result := r.cache.refresh(ctx, runtimeCacheKey{identity}, "show", func() (refreshResult, error) {
					info, fetchErr := introspector.InspectModel(ctx, request.Provider, request.ModelID)
					return refreshResult{info: info}, fetchErr
				})
				if result.err == nil {
					show, showOK = result.info, true
					process, processOK = result.info, result.info.RuntimeContext > 0
				}
			}
		}
		if introspector != nil && !processOK {
			// The show flight may have been shared with another resolver that is
			// already refreshing ps. Re-read the cache after waiting for show so
			// this resolver joins that ps flight instead of issuing a duplicate.
			entry, now, _, _, err = r.cache.snapshotContext(ctx, resolveKey)
			if err != nil {
				return ResolveModelProfile(request.Profile), err
			}
			if entry.psFetched && now.Sub(entry.psAt) < r.cache.psTTL {
				process, processOK = entry.process, true
			}
		}
		if introspector != nil && !processOK {
			if split, ok := introspector.(providerintrospection.ModelProcessIntrospector); ok {
				result := r.cache.refresh(ctx, runtimeCacheKey{identity}, "ps", func() (refreshResult, error) {
					info, found, fetchErr := split.InspectProcess(ctx, request.Provider, request.ModelID)
					return refreshResult{info: info, found: found}, fetchErr
				})
				if result.err == nil {
					process, processOK = result.info, result.found
				}
			}
		}

		input := request.Profile
		input.ModelID = request.ModelID
		if input.Provider == "" {
			input.Provider = request.Provider.Provider
			if input.Provider == "" {
				input.Provider = request.Provider.Name
			}
		}
		if r.catalog != nil {
			applyCatalogEvidence(r.catalog, &input, request.Provider, request.ModelID)
		}
		if showOK {
			if show.Family != "" {
				input.Family = show.Family
				input.Estimator.ProviderRuntime = estimatorForFamily(show.Family)
				input.Estimator.ProviderRuntimeProvenance = "provider_family_derived"
			}
			if show.ConfiguredContext > 0 {
				input.Context.ConfiguredContext = show.ConfiguredContext
			}
			if show.ModelMaxContext > 0 {
				if strings.EqualFold(input.Provider, "ollama") || strings.EqualFold(request.Provider.Type, "ollama") {
					input.Context.ModelInfoContext = show.ModelMaxContext
				} else {
					input.Context.ProviderMetadataContext = show.ModelMaxContext
				}
			}
			input.MaxOutputTokens = mergeMaxOutputTokens(input.MaxOutputTokens, show.MaxOutputTokens)
			applyCapabilityEvidence(&input.Capabilities, show.Capabilities, false)
			applyCapabilityStates(&input.Capabilities, show.CapabilityEvidence, false)
		}
		if processOK && process.RuntimeContext > 0 {
			input.Context.RuntimeContext = process.RuntimeContext
		}
		if processOK {
			applyCapabilityEvidence(&input.Capabilities, process.Capabilities, true)
			applyCapabilityStates(&input.Capabilities, process.CapabilityEvidence, true)
		}
		r.cache.mu.Lock()
		if r.cache.generation[resolveKey] != generation || r.cache.residency[resolveKey] != residency {
			r.cache.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return ResolveModelProfile(request.Profile), err
			}
			show, process = providerintrospection.RuntimeModelInfo{}, providerintrospection.RuntimeModelInfo{}
			showOK, processOK = false, false
			continue
		}
		input.Context.ObservedContext = r.cache.observed[resolveKey]
		profile := ResolveModelProfile(input)
		r.cache.mu.Unlock()
		return profile, nil
	}
}

func catalogLookupIdentity(provider providerintrospection.ProviderRef, modelID string) (string, string) {
	providerName := strings.ToLower(strings.TrimSpace(provider.Provider))
	if providerName == "" {
		providerName = strings.ToLower(strings.TrimSpace(provider.Name))
	}
	adapterType := strings.ToLower(strings.TrimSpace(provider.Type))
	if adapterType == "ollama" {
		providerName = "ollama"
	}
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	qualifier, remainder, ok := strings.Cut(modelID, "/")
	if !ok {
		return providerName, modelID
	}
	qualifier = strings.ToLower(strings.TrimSpace(qualifier))
	boundName := strings.ToLower(strings.TrimSpace(provider.Name))
	strip := qualifier == providerName
	if adapterType == "ollama" && provider.Name == "local" {
		strip = strip || qualifier == "local" || qualifier == "ollama"
	}
	if boundName != "" && boundName != "local" {
		strip = strip || qualifier == boundName
	}
	if strip {
		return providerName, remainder
	}
	return providerName, modelID
}

func applyCatalogEvidence(catalog modelcatalog.Reader, input *ModelProfileInput, provider providerintrospection.ProviderRef, modelID string) {
	if catalog == nil || input == nil {
		return
	}
	catalogProvider, catalogModelID := catalogLookupIdentity(provider, modelID)
	result, found := catalog.Lookup(catalogProvider, catalogModelID)
	if !found {
		return
	}
	model := result.Model
	if input.Family == "" {
		input.Family = model.Family
	}
	if input.Estimator.Catalog == "" {
		input.Estimator.Catalog = model.Estimator
		input.Estimator.CatalogProvenance = model.EstimatorProvenance
	}
	if model.Context > 0 {
		input.Context.CatalogContext = model.Context
	}
	if model.Output > 0 && input.MaxOutputTokens.Value <= 0 {
		input.MaxOutputTokens = ResolvedValue[int]{Value: model.Output, Source: SourceCatalog}
	}
	applyCatalogCapability(&input.Capabilities.Tools, model.ToolCall)
	applyCatalogCapability(&input.Capabilities.Attachments, model.Attachment)
	applyCatalogCapability(&input.Capabilities.Reasoning, model.Reasoning)
	applyCatalogCapability(&input.Capabilities.Temperature, model.Temperature)
}

func applyCatalogCapability(target *CapabilityEvidence, value *bool) {
	if target == nil || value == nil || target.Catalog != "" {
		return
	}
	if *value {
		target.Catalog = CapabilityYes
	} else {
		target.Catalog = CapabilityNo
	}
}

// ObserveContext records a lower-authority overflow observation and forces a
// fresh ps lookup. It never mutates operator/provider evidence.
func (r *RuntimeResolver) ObserveContext(provider providerintrospection.ProviderRef, modelID string, window int) error {
	return r.ObserveContextWithContext(context.Background(), provider, modelID, window)
}

// ObserveContextWithContext records an overflow observation while preserving
// the caller's cancellation during bounded-cache admission.
func (r *RuntimeResolver) ObserveContextWithContext(ctx context.Context, provider providerintrospection.ProviderRef, modelID string, window int) error {
	if r == nil || window <= 0 {
		return nil
	}
	identity, err := Identity(provider, modelID)
	if err != nil {
		return err
	}
	return r.cache.observeContext(ctx, identity, window)
}

func applyCapabilityEvidence(input *CapabilityResolutionInput, capabilities []string, runtime bool) {
	for _, capability := range capabilities {
		canonical, ok := NormalizeCapabilityName(capability)
		if !ok {
			continue
		}
		switch canonical {
		case "tools":
			if runtime {
				input.Tools.Runtime = CapabilityYes
			} else {
				input.Tools.ProviderMetadata = CapabilityYes
			}
		case "attachments":
			if runtime {
				input.Attachments.Runtime = CapabilityYes
			} else {
				input.Attachments.ProviderMetadata = CapabilityYes
			}
		case "reasoning":
			if runtime {
				input.Reasoning.Runtime = CapabilityYes
			} else {
				input.Reasoning.ProviderMetadata = CapabilityYes
			}
		case "temperature":
			if runtime {
				input.Temperature.Runtime = CapabilityYes
			} else {
				input.Temperature.ProviderMetadata = CapabilityYes
			}
		}
	}
}

func applyCapabilityStates(input *CapabilityResolutionInput, evidence map[string]providerintrospection.CapabilityState, runtime bool) {
	for name, state := range evidence {
		canonical, ok := NormalizeCapabilityName(name)
		if !ok {
			continue
		}
		value := CapabilityUnknown
		switch state {
		case providerintrospection.CapabilityYes:
			value = CapabilityYes
		case providerintrospection.CapabilityNo:
			value = CapabilityNo
		}
		switch canonical {
		case "tools":
			if runtime {
				input.Tools.Runtime = value
			} else {
				input.Tools.ProviderMetadata = value
			}
		case "attachments":
			if runtime {
				input.Attachments.Runtime = value
			} else {
				input.Attachments.ProviderMetadata = value
			}
		case "reasoning":
			if runtime {
				input.Reasoning.Runtime = value
			} else {
				input.Reasoning.ProviderMetadata = value
			}
		case "temperature":
			if runtime {
				input.Temperature.Runtime = value
			} else {
				input.Temperature.ProviderMetadata = value
			}
		}
	}
}

// InvalidateProfile is a convenience for callers that already have a
// provider reference and need the next resolution to refresh runtime state.
func (r *RuntimeResolver) InvalidateProfile(provider providerintrospection.ProviderRef, modelID string) error {
	identity, err := Identity(provider, modelID)
	if err != nil {
		return err
	}
	r.cache.Invalidate(identity)
	return nil
}
