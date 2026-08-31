package agent

import (
	"context"
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
