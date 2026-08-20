package providerproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProviderProxyChildHelper(t *testing.T) {
	if os.Getenv("HUFU_PROXY_TEST_CHILD") != "1" {
		return
	}
	os.Exit(RunChild(os.Stdin, os.Stdout))
}

func TestProxyForwardsAndCloseReapsBlockedChild(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var request struct {
		method string
		path   string
		query  string
		body   string
		auth   string
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("environment disallows local sockets: %v", err)
		}
		t.Fatalf("listen upstream: %v", err)
	}
	upstreamServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, fmt.Sprintf("read body: %v", readErr), http.StatusBadRequest)
			return
		}
		request.method = r.Method
		request.path = r.URL.Path
		request.query = r.URL.RawQuery
		request.body = string(body)
		request.auth = r.Header.Get("Authorization")
		startOnce.Do(func() { close(started) })
		<-release
	})}
	go func() { _ = upstreamServer.Serve(listener) }()
	upstreamURL := "http://" + listener.Addr().String() + "/v1"
	defer func() { close(release); _ = upstreamServer.Close() }()

	p, err := StartWithCommand(context.Background(), os.Args[0], []string{"-test.run=TestProviderProxyChildHelper"}, []string{"HUFU_PROXY_TEST_CHILD=1"}, Config{UpstreamURL: upstreamURL, APIKey: "secret-not-persisted"})
	if err != nil {
		t.Fatalf("start provider proxy: %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		req, requestErr := http.NewRequest(http.MethodPost, p.URL()+"/chat/completions?stream=false", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hello"}]}`))
		if requestErr != nil {
			requestDone <- requestErr
			return
		}
		req.Header.Set("Authorization", "Bearer caller")
		resp, requestErr := (&http.Client{}).Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-started:
	case requestErr := <-requestDone:
		t.Fatalf("proxy request ended before upstream received it: %v", requestErr)
	case <-time.After(time.Second):
		_ = p.Close()
		t.Fatal("proxy did not forward request")
	}
	if request.method != http.MethodPost || request.path != "/v1/chat/completions" || request.query != "stream=false" {
		t.Fatalf("upstream request routing = method=%q path=%q query=%q", request.method, request.path, request.query)
	}
	if request.body != `{"model":"test","messages":[{"role":"user","content":"hello"}]}` {
		t.Fatalf("upstream body = %q", request.body)
	}
	if request.auth != "Bearer caller" {
		t.Fatalf("upstream authorization = %q, want caller authorization", request.auth)
	}
	closeErr := p.Close()
	if closeErr != nil {
		t.Fatalf("close proxy: %v", closeErr)
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("blocked provider request remained after proxy process was reaped")
	}
	if err := p.Close(); err != nil {
		rest := err
		t.Fatalf("second close was not idempotent: %v", rest)
	}
}

func TestProxyRejectsRequestsWithoutCapability(t *testing.T) {
	upstreamReached := make(chan struct{}, 1)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("environment disallows local sockets: %v", err)
		}
		t.Fatalf("listen upstream: %v", err)
	}
	upstreamServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReached <- struct{}{}
		http.Error(w, "upstream should not be reached", http.StatusInternalServerError)
	})}
	go func() { _ = upstreamServer.Serve(listener) }()
	defer func() { _ = upstreamServer.Close() }()

	p, err := StartWithCommand(context.Background(), os.Args[0], []string{"-test.run=TestProviderProxyChildHelper"}, []string{"HUFU_PROXY_TEST_CHILD=1"}, Config{
		UpstreamURL: "http://" + listener.Addr().String() + "/v1",
		APIKey:      "secret-not-persisted",
	})
	if err != nil {
		t.Fatalf("start provider proxy: %v", err)
	}
	defer func() { _ = p.Close() }()

	proxyURL, err := url.Parse(p.URL())
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	proxyURL.Path = "/"
	resp, err := (&http.Client{}).Get(proxyURL.String())
	if err != nil {
		t.Fatalf("request unauthorized proxy path: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized proxy status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	select {
	case <-upstreamReached:
		t.Fatal("unauthorized proxy request reached upstream")
	case <-time.After(100 * time.Millisecond):
	}
}
