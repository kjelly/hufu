package scripted

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Endpoint is a logical transport identity, never a network destination.
const Endpoint = "http://scripted.invalid/v1"

// Boundary owns an in-memory provider transport. Requests cannot escape to the
// network, and closing the boundary cancels and drains handlers before return.
// The provider owns its cursor; create a new provider and boundary per run.
type Boundary struct {
	provider *Provider
	client   *http.Client
	mu       sync.Mutex
	closed   bool
	next     uint64
	active   map[uint64]func()
	handlers sync.WaitGroup
}

func NewBoundary(ctx context.Context, provider *Provider) (*Boundary, error) {
	if ctx == nil || provider == nil {
		return nil, errors.New("scripted boundary requires context and provider")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := &Boundary{provider: provider, active: make(map[uint64]func())}
	b.client = &http.Client{
		Transport: b,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("scripted transport refuses redirects")
		},
	}
	// Like the ordinary provider proxy, ctx bounds startup only. The runtime
	// invocation owns Abort/Stop; a first worker attempt's context may end
	// before later attempts or auxiliary consumers in the same invocation.
	return b, nil
}

func (*Boundary) URL() string                { return Endpoint }
func (b *Boundary) HTTPClient() *http.Client { return b.client }
func (b *Boundary) Stop() error              { return b.Abort() }

func (b *Boundary) Abort() error {
	b.mu.Lock()
	b.closed = true
	// Cancel outside the lock: response Close removes its registration.
	cancels := make([]func(), 0, len(b.active))
	for _, cancel := range b.active {
		cancels = append(cancels, cancel)
	}
	b.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	b.handlers.Wait()
	return nil
}

func (b *Boundary) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("scripted transport requires a request")
	}
	if request.Body != nil {
		defer func() { _ = request.Body.Close() }()
	}
	if request.URL.Scheme != "http" || request.URL.Host != "scripted.invalid" ||
		request.URL.User != nil || request.URL.RawQuery != "" ||
		(request.URL.Path != "/v1/chat/completions" && request.URL.Path != "/v1/models") {
		return nil, errors.New("scripted transport refuses unknown destination")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(request.Context())
	cancelRequest := func() {
		cancel()
		if request.Body != nil {
			_ = request.Body.Close()
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancelRequest()
		return nil, errors.New("scripted boundary is closed")
	}
	b.next++
	id := b.next
	b.active[id] = cancelRequest
	b.handlers.Add(1)
	b.mu.Unlock()
	defer b.handlers.Done()
	stopRequest := context.AfterFunc(ctx, cancelRequest)
	defer stopRequest()
	recorder := httptest.NewRecorder()
	b.provider.ServeHTTP(recorder, request.Clone(ctx))
	if err := ctx.Err(); err != nil {
		b.remove(id)
		cancelRequest()
		return nil, err
	}
	response := recorder.Result()
	body := &boundaryBody{reader: bytes.NewReader(recorder.Body.Bytes()), done: func() {
		cancel()
		b.remove(id)
	}}
	stopBody := context.AfterFunc(ctx, func() { _ = body.Close() })
	body.mu.Lock()
	body.stop = stopBody
	bodyClosed := body.closed
	body.mu.Unlock()
	if bodyClosed {
		stopBody()
	}
	response.Body = body
	response.Request = request
	b.mu.Lock()
	closed := b.closed
	if !closed {
		b.active[id] = func() { _ = body.Close() }
	}
	b.mu.Unlock()
	if closed {
		_ = body.Close()
		return nil, errors.New("scripted boundary closed during request")
	}
	return response, nil
}

func (b *Boundary) remove(id uint64) {
	b.mu.Lock()
	delete(b.active, id)
	b.mu.Unlock()
}

type boundaryBody struct {
	mu     sync.Mutex
	reader *bytes.Reader
	closed bool
	done   func()
	stop   func() bool
}

func (b *boundaryBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}

func (b *boundaryBody) Close() error {
	b.mu.Lock()
	alreadyClosed := b.closed
	b.closed = true
	stop := b.stop
	b.mu.Unlock()
	if !alreadyClosed {
		if stop != nil {
			stop()
		}
		b.done()
	}
	return nil
}
