package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerproxy"
)

type fakeProviderBoundary struct {
	endpoint   string
	client     *http.Client
	abortCalls atomic.Int32
}

func (b *fakeProviderBoundary) URL() string              { return b.endpoint }
func (b *fakeProviderBoundary) HTTPClient() *http.Client { return b.client }
func (b *fakeProviderBoundary) Abort() error             { b.abortCalls.Add(1); return nil }
func (b *fakeProviderBoundary) Stop() error              { return b.Abort() }

type blockingProviderBoundary struct {
	*fakeProviderBoundary
	abortStarted chan struct{}
	abortRelease chan struct{}
	once         sync.Once
}

func (b *blockingProviderBoundary) Abort() error {
	b.once.Do(func() { close(b.abortStarted) })
	<-b.abortRelease
	return b.fakeProviderBoundary.Abort()
}

func TestProviderManagerFallsBackToOwnedInProcessBoundaryOnListenerPermission(t *testing.T) {
	for _, permission := range []string{"operation not permitted", "permission denied"} {
		t.Run(permission, func(t *testing.T) {
			pm, err := NewProviderManager("https://provider.example/v1", "secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			fallback := &fakeProviderBoundary{endpoint: "https://provider.example/v1", client: &http.Client{}}
			var processCalls, fallbackCalls atomic.Int32
			pm.startProcessBoundary = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
				processCalls.Add(1)
				return nil, fmt.Errorf("listen tcp 127.0.0.1:0: %s: %w", permission, providerproxy.ErrListenerUnavailable)
			}
			pm.startInProcessBoundary = func(context.Context, providerproxy.Config) (providerproxy.Boundary, error) {
				fallbackCalls.Add(1)
				return fallback, nil
			}

			if err := pm.StartInvocationProxy(context.Background(), "hufu"); err != nil {
				t.Fatalf("start invocation boundary: %v", err)
			}
			if processCalls.Load() != 1 || fallbackCalls.Load() != 1 {
				t.Fatalf("boundary starts = process:%d fallback:%d, want one each", processCalls.Load(), fallbackCalls.Load())
			}
			provider := pm.GetProvider("local/model")
			endpoint, client, active := provider.effectiveBaseURL()
			if !active || endpoint != fallback.endpoint || client != fallback.client {
				t.Fatalf("provider boundary = active:%t endpoint:%q client:%p, want owned fallback %q/%p", active, endpoint, client, fallback.endpoint, fallback.client)
			}
			if err := pm.AbortInvocationProxy(); err != nil {
				t.Fatalf("abort invocation boundary: %v", err)
			}
			if fallback.abortCalls.Load() != 1 {
				t.Fatalf("fallback abort calls = %d, want one", fallback.abortCalls.Load())
			}
			if err := pm.StopInvocationProxy(); err != nil {
				t.Fatalf("repeated stop: %v", err)
			}
			if fallback.abortCalls.Load() != 1 {
				t.Fatalf("fallback abort calls after repeated stop = %d, want one", fallback.abortCalls.Load())
			}
		})
	}
}

func TestProviderManagerFailsClosedForNonListenerProxyStartupError(t *testing.T) {
	pm, err := NewProviderManager("https://provider.example/v1", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fallbackCalls atomic.Int32
	pm.startProcessBoundary = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
		return nil, errors.New("provider proxy protocol failure")
	}
	pm.startInProcessBoundary = func(context.Context, providerproxy.Config) (providerproxy.Boundary, error) {
		fallbackCalls.Add(1)
		return nil, nil
	}
	if err := pm.StartInvocationProxy(context.Background(), "hufu"); err == nil {
		t.Fatal("non-listener proxy failure unexpectedly admitted provider boundary")
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls = %d, want zero for non-listener failure", fallbackCalls.Load())
	}
}

func TestProviderManagerSerializesConcurrentBoundaryOwnership(t *testing.T) {
	pm, err := NewProviderManager("https://provider.example/v1", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &blockingProviderBoundary{
		fakeProviderBoundary: &fakeProviderBoundary{endpoint: "https://provider.example/v1", client: &http.Client{}},
		abortStarted:         make(chan struct{}),
		abortRelease:         make(chan struct{}),
	}
	second := &fakeProviderBoundary{endpoint: "https://provider.example/v1", client: &http.Client{}}
	var starts atomic.Int32
	pm.startProcessBoundary = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
		if starts.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	pm.startInProcessBoundary = func(context.Context, providerproxy.Config) (providerproxy.Boundary, error) {
		t.Fatal("unexpected in-process fallback")
		return nil, nil
	}
	if err := pm.StartInvocationProxy(context.Background(), "hufu"); err != nil {
		t.Fatalf("start first invocation boundary: %v", err)
	}

	firstAbortDone := make(chan error, 1)
	go func() { firstAbortDone <- pm.AbortInvocationProxy() }()
	select {
	case <-first.abortStarted:
	case <-time.After(time.Second):
		t.Fatal("first boundary abort did not start")
	}

	secondStartDone := make(chan error, 1)
	go func() { secondStartDone <- pm.StartInvocationProxy(context.Background(), "hufu") }()
	select {
	case err := <-secondStartDone:
		t.Fatalf("second invocation admitted before first cleanup: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(first.abortRelease)
	if err := <-firstAbortDone; err != nil {
		t.Fatalf("abort first invocation boundary: %v", err)
	}
	if err := <-secondStartDone; err != nil {
		t.Fatalf("start second invocation boundary: %v", err)
	}
	if starts.Load() != 2 {
		t.Fatalf("process boundary starts = %d, want two serialized admissions", starts.Load())
	}
	if err := pm.StopInvocationProxy(); err != nil {
		t.Fatalf("stop second invocation boundary: %v", err)
	}
	if first.abortCalls.Load() != 1 || second.abortCalls.Load() != 1 {
		t.Fatalf("abort calls = first:%d second:%d, want one each", first.abortCalls.Load(), second.abortCalls.Load())
	}
}

func TestProviderManagerUsesCanonicalConfiguredLocalTargetForProxyAndIdentity(t *testing.T) {
	const (
		defaultURL = "https://default.example/v1"
		defaultKey = "default-key"
		localURL   = "http://localhost:11434/v1"
		localKey   = "local-key"
	)
	pm, err := NewProviderManager(defaultURL, defaultKey, map[string]config.ProviderConfig{
		"ollama": {ProviderURL: localURL, ProviderAPIKey: localKey},
	})
	if err != nil {
		t.Fatal(err)
	}

	var starts []providerproxy.Config
	boundary := &fakeProviderBoundary{endpoint: "http://127.0.0.1:43123", client: &http.Client{}}
	pm.startProcessBoundary = func(_ context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		starts = append(starts, cfg)
		return boundary, nil
	}
	if err := pm.StartInvocationProxy(t.Context(), "hufu"); err != nil {
		t.Fatalf("start invocation proxy: %v", err)
	}

	if len(starts) != 1 {
		t.Fatalf("local proxy starts = %d, want exactly one", len(starts))
	}
	if got := pm.configs["local"].ProviderURL; got != localURL {
		t.Fatalf("selected provider config URL = %q, want exact raw URL %q", got, localURL)
	}
	if starts[0].UpstreamURL != localURL || starts[0].APIKey != localKey {
		t.Fatalf("local proxy config = %#v, want upstream %q and key %q", starts[0], localURL, localKey)
	}

	localProvider := pm.GetProvider("local/model")
	ollamaProvider := pm.GetProvider("ollama/model")
	if localProvider != ollamaProvider {
		t.Fatal("local and ollama aliases resolved to different provider objects")
	}
	if localProvider.baseURL != localURL || localProvider.apiKey != localKey {
		t.Fatalf("provider target = %q/%q, want %q/%q", localProvider.baseURL, localProvider.apiKey, localURL, localKey)
	}
	endpoint, client, active := localProvider.effectiveBaseURL()
	if !active || endpoint != boundary.endpoint || client != boundary.client {
		t.Fatalf("provider boundary = active:%t endpoint:%q client:%p, want %q/%p", active, endpoint, client, boundary.endpoint, boundary.client)
	}

	for _, modelID := range []string{"local/model", "ollama/model"} {
		ref, refErr := pm.ResolveProviderRef(modelID)
		if refErr != nil {
			t.Fatalf("ResolveProviderRef(%q): %v", modelID, refErr)
		}
		if ref.Provider != "local" || ref.BaseURL != localURL {
			t.Fatalf("provider ref for %q = %#v, want canonical local target %q", modelID, ref, localURL)
		}
		identity, identityErr := modelprofile.Identity(ref, modelID)
		if identityErr != nil {
			t.Fatalf("model identity for %q: %v", modelID, identityErr)
		}
		if identity.BaseURL != "http://127.0.0.1:11434" || identity.Provider != "local" || identity.ModelID != "model" {
			t.Fatalf("model identity for %q = %#v, want local upstream identity", modelID, identity)
		}
	}

	if err := pm.AbortInvocationProxy(); err != nil {
		t.Fatalf("abort invocation proxy: %v", err)
	}
	if boundary.abortCalls.Load() != 1 {
		t.Fatalf("proxy abort calls = %d, want one", boundary.abortCalls.Load())
	}
	if _, _, active := localProvider.effectiveBaseURL(); active {
		t.Fatal("provider boundary remained attached after abort")
	}
}

func TestProviderManagerLocalConfigPrecedenceAndInheritance(t *testing.T) {
	const (
		defaultURL = "https://default.example/v1"
		defaultKey = "default-key"
	)
	tests := map[string]struct {
		configs map[string]config.ProviderConfig
		wantURL string
		wantKey string
	}{
		"explicit local takes precedence over ollama": {
			configs: map[string]config.ProviderConfig{
				"local":  {ProviderURL: "https://local.example/v1", ProviderAPIKey: "local-key"},
				"ollama": {ProviderURL: "https://alias.example/v1", ProviderAPIKey: "alias-key"},
			},
			wantURL: "https://local.example/v1",
			wantKey: "local-key",
		},
		"local URL inherits default key": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "https://local.example/v1"},
			},
			wantURL: "https://local.example/v1",
			wantKey: "default-key",
		},
		"local key inherits default URL": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderAPIKey: "local-key"},
			},
			wantURL: defaultURL,
			wantKey: "local-key",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			pm, err := NewProviderManager(defaultURL, defaultKey, test.configs)
			if err != nil {
				t.Fatal(err)
			}
			var starts []providerproxy.Config
			pm.startProcessBoundary = func(_ context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				starts = append(starts, cfg)
				return &fakeProviderBoundary{endpoint: "http://127.0.0.1:43124", client: &http.Client{}}, nil
			}
			if err := pm.StartInvocationProxy(t.Context(), "hufu"); err != nil {
				t.Fatalf("start invocation proxy: %v", err)
			}
			t.Cleanup(func() { _ = pm.AbortInvocationProxy() })
			if len(starts) == 0 || starts[0].UpstreamURL != test.wantURL || starts[0].APIKey != test.wantKey {
				t.Fatalf("local proxy config = %#v, want upstream %q and key %q", starts, test.wantURL, test.wantKey)
			}
			provider := pm.GetProvider("ollama/model")
			if provider.baseURL != test.wantURL || provider.apiKey != test.wantKey {
				t.Fatalf("provider target = %q/%q, want %q/%q", provider.baseURL, provider.apiKey, test.wantURL, test.wantKey)
			}
		})
	}
}

func TestNewProviderManagerRejectsConflictingSameTierProviderAliases(t *testing.T) {
	tests := map[string]map[string]config.ProviderConfig{
		"ollama aliases with different URLs": {
			"ollama": {ProviderURL: "https://first.example/v1", ProviderAPIKey: "same-key"},
			"Ollama": {ProviderURL: "https://second.example/v1", ProviderAPIKey: "same-key"},
		},
		"local aliases with different keys": {
			"local": {ProviderURL: "https://same.example/v1", ProviderAPIKey: "first-key"},
			"LOCAL": {ProviderURL: "https://same.example/v1", ProviderAPIKey: "second-key"},
		},
		"local aliases with different introspection types": {
			"local": {ProviderURL: "https://same.example/v1", ProviderAPIKey: "same-key", IntrospectionType: "ollama"},
			"LOCAL": {ProviderURL: "https://same.example/v1", ProviderAPIKey: "same-key", IntrospectionType: "openai-compatible"},
		},
	}

	for name, configs := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProviderManager("https://default.example/v1", "default-key", configs); err == nil {
				t.Fatal("NewProviderManager unexpectedly accepted conflicting same-tier aliases")
			}
		})
	}
}

func TestNewProviderManagerAcceptsEquivalentSameTierProviderAliases(t *testing.T) {
	tests := map[string]struct {
		configs map[string]config.ProviderConfig
		wantURL string
	}{
		"ollama aliases": {
			configs: map[string]config.ProviderConfig{
				"ollama": {ProviderURL: "http://localhost:11434/v1", ProviderAPIKey: "local-key"},
				"Ollama": {ProviderURL: "http://127.0.0.1:11434/v1", ProviderAPIKey: "local-key"},
			},
			wantURL: "http://localhost:11434/v1",
		},
		"local aliases": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "http://localhost:11434/v1", ProviderAPIKey: "local-key"},
				"LOCAL": {ProviderURL: "http://127.0.0.1:11434/v1", ProviderAPIKey: "local-key"},
			},
			wantURL: "http://localhost:11434/v1",
		},
		"local aliases with equivalent effective introspection defaults": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "http://localhost:11434/v1", ProviderAPIKey: "local-key"},
				"LOCAL": {
					ProviderURL:       "http://127.0.0.1:11434/v1",
					ProviderAPIKey:    "local-key",
					IntrospectionType: "Ollama",
				},
			},
			wantURL: "http://localhost:11434/v1",
		},
		"host case and trailing slash": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "https://LOCAL.example/v1/", ProviderAPIKey: "local-key"},
				"LOCAL": {ProviderURL: "https://local.example/v1", ProviderAPIKey: "local-key"},
			},
			wantURL: "https://LOCAL.example/v1/",
		},
		"v1 path and no v1 path": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "https://local.example/v1", ProviderAPIKey: "local-key"},
				"LOCAL": {ProviderURL: "https://local.example", ProviderAPIKey: "local-key"},
			},
			wantURL: "https://local.example/v1",
		},
		"custom base path trailing slash": {
			configs: map[string]config.ProviderConfig{
				"local": {ProviderURL: "https://local.example/custom/", ProviderAPIKey: "local-key"},
				"LOCAL": {ProviderURL: "https://local.example/custom", ProviderAPIKey: "local-key"},
			},
			wantURL: "https://local.example/custom/",
		},
		"aliases use lexical tie-break when exact key is absent": {
			configs: map[string]config.ProviderConfig{
				" LOCAL ": {ProviderURL: "http://localhost:11434/v1", ProviderAPIKey: "local-key"},
				"Local":   {ProviderURL: "http://127.0.0.1:11434/v1", ProviderAPIKey: "local-key"},
			},
			wantURL: "http://localhost:11434/v1",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for range 32 {
				pm, err := NewProviderManager("https://default.example/v1", "default-key", test.configs)
				if err != nil {
					t.Fatalf("NewProviderManager rejected equivalent aliases: %v", err)
				}
				provider := pm.DefaultProvider()
				if provider.apiKey != "local-key" {
					t.Fatalf("canonical local key = %q, want %q", provider.apiKey, "local-key")
				}
				if provider.baseURL != test.wantURL {
					t.Fatalf("selected invocation URL = %q, want exact raw URL %q", provider.baseURL, test.wantURL)
				}
			}
		})
	}
}

func TestConfiguredLocalProxyNormalizesOllamaModelOnTransport(t *testing.T) {
	const (
		defaultURL = "https://default.example/v1"
		localKey   = "local-key"
		modelID    = "ollama/model"
	)
	var requestURL string
	var requestModel string
	var requestAuthorization string
	var requestProxyVersion string
	server := newAgentIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURL = "http://" + r.Host + r.URL.String()
		requestAuthorization = r.Header.Get("Authorization")
		requestProxyVersion = r.Header.Get("X-Hufu-Provider-Proxy-Version")
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode provider request: %v", err)
		}
		requestModel = request.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"configured-local","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	localURL := server.URL + "/v1"

	pm, err := NewProviderManager(defaultURL, "default-key", map[string]config.ProviderConfig{
		"local": {ProviderURL: localURL, ProviderAPIKey: localKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	pm.startProcessBoundary = func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	if err := pm.StartInvocationProxy(t.Context(), "hufu"); err != nil {
		t.Fatalf("start invocation proxy: %v", err)
	}
	t.Cleanup(func() { _ = pm.AbortInvocationProxy() })

	provider := pm.DefaultProvider()
	if provider != pm.GetProvider(modelID) {
		t.Fatal("DefaultProvider and configured local provider are different objects")
	}
	languageModel, err := provider.LanguageModel(t.Context(), modelID)
	if err != nil {
		t.Fatalf("create language model: %v", err)
	}
	if _, err := languageModel.Generate(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")},
	}); err != nil {
		t.Fatalf("generate through configured local proxy: %v", err)
	}

	if requestURL != localURL+"/chat/completions" {
		t.Fatalf("request URL = %q, want configured upstream %q", requestURL, localURL+"/chat/completions")
	}
	if requestModel != "model" {
		t.Fatalf("wire model = %q, want basename without ollama qualifier", requestModel)
	}
	if requestAuthorization != "Bearer "+localKey {
		t.Fatalf("request authorization = %q, want %q", requestAuthorization, "Bearer "+localKey)
	}
	if requestProxyVersion != providerproxy.ProtocolVersion {
		t.Fatalf("request proxy version = %q, want %q", requestProxyVersion, providerproxy.ProtocolVersion)
	}
}

func TestProviderManagerPreservesDefaultAndNonLocalProviderTargets(t *testing.T) {
	const (
		defaultURL = "https://default.example/v1"
		defaultKey = "default-key"
		remoteURL  = "https://remote.example/v1"
	)
	pm, err := NewProviderManager(defaultURL, defaultKey, map[string]config.ProviderConfig{
		"remote": {ProviderURL: remoteURL},
	})
	if err != nil {
		t.Fatal(err)
	}

	var starts []providerproxy.Config
	var boundaries []*fakeProviderBoundary
	pm.startProcessBoundary = func(_ context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		starts = append(starts, cfg)
		boundary := &fakeProviderBoundary{
			endpoint: fmt.Sprintf("http://127.0.0.1:4312%d", len(starts)),
			client:   &http.Client{},
		}
		boundaries = append(boundaries, boundary)
		return boundary, nil
	}
	if err := pm.StartInvocationProxy(t.Context(), "hufu"); err != nil {
		t.Fatalf("start invocation proxy: %v", err)
	}
	if len(starts) != 2 {
		t.Fatalf("proxy starts = %d, want default and non-local targets", len(starts))
	}
	if starts[0].UpstreamURL != defaultURL || starts[0].APIKey != defaultKey {
		t.Fatalf("default proxy config = %#v, want %q/%q", starts[0], defaultURL, defaultKey)
	}
	if starts[1].UpstreamURL != remoteURL || starts[1].APIKey != defaultKey {
		t.Fatalf("non-local proxy config = %#v, want %q/%q", starts[1], remoteURL, defaultKey)
	}

	remoteProvider := pm.GetProvider("remote/model")
	if remoteProvider.baseURL != remoteURL || remoteProvider.apiKey != defaultKey {
		t.Fatalf("non-local provider target = %q/%q, want %q/%q", remoteProvider.baseURL, remoteProvider.apiKey, remoteURL, defaultKey)
	}
	if _, _, active := remoteProvider.effectiveBaseURL(); !active {
		t.Fatal("non-local provider did not receive its invocation boundary")
	}
	if err := pm.AbortInvocationProxy(); err != nil {
		t.Fatalf("abort invocation proxy: %v", err)
	}
	for i, boundary := range boundaries {
		if boundary.abortCalls.Load() != 1 {
			t.Fatalf("boundary %d abort calls = %d, want one", i, boundary.abortCalls.Load())
		}
	}
}

func TestOllamaProviderListModelNamesProxyChild(t *testing.T) {
	if os.Getenv("HUFU_OLLAMA_PROVIDER_PROXY_TEST_CHILD") != "1" {
		return
	}
	os.Exit(providerproxy.RunChild(os.Stdin, os.Stdout))
}

func TestListModelNamesUsesTimeoutWithInvocationProxy(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("environment disallows local sockets: %v", err)
		}
		t.Fatalf("listen upstream: %v", err)
	}

	type requestObservation struct {
		method        string
		path          string
		query         string
		authorization string
		proxyVersion  string
	}
	requests := make(chan requestObservation, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- requestObservation{
			method:        r.Method,
			path:          r.URL.Path,
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			proxyVersion:  r.Header.Get("X-Hufu-Provider-Proxy-Version"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	upstreamURL := "http://" + listener.Addr().String() + "/v1"
	provider, err := NewOllamaProvider(upstreamURL, "test-key", "ollama")
	if err != nil {
		t.Fatalf("create Ollama provider: %v", err)
	}

	assertModelListRequest := func(t *testing.T, wantProxy bool) {
		t.Helper()
		request := <-requests
		if request.method != http.MethodGet || request.path != "/v1/models" || request.query != "" {
			t.Fatalf("models request = method=%q path=%q query=%q", request.method, request.path, request.query)
		}
		if request.authorization != "Bearer test-key" {
			t.Fatalf("models authorization = %q, want %q", request.authorization, "Bearer test-key")
		}
		if wantProxy && request.proxyVersion != providerproxy.ProtocolVersion {
			t.Fatalf("models proxy version = %q, want %q", request.proxyVersion, providerproxy.ProtocolVersion)
		}
		if !wantProxy && request.proxyVersion != "" {
			t.Fatalf("direct models request unexpectedly used proxy version %q", request.proxyVersion)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if names, err := provider.ListModelNames(ctx); err != nil || len(names) != 1 || names[0] != "test-model" {
		t.Fatalf("direct ListModelNames() = %v, %v", names, err)
	}
	assertModelListRequest(t, false)

	proxy, err := providerproxy.StartWithCommand(
		ctx,
		os.Args[0],
		[]string{"-test.run=TestOllamaProviderListModelNamesProxyChild"},
		[]string{"HUFU_OLLAMA_PROVIDER_PROXY_TEST_CHILD=1"},
		providerproxy.Config{UpstreamURL: upstreamURL, APIKey: "test-key"},
	)
	if err != nil {
		t.Fatalf("start invocation proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	provider.setProxyURL(proxy.URL())

	if names, err := provider.ListModelNames(ctx); err != nil || len(names) != 1 || names[0] != "test-model" {
		t.Fatalf("proxied ListModelNames() = %v, %v", names, err)
	}
	assertModelListRequest(t, true)
}
