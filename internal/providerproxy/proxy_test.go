package providerproxy

import (
	"bufio"
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

func TestInProcessBoundaryAbortClosesBlockedRequest(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	requestReceived := make(chan struct{})
	base := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "http://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, boundary.URL()+"/chat/completions", strings.NewReader(`{"model":"test"}`))
		if err != nil {
			requestDone <- err
			return
		}
		resp, err := boundary.HTTPClient().Do(req)
		if resp != nil {
			_, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err == nil {
				err = readErr
			}
		}
		requestDone <- err
	}()
	go func() {
		_, _ = http.ReadRequest(bufio.NewReader(serverConn))
		_, _ = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nx")
		close(requestReceived)
	}()
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("in-process boundary did not send provider request")
	}

	abortDone := make(chan error, 1)
	go func() { abortDone <- boundary.Abort() }()
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("blocked provider request unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not unblock provider request")
	}
	if err := <-abortDone; err != nil {
		t.Fatalf("abort in-process boundary: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := boundary.Stop(); err != nil {
				t.Errorf("repeated concurrent stop: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestInProcessResponseBodyKeepsRequestOwnedAfterHeaders(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	base := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "http://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}

	serverDone := make(chan error, 1)
	go func() {
		_, err := http.ReadRequest(bufio.NewReader(serverConn))
		if err == nil {
			_, err = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 11\r\n\r\nhello world")
		}
		serverDone <- err
	}()

	req, err := http.NewRequest(http.MethodGet, boundary.URL()+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := boundary.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("provider request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != "hello world" {
		t.Fatalf("response body = %q, want complete body", body)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("provider response: %v", err)
	}
	if err := boundary.Abort(); err != nil {
		t.Fatalf("abort in-process boundary: %v", err)
	}
}

func TestInProcessAbortUnblocksResponseBodyReadAndWaitsForRequest(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	base := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return clientConn, nil
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "http://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}

	firstChunkWritten := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		_, err := http.ReadRequest(bufio.NewReader(serverConn))
		if err == nil {
			_, err = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n")
			close(firstChunkWritten)
		}
		serverDone <- err
	}()

	req, err := http.NewRequest(http.MethodGet, boundary.URL()+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := boundary.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("provider request: %v", err)
	}
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first response chunk: %v", err)
	}
	if string(first) != "x" {
		t.Fatalf("first response chunk = %q, want x", first)
	}
	select {
	case <-firstChunkWritten:
	case <-time.After(time.Second):
		t.Fatal("provider did not send first response chunk")
	}

	blockedRead := make(chan error, 1)
	go func() {
		_, readErr := resp.Body.Read(make([]byte, 1))
		blockedRead <- readErr
	}()
	abortDone := make(chan error, 1)
	go func() { abortDone <- boundary.Abort() }()

	select {
	case readErr := <-blockedRead:
		if readErr == nil {
			t.Fatal("blocked response read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not unblock response body read")
	}
	select {
	case abortErr := <-abortDone:
		if abortErr != nil {
			t.Fatalf("abort in-process boundary: %v", abortErr)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not wait for active response request")
	}
	if err := <-serverDone; err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("provider connection: %v", err)
	}
}

func TestInProcessBoundaryAbortCancelsBlockedDialTLSContext(t *testing.T) {
	dialStarted := make(chan struct{})
	base := &http.Transport{
		Proxy: nil,
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "https://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, boundary.URL()+"/models", nil)
		if err != nil {
			requestDone <- err
			return
		}
		_, err = boundary.HTTPClient().Do(req)
		requestDone <- err
	}()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("TLS dial hook did not start")
	}

	if err := boundary.Abort(); err != nil {
		t.Fatalf("abort in-process boundary: %v", err)
	}
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("blocked TLS dial unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not cancel blocked TLS dial")
	}
}

func TestInProcessBoundaryAbortCancelsBlockedDialContext(t *testing.T) {
	dialStarted := make(chan struct{})
	base := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "http://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}
	requestDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet, boundary.URL()+"/models", nil)
		if err != nil {
			requestDone <- err
			return
		}
		_, err = boundary.HTTPClient().Do(req)
		requestDone <- err
	}()
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("dial hook did not start")
	}

	if err := boundary.Abort(); err != nil {
		t.Fatalf("abort in-process boundary: %v", err)
	}
	select {
	case err := <-requestDone:
		if err == nil {
			t.Fatal("blocked dial unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not cancel blocked dial")
	}
}

func TestInProcessBoundaryRecoversPanickingDialBeforeAbort(t *testing.T) {
	base := &http.Transport{
		Proxy: nil,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			panic("synthetic dial panic")
		},
	}
	boundary, err := startInProcess(context.Background(), Config{UpstreamURL: "http://provider.invalid/v1"}, base)
	if err != nil {
		t.Fatalf("start in-process boundary: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, boundary.URL()+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := boundary.HTTPClient().Do(req); err == nil {
		t.Fatal("panicking dial unexpectedly returned a response")
	}

	abortDone := make(chan error, 1)
	go func() { abortDone <- boundary.Abort() }()
	select {
	case err := <-abortDone:
		if err != nil {
			t.Fatalf("abort after panicking dial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort waited on active request after panicking dial")
	}
}

func TestOwnedTransportAbortAfterPanickingRoundTripDoesNotWaitForever(t *testing.T) {
	owned, err := newOwnedTransport(&http.Transport{})
	if err != nil {
		t.Fatalf("new owned transport: %v", err)
	}
	owned.roundTrip = func(*http.Request) (*http.Response, error) {
		panic("synthetic transport panic")
	}
	req, err := http.NewRequest(http.MethodGet, "http://provider.invalid/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panicking transport unexpectedly returned without panic")
			}
		}()
		_, _ = owned.RoundTrip(req)
	}()

	closeDone := make(chan error, 1)
	go func() { closeDone <- owned.close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close after panicking transport: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close waited on active request after panicking transport")
	}
}

func TestInProcessBoundaryRejectsLegacyDialTLSHook(t *testing.T) {
	_, err := startInProcess(context.Background(), Config{UpstreamURL: "https://provider.invalid/v1"}, &http.Transport{
		DialTLS: func(string, string) (net.Conn, error) {
			return nil, errors.New("legacy hook should not run")
		},
	})
	if !errors.Is(err, errUnsupportedLegacyDialHook) {
		t.Fatalf("legacy DialTLS error = %v, want unsupported-hook error", err)
	}
}

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
