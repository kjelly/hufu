package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"charm.land/fantasy"
)

func compilationWithRetainedBuffer() (*providerCompilation, weak.Pointer[[1 << 20]byte]) {
	buffer := new([1 << 20]byte)
	buffer[0] = 1
	scope := newProviderCompilation(true, func() { clear(buffer[:]) })
	scope.authorize = func(context.Context) error { runtime.KeepAlive(buffer); return nil }
	return scope, weak.Make(buffer)
}

func TestProviderCompilationDropsFinishedBufferOwnership(t *testing.T) {
	scope, buffer := compilationWithRetainedBuffer()
	scope.innerReturned()
	scope.release()
	for range 3 {
		runtime.GC()
	}
	if buffer.Value() != nil {
		t.Fatal("retaining a finished compilation retained its private buffer owner")
	}
	runtime.KeepAlive(scope)
}

func TestProviderCompilationWaitsForEncoderReturn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	released := make(chan struct{})
	started := make(chan struct{})
	scope := newProviderCompilation(false, func() { close(released) })
	t.Cleanup(func() { scope.innerReturned(); scope.release() })
	go func() { close(started); scope.release() }()
	<-started
	select {
	case <-released:
		t.Fatal("input buffer released while its encoder was active")
	case <-time.After(20 * time.Millisecond):
	}
	scope.innerReturned()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("input buffer did not release after encoder returned")
	}
}

func TestProviderCompilationCancellationWaitsForStreamEncoder(t *testing.T) {
	for _, object := range []bool{false, true} {
		name := "stream"
		if object {
			name = "object_stream"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, proceed, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
			allow := sync.OnceFunc(func() { close(proceed) })
			t.Cleanup(allow)
			limiter := newCapacityLimiter(1)
			admission := &compilationAdmission{recordingAdmission: &recordingAdmission{limiter: limiter}, compile: func(request ProviderRequest) (ProviderRequest, func(), error) {
				wire, cleanup, err := compileFixtureImages(t, request)
				return wire, func() { cleanup(); close(released) }, err
			}}
			inner := &streamLanguageModel{
				streamFn: func(context.Context) fantasy.StreamResponse {
					close(entered)
					<-proceed
					return func(func(fantasy.StreamPart) bool) {}
				},
				objectStreamFn: func(context.Context) fantasy.ObjectStreamResponse {
					close(entered)
					<-proceed
					return func(func(fantasy.ObjectStreamPart) bool) {}
				},
			}
			model := NewAdmittedLanguageModel("test-model", inner, admission)
			messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if object {
					stream, err := model.StreamObject(ctx, fantasy.ObjectCall{Prompt: messages})
					if err == nil {
						for range stream {
						}
					}
				} else {
					stream, err := model.Stream(ctx, fantasy.Call{Prompt: messages})
					if err == nil {
						for range stream {
						}
					}
				}
			}()
			t.Cleanup(func() { allow(); cancel(); <-finished })
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("encoder was not entered")
			}
			cancel()
			select {
			case <-released:
				t.Fatal("cancellation released an active encoder's input")
			case <-time.After(20 * time.Millisecond):
			}
			if limiter.active.Load() != 1 {
				t.Fatal("provider slot released before encoder returned")
			}
			allow()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled stream did not finish")
			}
			select {
			case <-released:
			default:
				t.Fatal("cancelled stream leaked its compiler buffers")
			}
			if limiter.active.Load() != 0 {
				t.Fatal("cancelled stream leaked its provider slot")
			}
		})
	}
}

type drainingProviderBody struct {
	io.ReadCloser
	closing chan struct{}
	allow   chan struct{}
}

func (b *drainingProviderBody) Close() error {
	close(b.closing)
	<-b.allow
	return b.ReadCloser.Close()
}

func TestProviderCompilationDrainsHTTPBeforeReleasingInput(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	released := make(chan struct{})
	body := &drainingProviderBody{ReadCloser: io.NopCloser(strings.NewReader("fixture")), closing: make(chan struct{}), allow: make(chan struct{})}
	allowClose := sync.OnceFunc(func() { close(body.allow) })
	scope := newProviderCompilation(true, func() { close(released) })
	t.Cleanup(func() { allowClose(); scope.innerReturned(); scope.release() })
	ctx := scope.bind(parent)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://fixture.invalid/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := toolCallIndexTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
	scope.innerReturned()
	go scope.release()
	select {
	case <-body.closing:
	case <-ctx.Done():
		t.Fatal("native response did not begin draining")
	}
	select {
	case <-released:
		t.Fatal("input reservation released before HTTP consumer drained")
	default:
	}
	allowClose()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("input reservation leaked after HTTP consumer drained")
	}
}

func TestNativeProviderRequestsDoNotFollowRedirects(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		_, _ = w.Write([]byte("fixture"))
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	for _, native := range []bool{false, true} {
		name := "text"
		if native {
			name = "native image"
		}
		t.Run(name, func(t *testing.T) {
			received.Store(0)
			var scope *providerCompilation
			if native {
				scope = newProviderCompilation(true, nil)
			}
			defer func() { scope.innerReturned(); scope.release() }()
			ctx := scope.bind(t.Context())
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, source.URL, strings.NewReader("synthetic request"))
			if err != nil {
				t.Fatal(err)
			}
			redirects := 0
			client := withToolCallIndexes(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { redirects++; return nil }})
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if native {
				if received.Load() != 0 || redirects != 0 || response.StatusCode != http.StatusTemporaryRedirect {
					t.Fatal("native request followed an unadmitted redirect")
				}
			} else if received.Load() != 1 || redirects != 1 || response.StatusCode != http.StatusOK {
				t.Fatal("native redirect policy changed legacy text behavior")
			}
		})
	}
}

type asynchronousProviderBody struct {
	reading chan struct{}
	closed  chan struct{}
	allow   chan struct{}
}

func (b *asynchronousProviderBody) Read([]byte) (int, error) {
	close(b.reading)
	<-b.allow
	return 0, io.EOF
}

func (b *asynchronousProviderBody) Close() error {
	close(b.closed)
	return nil
}

func TestProviderCompilationDrainsRequestBodyReadsAfterRoundTripReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	body := &asynchronousProviderBody{reading: make(chan struct{}), closed: make(chan struct{}), allow: make(chan struct{})}
	allow := sync.OnceFunc(func() { close(body.allow) })
	released, readDone := make(chan struct{}), make(chan struct{})
	scope := newProviderCompilation(true, func() { close(released) })
	t.Cleanup(func() { allow(); scope.innerReturned(); scope.release() })
	request, err := http.NewRequestWithContext(scope.bind(ctx), http.MethodPost, "https://fixture.invalid/v1/chat/completions", body)
	if err != nil {
		t.Fatal(err)
	}
	var tracked io.ReadCloser
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		tracked = request.Body
		go func() {
			defer close(readDone)
			_, _ = tracked.Read(make([]byte, 1))
		}()
		<-body.reading
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
	})
	if _, err := compiledProviderRoundTrip(request, transport); err != nil {
		t.Fatal(err)
	}
	scope.innerReturned()
	go scope.release()
	select {
	case <-body.closed:
	case <-ctx.Done():
		t.Fatal("native request body was not closed")
	}
	select {
	case <-released:
		t.Fatal("compiler released inputs while HTTP request body was still reading")
	default:
	}
	if _, err := tracked.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("closed native body admitted another reader")
	}
	allow()
	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("request body consumer did not release its reservation")
	}
	<-readDone
}
