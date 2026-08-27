package providerproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

var errBoundaryClosed = errors.New("provider hard-abort boundary closed")

// InProcess is the fallback boundary used when the subprocess proxy cannot
// bind its loopback listener. It owns the HTTP transport and every connection
// and response body created through that transport.
type InProcess struct {
	url       string
	client    *http.Client
	transport *ownedTransport
	closeOnce sync.Once
	closeErr  error
}

// StartInProcess creates an invocation-exclusive HTTP client without opening
// a local listener. Credentials remain in Fantasy's request options and are
// never copied into a URL, environment variable, or persisted value.
func StartInProcess(ctx context.Context, cfg Config) (*InProcess, error) {
	return startInProcess(ctx, cfg, nil)
}

func startInProcess(ctx context.Context, cfg Config, base *http.Transport) (*InProcess, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start in-process provider boundary: %w", err)
	}
	if base == nil {
		base = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
		}
	}
	owned, err := newOwnedTransport(base)
	if err != nil {
		return nil, err
	}
	return &InProcess{
		url:       strings.TrimRight(cfg.UpstreamURL, "/"),
		client:    &http.Client{Transport: owned},
		transport: owned,
	}, nil
}

func (b *InProcess) URL() string {
	if b == nil {
		return ""
	}
	return b.url
}

func (b *InProcess) HTTPClient() *http.Client {
	if b == nil {
		return nil
	}
	return b.client
}

// Abort closes all active response bodies and connections, then closes idle
// transport state. It is synchronous and idempotent.
func (b *InProcess) Abort() error { return b.close() }

func (b *InProcess) Stop() error { return b.close() }

func (b *InProcess) close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if b.transport != nil {
			b.closeErr = b.transport.close()
		}
	})
	return b.closeErr
}

type ownedTransport struct {
	base    *http.Transport
	dial    func(context.Context, string, string) (net.Conn, error)
	dialTLS func(context.Context, string, string) (net.Conn, error)

	mu      sync.Mutex
	closed  bool
	conns   map[net.Conn]struct{}
	bodies  map[io.ReadCloser]struct{}
	cancels map[*activeRequest]context.CancelFunc
	active  sync.WaitGroup
}

var errUnsupportedLegacyDialHook = errors.New("provider boundary does not support legacy dial hooks")

func newOwnedTransport(base *http.Transport) (*ownedTransport, error) {
	base = base.Clone()
	// http.Transport's legacy Dial and DialTLS hooks cannot be cancelled.
	// Inspect them without naming the deprecated fields directly so the
	// connection-owning fallback fails closed instead of allowing an
	// uninterruptible hook to escape Abort's synchronous cleanup.
	if legacyDialHookSet(base, "Dial") || legacyDialHookSet(base, "DialTLS") {
		return nil, fmt.Errorf("%w: use DialContext and DialTLSContext", errUnsupportedLegacyDialHook)
	}
	dial := base.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	owned := &ownedTransport{
		base:    base,
		dial:    dial,
		conns:   make(map[net.Conn]struct{}),
		bodies:  make(map[io.ReadCloser]struct{}),
		cancels: make(map[*activeRequest]context.CancelFunc),
	}
	if base.DialTLSContext != nil {
		owned.dialTLS = base.DialTLSContext
		base.DialTLSContext = owned.dialTLSContext
	}
	base.DialContext = owned.dialContext
	return owned, nil
}

func legacyDialHookSet(base *http.Transport, name string) bool {
	field := reflect.ValueOf(base).Elem().FieldByName(name)
	return field.IsValid() && field.Kind() == reflect.Func && !field.IsNil()
}

func (t *ownedTransport) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errBoundaryClosed
	}
	dial := t.dial
	t.mu.Unlock()

	conn, err := dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tracked := &ownedConn{Conn: conn, owner: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = tracked.Close()
		return nil, errBoundaryClosed
	}
	t.conns[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *ownedTransport) dialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errBoundaryClosed
	}
	dial := t.dialTLS
	t.mu.Unlock()

	conn, err := dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tracked := &ownedConn{Conn: conn, owner: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = tracked.Close()
		return nil, errBoundaryClosed
	}
	t.conns[tracked] = struct{}{}
	t.mu.Unlock()
	return tracked, nil
}

func (t *ownedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	request, ok := t.begin(req.Context())
	if !ok {
		return nil, errBoundaryClosed
	}
	req = req.WithContext(request.ctx)

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.end(request)
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		t.end(request)
		return resp, nil
	}
	body := &ownedBody{ReadCloser: resp.Body, owner: t, request: request}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = body.Close()
		return nil, errBoundaryClosed
	}
	t.bodies[body] = struct{}{}
	t.mu.Unlock()
	resp.Body = body
	return resp, nil
}

type activeRequest struct {
	ctx     context.Context
	endOnce sync.Once
}

func (t *ownedTransport) begin(parent context.Context) (*activeRequest, bool) {
	ctx, cancel := context.WithCancel(parent)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		cancel()
		return nil, false
	}
	request := &activeRequest{ctx: ctx}
	t.cancels[request] = cancel
	t.active.Add(1)
	return request, true
}

func (t *ownedTransport) end(request *activeRequest) {
	if request == nil {
		return
	}
	request.endOnce.Do(func() {
		t.mu.Lock()
		if cancel, ok := t.cancels[request]; ok {
			delete(t.cancels, request)
			cancel()
		}
		t.mu.Unlock()
		t.active.Done()
	})
}

func (t *ownedTransport) removeConn(conn net.Conn) {
	t.mu.Lock()
	delete(t.conns, conn)
	t.mu.Unlock()
}

func (t *ownedTransport) removeBody(body io.ReadCloser) {
	t.mu.Lock()
	delete(t.bodies, body)
	t.mu.Unlock()
}

func (t *ownedTransport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	bodies := make([]io.ReadCloser, 0, len(t.bodies))
	for body := range t.bodies {
		bodies = append(bodies, body)
	}
	conns := make([]net.Conn, 0, len(t.conns))
	for conn := range t.conns {
		conns = append(conns, conn)
	}
	cancels := make([]context.CancelFunc, 0, len(t.cancels))
	for _, cancel := range t.cancels {
		cancels = append(cancels, cancel)
	}
	t.bodies = make(map[io.ReadCloser]struct{})
	t.conns = make(map[net.Conn]struct{})
	t.cancels = make(map[*activeRequest]context.CancelFunc)
	t.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	var joined error
	for _, body := range bodies {
		joined = errors.Join(joined, body.Close())
	}
	for _, conn := range conns {
		joined = errors.Join(joined, conn.Close())
	}
	t.base.CloseIdleConnections()
	t.active.Wait()
	return joined
}

type ownedConn struct {
	net.Conn
	owner *ownedTransport
	once  sync.Once
}

func (c *ownedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.owner.removeConn(c)
	})
	return err
}

type ownedBody struct {
	io.ReadCloser
	owner   *ownedTransport
	request *activeRequest
	once    sync.Once
}

func (b *ownedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.owner.end(b.request)
	}
	return n, err
}

func (b *ownedBody) Close() error {
	var err error
	b.once.Do(func() {
		err = b.ReadCloser.Close()
		b.owner.removeBody(b)
		b.owner.end(b.request)
	})
	return err
}
