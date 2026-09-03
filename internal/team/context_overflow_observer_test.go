package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/sidecar"
)

type overflowObserverIntrospector struct {
	showCalls    int
	processCalls int
}

func (i *overflowObserverIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, _ string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{}, nil
}

func (i *overflowObserverIntrospector) InspectShow(_ context.Context, _ providerintrospection.ProviderRef, _ string) (providerintrospection.RuntimeModelInfo, error) {
	i.showCalls++
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 16_384}, nil
}

func (i *overflowObserverIntrospector) InspectProcess(_ context.Context, _ providerintrospection.ProviderRef, _ string) (providerintrospection.RuntimeModelInfo, bool, error) {
	i.processCalls++
	return providerintrospection.RuntimeModelInfo{RuntimeContext: 8_192}, true, nil
}

func newOverflowObserverCoordinator(t *testing.T, introspector *overflowObserverIntrospector) (*Coordinator, *modelprofile.RuntimeResolver) {
	t.Helper()
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver := modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, modelprofile.ProfileCacheOptions{})
	return &Coordinator{modelProfileRuntime: &ModelProfileRuntime{manager: manager, resolver: resolver}}, resolver
}

func resolveOverflowObserverProfile(t *testing.T, resolver *modelprofile.RuntimeResolver, modelID string) modelprofile.ModelProfile {
	t.Helper()
	provider := providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", false)
	profile, err := resolver.Resolve(t.Context(), modelprofile.RuntimeResolutionRequest{
		Provider: provider,
		ModelID:  modelID,
		Profile:  modelprofile.ModelProfileInput{ModelID: modelID, Context: modelprofile.ContextResolutionInput{FallbackContext: 128_000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestSidecarErrorObserverRecordsOnlyProviderOverflowCapacity(t *testing.T) {
	modelID := "observer-model"
	introspector := &overflowObserverIntrospector{}
	c, resolver := newOverflowObserverCoordinator(t, introspector)

	if err := c.observeSidecarError(t.Context(), modelID, errors.New("provider rejected request: context window exceeded at 4096 tokens")); err != nil {
		t.Fatal(err)
	}
	profile := resolveOverflowObserverProfile(t, resolver, modelID)
	if got := profile.Sources.ObservedContext.Value; got != 4_096 {
		t.Fatalf("observed context = %d, want 4096", got)
	}

	beforeProcess := introspector.processCalls
	if err := c.observeSidecarError(t.Context(), modelID, errors.New("provider rejected request because context window was exceeded")); err != nil {
		t.Fatal(err)
	}
	_ = resolveOverflowObserverProfile(t, resolver, modelID)
	if introspector.processCalls <= beforeProcess {
		t.Fatalf("unparseable provider error did not invalidate process profile: before=%d after=%d", beforeProcess, introspector.processCalls)
	}

	if err := c.observeSidecarError(t.Context(), modelID, &CannotFitError{ModelID: modelID, ProvenNoSend: true}); err != nil {
		t.Fatal(err)
	}
	profile = resolveOverflowObserverProfile(t, resolver, modelID)
	if got := profile.Sources.ObservedContext.Value; got != 4_096 {
		t.Fatalf("pre-provider CannotFit changed observed context to %d", got)
	}
}

type blockingOverflowIntrospector struct {
	showStarted chan struct{}
	releaseShow chan struct{}
}

func (i *blockingOverflowIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{}, nil
}

func (i *blockingOverflowIntrospector) InspectShow(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	select {
	case i.showStarted <- struct{}{}:
	default:
	}
	<-i.releaseShow
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 16_384}, nil
}

func (i *blockingOverflowIntrospector) InspectProcess(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, bool, error) {
	return providerintrospection.RuntimeModelInfo{RuntimeContext: 8_192}, true, nil
}

type cancelingOverflowAgent struct {
	cancel func(error)
	err    error
}

func (cancelingOverflowAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a cancelingOverflowAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.cancel(a.err)
	return nil, a.err
}

func newSaturatedOverflowCoordinator(t *testing.T) (*Coordinator, *modelprofile.RuntimeResolver, *blockingOverflowIntrospector, chan struct{}) {
	t.Helper()
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	introspector := &blockingOverflowIntrospector{
		showStarted: make(chan struct{}, 1),
		releaseShow: make(chan struct{}),
	}
	resolver := modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return introspector
	}, modelprofile.ProfileCacheOptions{Capacity: 1})
	c := &Coordinator{
		modelProfileRuntime: &ModelProfileRuntime{manager: manager, resolver: resolver},
		session:             &TeamSession{Workspace: t.TempDir()},
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
	}
	activeDone := make(chan struct{})
	go func() {
		_, _ = resolver.Resolve(context.Background(), modelprofile.RuntimeResolutionRequest{
			Provider: providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", false),
			ModelID:  "active-model",
			Profile:  modelprofile.ModelProfileInput{ModelID: "active-model", Context: modelprofile.ContextResolutionInput{FallbackContext: 128_000}},
		})
		close(activeDone)
	}()
	return c, resolver, introspector, activeDone
}

func TestWorkerOverflowObservationUsesCanceledInvocationContext(t *testing.T) {
	modelID := "overflow-worker-model"
	c, resolver, introspector, activeDone := newSaturatedOverflowCoordinator(t)
	select {
	case <-introspector.showStarted:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not start")
	}

	providerErr := fmt.Errorf("context window exceeded: context window 4096")
	ctx, cancel := context.WithCancelCause(t.Context())
	ctx = context.WithValue(ctx, modelKey{}, modelID)
	done := make(chan error, 1)
	go func() {
		_, _, err := c.runAgentWithStatusAndHistory(ctx, cancelingOverflowAgent{cancel: cancel, err: providerErr}, "worker", "prompt", nil, &taskTiming{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), providerErr.Error()) {
			t.Fatalf("worker error = %v, want original provider overflow", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("worker overflow handling waited on saturated cache after invocation cancellation")
	}

	close(introspector.releaseShow)
	select {
	case <-activeDone:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not finish")
	}

	// If the overflow observer had used Background, it would have admitted the
	// observed identity after the active refresh released the only slot. A
	// canceled invocation must leave that identity absent; the first explicit
	// resolution below therefore performs the normal fresh admission.
	profile, err := resolver.Resolve(t.Context(), modelprofile.RuntimeResolutionRequest{
		Provider: providerintrospection.NewProviderRef("local", "local", "ollama", "http://127.0.0.1:11434/v1", "", false),
		ModelID:  modelID,
		Profile:  modelprofile.ModelProfileInput{ModelID: modelID, Context: modelprofile.ContextResolutionInput{FallbackContext: 128_000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := profile.Sources.ObservedContext.Value; got != 0 {
		t.Fatalf("canceled overflow observation mutated observed context to %d", got)
	}
}

func TestSidecarOverflowObserverUsesCanceledInvocationContext(t *testing.T) {
	modelID := "overflow-sidecar-model"
	c, _, introspector, activeDone := newSaturatedOverflowCoordinator(t)
	select {
	case <-introspector.showStarted:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not start")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- c.observeSidecarError(ctx, modelID, errors.New("provider rejected request: context window exceeded at 4096 tokens"))
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("sidecar observation error = %v, want context cancellation", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("sidecar overflow observation waited on saturated cache after invocation cancellation")
	}
	close(introspector.releaseShow)
	select {
	case <-activeDone:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not finish")
	}
}

type cancelingSidecarGenerationAgent struct {
	cancel  context.CancelFunc
	err     error
	started chan struct{}
	once    sync.Once
}

func (a *cancelingSidecarGenerationAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.once.Do(func() { close(a.started) })
	a.cancel()
	return nil, a.err
}

func (*cancelingSidecarGenerationAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func TestAttachedSidecarGenerationUsesCanceledContextForSaturatedOverflowObservation(t *testing.T) {
	modelID := "overflow-integrated-sidecar-model"
	c, resolver, introspector, activeDone := newSaturatedOverflowCoordinator(t)

	released := false
	release := func() {
		if !released {
			close(introspector.releaseShow)
			released = true
		}
	}
	t.Cleanup(release)

	providerErr := errors.New("provider rejected request: context window exceeded at 4096 tokens")
	ctx, cancel := context.WithCancel(t.Context())
	generation := &cancelingSidecarGenerationAgent{
		cancel:  cancel,
		err:     providerErr,
		started: make(chan struct{}),
	}
	attached := c.attachSidecarUsageObserver(sidecar.NewSidecarForTest(modelID, generation))
	if attached == nil {
		t.Fatal("attachSidecarUsageObserver returned nil")
	}
	// Keep the real observer installed by attachSidecarUsageObserver while
	// supplying only the provider-bound context needed by this injected agent.
	attached.SetInvocationBinder(func(ctx context.Context, gotModelID string) (context.Context, agent.ProviderAdmissionContext, error) {
		return ctx, agent.ProviderAdmissionContext{
			ModelID:             gotModelID,
			ProviderIdentity:    "local",
			ProviderBaseURL:     "http://127.0.0.1:11434/v1",
			Bound:               true,
			ContextWindow:       128_000,
			MaxOutputTokens:     int(sidecar.ClassifierProfile.MaxOutputTokens),
			SafetyMarginTokens:  512,
			ContextWindowSource: "test",
		}, nil
	})
	attached.SetPromptPreparer(func(_ context.Context, _, prompt string) (string, error) {
		return prompt, nil
	})

	select {
	case <-introspector.showStarted:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not start")
	}

	done := make(chan error, 1)
	go func() {
		_, err := attached.Execute(ctx, "classify this failure")
		done <- err
	}()

	select {
	case <-generation.started:
	case <-time.After(time.Second):
		t.Fatal("injected sidecar generation did not start")
	}
	select {
	case err := <-done:
		if !errors.Is(err, providerErr) {
			t.Fatalf("sidecar generation error = %v, want original provider error %v", err, providerErr)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("attached sidecar generation waited on saturated cache after invocation cancellation")
	}

	release()
	select {
	case <-activeDone:
	case <-time.After(time.Second):
		t.Fatal("active profile refresh did not finish")
	}

	profile := resolveOverflowObserverProfile(t, resolver, modelID)
	if got := profile.Sources.ObservedContext.Value; got != 0 {
		t.Fatalf("canceled sidecar overflow observation mutated observed context to %d", got)
	}
}
