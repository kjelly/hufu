package agent

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type providerCompilationKey struct{}

type providerCompilation struct {
	native       bool
	cleanup      func()
	authorize    func(context.Context) error
	returned     chan struct{}
	markReturned func()
	finish       func()
	stopCancel   func() bool
	mu           sync.Mutex
	closed       bool
	bodies       map[*compiledProviderBody]struct{}
	active       sync.WaitGroup
}

func newProviderCompilation(native bool, cleanup func()) *providerCompilation {
	scope := &providerCompilation{native: native, cleanup: cleanup, returned: make(chan struct{}), bodies: make(map[*compiledProviderBody]struct{})}
	scope.markReturned = sync.OnceFunc(func() { close(scope.returned) })
	scope.finish = sync.OnceFunc(func() {
		// Context cancellation may race an SDK encoder before Stream returns.
		// Closing transport is safe then; releasing its input buffers is not.
		<-scope.returned
		if scope.stopCancel != nil {
			scope.stopCancel()
		}
		scope.closeBodies()
		scope.active.Wait()
		if scope.cleanup != nil {
			scope.cleanup()
		}
	})
	return scope
}

func (s *providerCompilation) bind(ctx context.Context) context.Context {
	if s == nil || !s.native {
		return ctx
	}
	s.stopCancel = context.AfterFunc(ctx, s.closeBodies)
	return context.WithValue(ctx, providerCompilationKey{}, s)
}

func (s *providerCompilation) innerReturned() {
	if s != nil {
		s.markReturned()
	}
}

func (s *providerCompilation) release() {
	if s != nil {
		s.finish()
	}
}

func nativeProviderCompilation(ctx context.Context) *providerCompilation {
	scope, _ := ctx.Value(providerCompilationKey{}).(*providerCompilation)
	return scope
}

func (s *providerCompilation) closeBodies() {
	s.mu.Lock()
	s.closed = true
	bodies := make([]*compiledProviderBody, 0, len(s.bodies))
	for body := range s.bodies {
		bodies = append(bodies, body)
	}
	s.mu.Unlock()
	for _, body := range bodies {
		_ = body.Close()
	}
}

func compiledProviderRoundTrip(req *http.Request, transport http.RoundTripper) (*http.Response, error) {
	scope := nativeProviderCompilation(req.Context())
	if scope == nil {
		return transport.RoundTrip(req)
	}
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, context.Canceled
	}
	scope.active.Add(1)
	authorize := scope.authorize
	scope.mu.Unlock()
	done := sync.OnceFunc(scope.active.Done)
	defer done()
	if req.Body != nil {
		body, ok := scope.trackBody(req.Body)
		if !ok {
			return nil, context.Canceled
		}
		req = req.Clone(req.Context())
		req.Body = body
	}
	if err := authorizeCompiledTransport(req.Context(), authorize); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	response, err := transport.RoundTrip(req)
	if response == nil || response.Body == nil {
		return response, err
	}
	body, ok := scope.trackBody(response.Body)
	if !ok {
		return nil, context.Canceled
	}
	if err != nil {
		_ = body.Close()
		return nil, err
	}
	response.Body = body
	return response, nil
}

func authorizeCompiledTransport(ctx context.Context, authorize func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *providerCompilation) trackBody(source io.ReadCloser) (*compiledProviderBody, bool) {
	body := &compiledProviderBody{source: source}
	body.close = sync.OnceValue(func() error {
		body.mu.Lock()
		body.closed = true
		body.mu.Unlock()
		err := source.Close()
		body.readers.Wait()
		s.mu.Lock()
		delete(s.bodies, body)
		s.mu.Unlock()
		s.active.Done()
		return err
	})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = source.Close()
		return nil, false
	}
	s.active.Add(1)
	s.bodies[body] = struct{}{}
	s.mu.Unlock()
	return body, true
}

type compiledProviderBody struct {
	source  io.ReadCloser
	close   func() error
	mu      sync.Mutex
	closed  bool
	readers sync.WaitGroup
}

func (b *compiledProviderBody) Close() error { return b.close() }

func (b *compiledProviderBody) Read(data []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.readers.Add(1)
	b.mu.Unlock()
	defer b.readers.Done()
	return b.source.Read(data)
}
