package modelprofile

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

type runtimeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *runtimeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *runtimeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

type splitRuntimeIntrospector struct {
	mu        sync.Mutex
	showCalls int
	psCalls   int
	show      providerintrospection.RuntimeModelInfo
	process   providerintrospection.RuntimeModelInfo
	showStart chan struct{}
	showBlock chan struct{}
	psStart   chan struct{}
	psBlock   chan struct{}
}

func (i *splitRuntimeIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return i.show, nil
}

func (i *splitRuntimeIntrospector) InspectShow(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	i.mu.Lock()
	i.showCalls++
	show := i.show
	start, block := i.showStart, i.showBlock
	i.mu.Unlock()
	if start != nil {
		select {
		case start <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return show, nil
}

func (i *splitRuntimeIntrospector) InspectProcess(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, bool, error) {
	i.mu.Lock()
	i.psCalls++
	process := i.process
	start, block := i.psStart, i.psBlock
	if block != nil {
		i.psBlock = nil
	}
	i.mu.Unlock()
	if start != nil {
		select {
		case start <- struct{}{}:
		default:
		}
	}
	if block != nil {
		<-block
	}
	return process, process.RuntimeContext > 0, nil
}

func (i *splitRuntimeIntrospector) counts() (int, int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.showCalls, i.psCalls
}

func testRuntimeProvider(key string) providerintrospection.ProviderRef {
	return providerintrospection.NewProviderRef("local", "ollama", "ollama", "http://127.0.0.1:11434/v1", key, false)
}

func testRuntimeRequest(provider providerintrospection.ProviderRef) RuntimeResolutionRequest {
	return RuntimeResolutionRequest{
		Provider: provider,
		ModelID:  "ollama/qwen3:8b",
		Profile:  ModelProfileInput{ModelID: "ollama/qwen3:8b", Provider: "ollama", Context: ContextResolutionInput{FallbackContext: 1_024}},
	}
}

func testRuntimeRequestForModel(provider providerintrospection.ProviderRef, modelID string) RuntimeResolutionRequest {
	request := testRuntimeRequest(provider)
	request.ModelID = modelID
	request.Profile.ModelID = modelID
	return request
}

func profileCacheSizes(cache *ProfileCache) (entries, observed, generation, active, flights, order int) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries), len(cache.observed), len(cache.generation), len(cache.active), len(cache.flights), len(cache.residentOrder)
}

func isolateProcessProfileCache(t *testing.T) {
	t.Helper()
	processProfileCache.Lock()
	previous := processProfileCache.cache
	processProfileCache.cache = nil
	processProfileCache.Unlock()
	t.Cleanup(func() {
		processProfileCache.Lock()
		processProfileCache.cache = previous
		processProfileCache.Unlock()
	})
}

func TestRuntimeCacheIdentitySeparatesProvidersAndOmitsAPIKey(t *testing.T) {
	first := testRuntimeProvider("first-secret")
	second := testRuntimeProvider("second-secret")
	one, err := Identity(first, "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	two, err := Identity(second, "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("API key changed cache identity: %#v != %#v", one, two)
	}
	otherProvider, err := Identity(providerintrospection.NewProviderRef("remote", "openai", "openai-compatible", first.BaseURL, "third-secret", false), "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if one == otherProvider {
		t.Fatalf("provider identity did not separate cache keys: %#v", one)
	}
}

func TestRuntimeResolverNoRuntimeSkipsProviderIntrospection(t *testing.T) {
	var factoryCalls int
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		factoryCalls++
		return &splitRuntimeIntrospector{}
	}, ProfileCacheOptions{})
	request := testRuntimeRequest(testRuntimeProvider("secret"))
	request.NoRuntime = true
	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 0 || profile.EffectiveContext != 1_024 {
		t.Fatalf("no-runtime resolution called provider or lost fallback: calls=%d profile=%#v", factoryCalls, profile)
	}
}

func TestRuntimeResolverNoRuntimeIgnoresWarmRuntimeAndObservedCache(t *testing.T) {
	const modelID = "qwen3:8b"
	catalog, err := modelcatalog.NewCatalog("test", []modelcatalog.CatalogModel{{
		Provider: "ollama",
		ID:       modelID,
		Family:   "catalog-family",
		Context:  65_536,
		Output:   2_048,
	}})
	if err != nil {
		t.Fatal(err)
	}
	introspector := &splitRuntimeIntrospector{
		show: providerintrospection.RuntimeModelInfo{
			Family:            "runtime-family-sentinel",
			ConfiguredContext: 77_777,
			ModelMaxContext:   88_888,
			MaxOutputTokens:   9_999,
			Capabilities:      []string{"tools"},
		},
		process: providerintrospection.RuntimeModelInfo{
			RuntimeContext: 66_666,
			Capabilities:   []string{"attachments"},
		},
	}
	resolver := NewRuntimeResolverWithCatalog(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{}, catalog)
	provider := testRuntimeProvider("secret")
	request := RuntimeResolutionRequest{
		Provider: provider,
		ModelID:  modelID,
		Profile: ModelProfileInput{
			ModelID:  modelID,
			Provider: "ollama",
			Context:  ContextResolutionInput{FallbackContext: 1_024},
		},
	}
	if _, err := resolver.Resolve(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ObserveContext(provider, modelID, 8_192); err != nil {
		t.Fatal(err)
	}
	normal, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 1 || psCalls != 2 {
		t.Fatalf("warm-cache calls show=%d ps=%d, want 1 and 2", showCalls, psCalls)
	}
	if normal.Family != "runtime-family-sentinel" || normal.RuntimeContext != 66_666 || normal.Sources.ObservedContext.Value != 8_192 {
		t.Fatalf("normal resolution did not retain runtime evidence: %#v", normal)
	}
	warmEntries := profileCacheState(resolver.cache)

	noRuntimeRequest := request
	noRuntimeRequest.NoRuntime = true
	noRuntime, err := resolver.Resolve(t.Context(), noRuntimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if gotShow, gotPS := introspector.counts(); gotShow != showCalls || gotPS != psCalls {
		t.Fatalf("no-runtime calls show=%d ps=%d, want unchanged at %d and %d", gotShow, gotPS, showCalls, psCalls)
	}
	if got := profileCacheState(resolver.cache); !reflect.DeepEqual(got, warmEntries) {
		t.Fatalf("no-runtime changed runtime cache: before=%#v after=%#v", warmEntries, got)
	}

	coldResolver := NewRuntimeResolverWithCatalog(nil, ProfileCacheOptions{}, catalog)
	cold, err := coldResolver.Resolve(t.Context(), noRuntimeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(noRuntime, cold) {
		t.Fatalf("no-runtime warm result differs from cold catalog/fallback result:\n warm=%#v\n cold=%#v", noRuntime, cold)
	}
	if noRuntime.Sources.RuntimeContext.Value != 0 || noRuntime.Sources.ObservedContext.Value != 0 ||
		noRuntime.Sources.RuntimeContext.Source != "" || noRuntime.Sources.ObservedContext.Source != "" {
		t.Fatalf("no-runtime profile retained runtime provenance: %#v", noRuntime.Sources)
	}
}

type profileCacheStateSnapshot struct {
	entries    int
	observed   int
	generation int
	active     int
	flights    int
	order      int
}

func profileCacheState(cache *ProfileCache) profileCacheStateSnapshot {
	entries, observed, generation, active, flights, order := profileCacheSizes(cache)
	return profileCacheStateSnapshot{entries: entries, observed: observed, generation: generation, active: active, flights: flights, order: order}
}

func TestRuntimeCacheIdentityCanonicalizesProviderQualifiedModels(t *testing.T) {
	local := providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", false)
	bare, err := Identity(local, "qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := Identity(local, "ollama/QWEN3:8B")
	if err != nil {
		t.Fatal(err)
	}
	if bare != qualified {
		t.Fatalf("local Ollama aliases differ: bare=%#v qualified=%#v", bare, qualified)
	}
	if bare.ModelID != "qwen3:8b" || bare.Type != "ollama" {
		t.Fatalf("canonical local identity = %#v", bare)
	}

	named := providerintrospection.NewProviderRef("gateway", "gateway", "openai-compatible", "https://gateway.example/v1", "", false)
	namedQualified, err := Identity(named, "gateway/org/QWEN3:8B")
	if err != nil {
		t.Fatal(err)
	}
	if namedQualified.ModelID != "org/qwen3:8b" {
		t.Fatalf("named provider qualifier was not removed: %#v", namedQualified)
	}
	other, err := Identity(providerintrospection.NewProviderRef("other", "other", "openai-compatible", named.BaseURL, "", false), "gateway/org/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if other.ModelID != "gateway/org/qwen3:8b" || other == namedQualified {
		t.Fatalf("non-matching provider qualifier was changed or providers merged: other=%#v named=%#v", other, namedQualified)
	}
	differentAdapter, err := Identity(providerintrospection.NewProviderRef("gateway", "gateway", "ollama", named.BaseURL, "", false), "gateway/org/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if differentAdapter == namedQualified {
		t.Fatalf("adapter type was omitted from cache identity: %#v", differentAdapter)
	}
}

func TestRuntimeCacheHasIndependentShowAndPSTTLs(t *testing.T) {
	clock := &runtimeClock{now: time.Unix(100, 0)}
	introspector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536, ModelMaxContext: 131_072},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector { return introspector }, ProfileCacheOptions{Now: clock.Now})
	request := testRuntimeRequest(testRuntimeProvider("secret"))
	if _, err := resolver.Resolve(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	clock.Advance(16 * time.Second)
	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 1 || psCalls != 2 {
		t.Fatalf("show calls=%d ps calls=%d, want 1 and 2", showCalls, psCalls)
	}
	if profile.EffectiveContext != 32_768 {
		t.Fatalf("effective context=%d, want runtime context", profile.EffectiveContext)
	}
}

func TestRuntimeCacheCapacityEvictsInactiveStateAndRefreshesIt(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{Capacity: 2})
	provider := testRuntimeProvider("secret")

	for _, modelID := range []string{"ollama/model-a", "ollama/model-b", "ollama/model-c"} {
		if _, err := resolver.Resolve(t.Context(), testRuntimeRequestForModel(provider, modelID)); err != nil {
			t.Fatal(err)
		}
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 3 || psCalls != 3 {
		t.Fatalf("initial refresh calls show=%d ps=%d, want three each", showCalls, psCalls)
	}
	entries, observed, generation, active, flights, order := profileCacheSizes(resolver.cache)
	if entries != 2 || observed != 2 || generation != 2 || active != 0 || flights != 0 || order != 2 {
		t.Fatalf("cache state sizes entries=%d observed=%d generation=%d active=%d flights=%d order=%d, want 2,2,2,0,0,2", entries, observed, generation, active, flights, order)
	}

	// A is the deterministic least-recently-used inactive victim after A, B,
	// C. Resolving it again must perform both runtime lookups afresh.
	if _, err := resolver.Resolve(t.Context(), testRuntimeRequestForModel(provider, "ollama/model-a")); err != nil {
		t.Fatal(err)
	}
	showCalls, psCalls = introspector.counts()
	if showCalls != 4 || psCalls != 4 {
		t.Fatalf("re-resolve refresh calls show=%d ps=%d, want four each", showCalls, psCalls)
	}
	entries, observed, generation, active, flights, order = profileCacheSizes(resolver.cache)
	if entries != 2 || observed != 2 || generation != 2 || active != 0 || flights != 0 || order != 2 {
		t.Fatalf("post-eviction cache state sizes entries=%d observed=%d generation=%d active=%d flights=%d order=%d, want 2,2,2,0,0,2", entries, observed, generation, active, flights, order)
	}
}

func TestRuntimeCacheKeepsSameModelIDsFromProvidersIsolated(t *testing.T) {
	firstProvider := providerintrospection.NewProviderRef("first", "first", "openai-compatible", "https://first.example/v1", "first-secret", false)
	secondProvider := providerintrospection.NewProviderRef("second", "second", "openai-compatible", "https://second.example/v1", "second-secret", false)
	firstIntrospector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ModelMaxContext: 65_536},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
	}
	secondIntrospector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ModelMaxContext: 131_072},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 64_000},
	}
	resolver := NewRuntimeResolver(func(provider providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		if provider.Provider == "first" {
			return firstIntrospector
		}
		return secondIntrospector
	}, ProfileCacheOptions{Capacity: 2})

	firstRequest := testRuntimeRequestForModel(firstProvider, "shared-model")
	firstRequest.Profile.Provider = firstProvider.Provider
	first, err := resolver.Resolve(t.Context(), firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := testRuntimeRequestForModel(secondProvider, "shared-model")
	secondRequest.Profile.Provider = secondProvider.Provider
	second, err := resolver.Resolve(t.Context(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if first.RuntimeContext != 32_768 || second.RuntimeContext != 64_000 {
		t.Fatalf("same model IDs shared runtime state: first=%d second=%d", first.RuntimeContext, second.RuntimeContext)
	}
	if first.ProviderContext != 65_536 || second.ProviderContext != 131_072 {
		t.Fatalf("same model IDs shared provider metadata: first=%d second=%d", first.ProviderContext, second.ProviderContext)
	}
}

func TestRuntimeCacheCoalescesSameKeyRefresh(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show:      providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		process:   providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
		showStart: make(chan struct{}, 1),
		showBlock: make(chan struct{}),
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector { return introspector }, ProfileCacheOptions{})
	request := testRuntimeRequest(testRuntimeProvider("secret"))
	request.ModelID = "qwen3:8b"
	request.Profile.ModelID = request.ModelID
	firstDone := make(chan struct{})
	go func() {
		_, _ = resolver.Resolve(context.Background(), request)
		close(firstDone)
	}()
	select {
	case <-introspector.showStart:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not start")
	}
	secondDone := make(chan struct{})
	go func() {
		qualified := request
		qualified.ModelID = "ollama/qwen3:8b"
		qualified.Profile.ModelID = qualified.ModelID
		_, _ = resolver.Resolve(context.Background(), qualified)
		close(secondDone)
	}()
	select {
	case <-secondDone:
		t.Fatal("duplicate refresh did not wait for same-key flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(introspector.showBlock)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first refresh did not finish")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("coalesced refresh did not finish")
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 1 || psCalls != 1 {
		t.Fatalf("show calls=%d ps calls=%d, want one each", showCalls, psCalls)
	}
}

func TestRuntimeCacheSaturationRespectsCancellationAndActiveIdentity(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show:      providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		showStart: make(chan struct{}, 1),
		showBlock: make(chan struct{}),
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{Capacity: 1})
	provider := testRuntimeProvider("secret")
	firstDone := make(chan error, 1)
	go func() {
		_, err := resolver.Resolve(context.Background(), testRuntimeRequestForModel(provider, "ollama/active"))
		firstDone <- err
	}()
	select {
	case <-introspector.showStart:
	case <-time.After(time.Second):
		t.Fatal("active refresh did not start")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := resolver.Resolve(ctx, testRuntimeRequestForModel(provider, "ollama/waiting"))
	if err == nil {
		t.Fatal("full cache resolution succeeded without a resident slot")
	}
	if ctx.Err() == nil {
		t.Fatalf("full cache resolution returned before cancellation: %v", err)
	}
	entries, observed, generation, active, flights, order := profileCacheSizes(resolver.cache)
	if entries != 1 || observed != 1 || generation != 1 || active != 1 || flights != 1 || order != 1 {
		t.Fatalf("saturated cache state sizes entries=%d observed=%d generation=%d active=%d flights=%d order=%d, want 1,1,1,1,1,1", entries, observed, generation, active, flights, order)
	}
	identity, err := Identity(provider, "ollama/active")
	if err != nil {
		t.Fatal(err)
	}
	key := runtimeCacheKey{identity}
	resolver.cache.mu.Lock()
	_, stillResident := resolver.cache.generation[key]
	resolver.cache.mu.Unlock()
	if !stillResident {
		t.Fatal("active identity was evicted while its refresh was blocked")
	}

	close(introspector.showBlock)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active refresh did not finish")
	}
}

func TestRuntimeCacheObservationSaturationRespectsCancellation(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		showStart: make(chan struct{}, 1),
		showBlock: make(chan struct{}),
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{Capacity: 1})
	provider := testRuntimeProvider("secret")
	activeDone := make(chan error, 1)
	go func() {
		_, err := resolver.Resolve(context.Background(), testRuntimeRequestForModel(provider, "ollama/active"))
		activeDone <- err
	}()
	select {
	case <-introspector.showStart:
	case <-time.After(time.Second):
		t.Fatal("active refresh did not start")
	}

	ctx, cancel := context.WithCancel(t.Context())
	observeDone := make(chan error, 1)
	go func() {
		observeDone <- resolver.ObserveContextWithContext(ctx, provider, "ollama/observed", 8_192)
	}()
	select {
	case err := <-observeDone:
		t.Fatalf("saturated observation returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	var err error
	select {
	case err = <-observeDone:
	case <-time.After(time.Second):
		t.Fatal("saturated observation did not honor cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("observation error=%v, want context cancellation", err)
	}
	entries, observed, generation, active, flights, order := profileCacheSizes(resolver.cache)
	if entries != 1 || observed != 1 || generation != 1 || active != 1 || flights != 1 || order != 1 {
		t.Fatalf("saturated observation changed cache state: entries=%d observed=%d generation=%d active=%d flights=%d order=%d", entries, observed, generation, active, flights, order)
	}

	close(introspector.showBlock)
	select {
	case err := <-activeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("active refresh did not finish")
	}
}

func TestRuntimeCacheResidencyTokenChangesAfterEvictionAndReadmission(t *testing.T) {
	cache := NewProfileCache(ProfileCacheOptions{Capacity: 1})
	provider := testRuntimeProvider("secret")
	firstIdentity, err := Identity(provider, "ollama/first")
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := Identity(provider, "ollama/second")
	if err != nil {
		t.Fatal(err)
	}
	firstKey := runtimeCacheKey{firstIdentity}
	secondKey := runtimeCacheKey{secondIdentity}

	_, _, _, oldResidency, err := cache.snapshotContext(t.Context(), firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := cache.snapshotContext(t.Context(), secondKey); err != nil {
		t.Fatal(err)
	}
	_, _, _, newResidency, err := cache.snapshotContext(t.Context(), firstKey)
	if err != nil {
		t.Fatal(err)
	}
	if oldResidency == newResidency {
		t.Fatal("eviction and readmission reused the old residency token")
	}
}

func TestProcessRuntimeResolverOptionsConfigureSharedCacheCapacity(t *testing.T) {
	isolateProcessProfileCache(t)
	resolver := NewProcessRuntimeResolverWithOptions(nil, ProfileCacheOptions{Capacity: 2})
	shared := NewProcessRuntimeResolverWithOptions(nil, ProfileCacheOptions{Capacity: 1})
	if resolver.cache != shared.cache {
		t.Fatal("process runtime resolvers do not share one cache")
	}
	if resolver.cache.capacity != 2 {
		t.Fatalf("shared cache capacity=%d, want 2", resolver.cache.capacity)
	}
	provider := testRuntimeProvider("secret")
	for _, modelID := range []string{"ollama/first", "ollama/second", "ollama/third"} {
		if _, err := resolver.Resolve(t.Context(), testRuntimeRequestForModel(provider, modelID)); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, _, _, _, order := profileCacheSizes(resolver.cache)
	if entries != 2 || order != 2 {
		t.Fatalf("shared cache retained entries=%d order=%d, want 2 and 2", entries, order)
	}
}

func TestRuntimeCacheInvalidationDuringFlightRejectsStaleCommit(t *testing.T) {
	cache := NewProfileCache(ProfileCacheOptions{Capacity: 1})
	provider := testRuntimeProvider("secret")
	identity, err := Identity(provider, "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	key := runtimeCacheKey{identity}
	oldBlock := make(chan struct{})
	oldStarted := make(chan struct{}, 1)
	oldContext, cancelOld := context.WithCancel(context.Background())
	oldDone := make(chan refreshResult, 1)
	go func() {
		oldDone <- cache.refresh(oldContext, key, "ps", func() (refreshResult, error) {
			oldStarted <- struct{}{}
			<-oldBlock
			return refreshResult{info: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768}, found: true}, nil
		})
	}()
	select {
	case <-oldStarted:
	case <-time.After(time.Second):
		t.Fatal("old refresh did not start")
	}

	cache.Invalidate(identity)
	fresh := cache.refresh(context.Background(), key, "ps", func() (refreshResult, error) {
		return refreshResult{info: providerintrospection.RuntimeModelInfo{RuntimeContext: 16_384}, found: true}, nil
	})
	if fresh.err != nil {
		t.Fatal(fresh.err)
	}
	if fresh.info.RuntimeContext != 16_384 {
		t.Fatalf("fresh refresh context=%d, want 16384", fresh.info.RuntimeContext)
	}

	cancelOld()
	close(oldBlock)
	select {
	case result := <-oldDone:
		if result.err == nil {
			t.Fatal("stale refresh was accepted after invalidation")
		}
	case <-time.After(time.Second):
		t.Fatal("stale refresh did not finish")
	}
	cache.mu.Lock()
	entry := cache.entries[key]
	cache.mu.Unlock()
	if entry.process.RuntimeContext != 16_384 {
		t.Fatalf("stale refresh overwrote fresh context=%d, want 16384", entry.process.RuntimeContext)
	}
}

func TestRuntimeCacheProviderInvalidationClearsAllRuntimeEvidence(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536, ModelMaxContext: 131_072},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{})
	provider := testRuntimeProvider("secret")
	request := testRuntimeRequest(provider)
	request.Profile.Context.CatalogContext = 131_072
	if _, err := resolver.Resolve(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if err := resolver.ObserveContext(provider, request.ModelID, 8_192); err != nil {
		t.Fatal(err)
	}
	if err := resolver.InvalidateProvider(provider); err != nil {
		t.Fatal(err)
	}
	introspector.mu.Lock()
	introspector.show = providerintrospection.RuntimeModelInfo{ConfiguredContext: 32_768, ModelMaxContext: 65_536}
	introspector.process = providerintrospection.RuntimeModelInfo{RuntimeContext: 16_384}
	introspector.mu.Unlock()
	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 2 || psCalls != 2 {
		t.Fatalf("provider invalidation calls show=%d ps=%d, want 2 each", showCalls, psCalls)
	}
	if profile.EffectiveContext != 16_384 || profile.Sources.CatalogContext.Value != 131_072 {
		t.Fatalf("runtime refresh did not preserve catalog or update runtime: %#v", profile)
	}
}

func TestRuntimeCacheProviderInvalidationRejectsStaleRefresh(t *testing.T) {
	cache := NewProfileCache(ProfileCacheOptions{Capacity: 1})
	provider := testRuntimeProvider("secret")
	identity, err := Identity(provider, "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	key := runtimeCacheKey{identity}
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	done := make(chan refreshResult, 1)
	oldContext, cancelOld := context.WithCancel(context.Background())
	go func() {
		done <- cache.refresh(oldContext, key, "show", func() (refreshResult, error) {
			started <- struct{}{}
			<-block
			return refreshResult{info: providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536}}, nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale show refresh did not start")
	}
	cache.InvalidateProvider(identity)
	fresh := cache.refresh(context.Background(), key, "show", func() (refreshResult, error) {
		return refreshResult{info: providerintrospection.RuntimeModelInfo{ConfiguredContext: 32_768}}, nil
	})
	if fresh.err != nil || fresh.info.ConfiguredContext != 32_768 {
		t.Fatalf("fresh provider refresh = %#v", fresh)
	}
	cancelOld()
	close(block)
	select {
	case result := <-done:
		if result.err == nil {
			t.Fatal("stale provider refresh was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("stale provider refresh did not finish")
	}
}

func TestRuntimeCacheEpochRejectsStaleOverflowRefresh(t *testing.T) {
	clock := &runtimeClock{now: time.Unix(200, 0)}
	oldProcess := providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768}
	introspector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		process: oldProcess,
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{Now: clock.Now})
	provider := testRuntimeProvider("secret")
	request := testRuntimeRequest(provider)
	if _, err := resolver.Resolve(t.Context(), request); err != nil {
		t.Fatal(err)
	}

	clock.Advance(16 * time.Second)
	staleBlock := make(chan struct{})
	introspector.mu.Lock()
	introspector.psStart = make(chan struct{}, 1)
	introspector.psBlock = staleBlock
	introspector.mu.Unlock()
	type resolution struct {
		profile ModelProfile
		err     error
	}
	oldDone := make(chan resolution, 1)
	go func() {
		profile, err := resolver.Resolve(t.Context(), request)
		oldDone <- resolution{profile: profile, err: err}
	}()
	select {
	case <-introspector.psStart:
	case <-time.After(time.Second):
		t.Fatal("stale process refresh did not start")
	}

	introspector.mu.Lock()
	introspector.process = providerintrospection.RuntimeModelInfo{RuntimeContext: 16_384}
	introspector.mu.Unlock()
	if err := resolver.ObserveContext(provider, request.ModelID, 8_192); err != nil {
		t.Fatal(err)
	}
	fresh, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.EffectiveContext != 16_384 {
		t.Fatalf("fresh runtime context=%d, want 16384", fresh.EffectiveContext)
	}
	close(staleBlock)
	oldResult := <-oldDone
	if oldResult.err != nil {
		t.Fatal(oldResult.err)
	}
	if oldResult.profile.EffectiveContext != 16_384 {
		t.Fatalf("in-flight runtime context=%d, want 16384", oldResult.profile.EffectiveContext)
	}
	if got := oldResult.profile.Sources.ObservedContext.Value; got != 8_192 {
		t.Fatalf("in-flight observed context=%d, want 8192", got)
	}
	afterStale, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if afterStale.EffectiveContext != 16_384 {
		t.Fatalf("stale refresh overwrote fresh runtime context=%d, want 16384", afterStale.EffectiveContext)
	}
	if got := afterStale.Sources.ObservedContext.Value; got != 8_192 {
		t.Fatalf("observed context=%d, want 8192", got)
	}
}

func TestProcessRuntimeResolversShareCacheAndObservedContext(t *testing.T) {
	isolateProcessProfileCache(t)
	introspector := &splitRuntimeIntrospector{
		show:      providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		process:   providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
		showStart: make(chan struct{}, 1),
		showBlock: make(chan struct{}),
	}
	factory := func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector { return introspector }
	first := NewProcessRuntimeResolver(factory)
	second := NewProcessRuntimeResolver(factory)
	firstProvider := testRuntimeProvider("first-secret")
	secondProvider := providerintrospection.NewProviderRef(
		"local", "ollama", "ollama", "HTTP://127.0.0.1:11434/v1/", "second-secret", false,
	)
	firstRequest := testRuntimeRequest(firstProvider)
	secondRequest := testRuntimeRequest(secondProvider)
	secondRequest.ModelID = "OLLAMA/QWEN3:8B"
	secondRequest.Profile.ModelID = secondRequest.ModelID

	type resolution struct {
		profile ModelProfile
		err     error
	}
	firstDone := make(chan resolution, 1)
	go func() {
		profile, err := first.Resolve(t.Context(), firstRequest)
		firstDone <- resolution{profile: profile, err: err}
	}()
	select {
	case <-introspector.showStart:
	case <-time.After(time.Second):
		t.Fatal("first process-scoped refresh did not start")
	}
	secondDone := make(chan resolution, 1)
	go func() {
		profile, err := second.Resolve(t.Context(), secondRequest)
		secondDone <- resolution{profile: profile, err: err}
	}()
	select {
	case <-secondDone:
		t.Fatal("second resolver did not join the shared refresh flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(introspector.showBlock)
	firstResult := <-firstDone
	secondResult := <-secondDone
	if firstResult.err != nil || secondResult.err != nil {
		t.Fatalf("shared process resolutions failed: first=%v second=%v", firstResult.err, secondResult.err)
	}
	showCalls, psCalls := introspector.counts()
	if showCalls != 1 || psCalls != 1 {
		t.Fatalf("shared process cache calls show=%d ps=%d, want one each", showCalls, psCalls)
	}
	if secondResult.profile.EffectiveContext != 32_768 {
		t.Fatalf("shared process profile effective context=%d, want 32768", secondResult.profile.EffectiveContext)
	}

	if err := first.ObserveContext(firstProvider, firstRequest.ModelID, 8_192); err != nil {
		t.Fatal(err)
	}
	profile, err := second.Resolve(t.Context(), secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Sources.ObservedContext.Value != 8_192 {
		t.Fatalf("shared observed context=%d, want 8192", profile.Sources.ObservedContext.Value)
	}
	if showCalls, psCalls = introspector.counts(); showCalls != 1 || psCalls != 2 {
		t.Fatalf("overflow refresh calls show=%d ps=%d, want 1 and 2", showCalls, psCalls)
	}
}

func TestRuntimeOperatorAndOverflowEvidencePrecedence(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show:    providerintrospection.RuntimeModelInfo{ConfiguredContext: 65_536},
		process: providerintrospection.RuntimeModelInfo{RuntimeContext: 32_768},
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector { return introspector }, ProfileCacheOptions{})
	request := testRuntimeRequest(testRuntimeProvider("secret"))
	request.Profile.Context.OperatorContext = 131_072
	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil || profile.EffectiveContext != 131_072 || profile.Sources.EffectiveContext.Source != SourceOperator {
		t.Fatalf("operator precedence profile=%#v err=%v", profile, err)
	}
	request.Profile.Context.OperatorContext = 0
	profile, err = resolver.Resolve(t.Context(), request)
	if err != nil || profile.EffectiveContext != 32_768 {
		t.Fatalf("runtime precedence profile=%#v err=%v", profile, err)
	}
	introspector.mu.Lock()
	introspector.process.RuntimeContext = 16_384
	introspector.mu.Unlock()
	resolver.ObserveContext(request.Provider, request.ModelID, 8_192)
	profile, err = resolver.Resolve(t.Context(), request)
	if err != nil || profile.EffectiveContext != 16_384 || profile.Sources.ObservedContext.Value != 8_192 {
		t.Fatalf("post-overflow profile=%#v err=%v", profile, err)
	}
	if showCalls, psCalls := introspector.counts(); showCalls != 1 || psCalls != 2 {
		t.Fatalf("overflow refresh calls show=%d ps=%d, want 1 and 2", showCalls, psCalls)
	}
}

func TestRuntimeResolverMaxOutputPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		source     MetadataSource
		want       int
		wantSource MetadataSource
	}{
		{name: "provider metadata overrides fallback", source: SourceFallback, want: 1_024, wantSource: SourceProviderMetadata},
		{name: "provider metadata overrides catalog", source: SourceCatalog, want: 1_024, wantSource: SourceProviderMetadata},
		{name: "operator overrides provider metadata", source: SourceOperator, want: 4_096, wantSource: SourceOperator},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			introspector := &splitRuntimeIntrospector{
				show: providerintrospection.RuntimeModelInfo{MaxOutputTokens: 1_024},
			}
			resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return introspector
			}, ProfileCacheOptions{})
			request := testRuntimeRequest(testRuntimeProvider("secret"))
			request.Profile.MaxOutputTokens = ResolvedValue[int]{Value: tt.want, Source: tt.source}

			profile, err := resolver.Resolve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if profile.MaxOutputTokens != tt.want || profile.Sources.MaxOutputTokens.Source != tt.wantSource {
				t.Fatalf("max output = %#v, want value=%d source=%q", profile.Sources.MaxOutputTokens, tt.want, tt.wantSource)
			}
		})
	}
}

func TestRuntimeResolverCapabilityProvenanceMatchesProviderAuthority(t *testing.T) {
	tests := []struct {
		name     string
		provider providerintrospection.ProviderRef
		profile  ModelProfileInput
		want     MetadataSource
	}{
		{
			name:     "ollama runtime",
			provider: testRuntimeProvider("runtime-secret"),
			profile:  ModelProfileInput{Provider: "ollama"},
			want:     SourceProviderRuntime,
		},
		{
			name: "provider metadata",
			provider: providerintrospection.NewProviderRef(
				"remote", "remote", "openai-compatible", "http://127.0.0.1:11434/v1", "metadata-secret", false,
			),
			profile: ModelProfileInput{Provider: "remote"},
			want:    SourceProviderMetadata,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			introspector := &splitRuntimeIntrospector{show: providerintrospection.RuntimeModelInfo{
				Capabilities:       []string{"tools"},
				CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityYes},
			}}
			if tt.want == SourceProviderRuntime {
				introspector.process = providerintrospection.RuntimeModelInfo{
					RuntimeContext:     32_768,
					Capabilities:       []string{"tools"},
					CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityYes},
				}
			}
			resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return introspector
			}, ProfileCacheOptions{})
			request := RuntimeResolutionRequest{Provider: tt.provider, ModelID: "model", Profile: tt.profile}
			profile, err := resolver.Resolve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if profile.SupportsTools != CapabilityYes || profile.Sources.Capabilities.Tools.Source != tt.want {
				t.Fatalf("tools profile = %#v, want yes from %q", profile.Sources.Capabilities.Tools, tt.want)
			}
		})
	}
}

func TestRuntimeResolverProcessCapabilityEvidenceOverridesShowAndFallback(t *testing.T) {
	introspector := &splitRuntimeIntrospector{
		show: providerintrospection.RuntimeModelInfo{
			CapabilityEvidence: map[string]providerintrospection.CapabilityState{
				"tools": providerintrospection.CapabilityUnknown,
			},
		},
		process: providerintrospection.RuntimeModelInfo{
			RuntimeContext: 32_768,
			Capabilities:   []string{"tools"},
			CapabilityEvidence: map[string]providerintrospection.CapabilityState{
				"tools": providerintrospection.CapabilityNo,
			},
		},
	}
	resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, ProfileCacheOptions{})
	request := testRuntimeRequest(testRuntimeProvider("secret"))
	request.Profile.Capabilities.Tools = CapabilityEvidence{
		Catalog:  CapabilityYes,
		Fallback: CapabilityYes,
	}

	profile, err := resolver.Resolve(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if profile.SupportsTools != CapabilityNo || profile.Sources.Capabilities.Tools.Source != SourceProviderRuntime {
		t.Fatalf("tools profile = %#v, want no from provider runtime", profile.Sources.Capabilities.Tools)
	}
}

func TestRuntimeResolverCapabilityPrecedenceAcrossShowAndProcess(t *testing.T) {
	tests := []struct {
		name          string
		show          providerintrospection.CapabilityState
		process       providerintrospection.CapabilityState
		processLoaded bool
		want          CapabilityState
		wantSource    MetadataSource
	}{
		{
			name:          "show yes and process no",
			show:          providerintrospection.CapabilityYes,
			process:       providerintrospection.CapabilityNo,
			processLoaded: true,
			want:          CapabilityNo,
			wantSource:    SourceProviderRuntime,
		},
		{
			name:          "show no and process yes",
			show:          providerintrospection.CapabilityNo,
			process:       providerintrospection.CapabilityYes,
			processLoaded: true,
			want:          CapabilityYes,
			wantSource:    SourceProviderRuntime,
		},
		{
			name:       "process absent leaves show eligible",
			show:       providerintrospection.CapabilityYes,
			want:       CapabilityYes,
			wantSource: SourceProviderMetadata,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			introspector := &splitRuntimeIntrospector{
				show: providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{
					"tools": tt.show,
				}},
			}
			if tt.processLoaded {
				introspector.process = providerintrospection.RuntimeModelInfo{
					RuntimeContext: 32_768,
					Capabilities:   []string{"tools"},
					CapabilityEvidence: map[string]providerintrospection.CapabilityState{
						"tools": tt.process,
					},
				}
			}
			resolver := NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return introspector
			}, ProfileCacheOptions{})
			request := testRuntimeRequest(testRuntimeProvider("secret"))
			request.Profile.Capabilities.Tools = CapabilityEvidence{Catalog: CapabilityYes, Fallback: CapabilityYes}

			profile, err := resolver.Resolve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if profile.SupportsTools != tt.want || profile.Sources.Capabilities.Tools.Source != tt.wantSource {
				t.Fatalf("tools profile = %#v, want value=%q source=%q", profile.Sources.Capabilities.Tools, tt.want, tt.wantSource)
			}
		})
	}
}
