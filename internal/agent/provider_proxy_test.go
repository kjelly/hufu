package agent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestOllamaProviderListModelNamesProxyChild(t *testing.T) {
	if os.Getenv("HUFU_OLLAMA_PROVIDER_PROXY_TEST_CHILD") != "1" {
		return
	}
	os.Exit(providerproxy.RunChild(os.Stdin, os.Stdout))
}

func TestOllamaProviderListModelNamesUsesInvocationProxy(t *testing.T) {
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
