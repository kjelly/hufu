package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

var ErrProviderCallBudget = errors.New("provider HTTP call budget exhausted")
var ErrProviderTransportEOF = errors.New("provider HTTP transport ended prematurely")

type providerCallBudgetKey struct{}
type providerCallBudget struct {
	remaining atomic.Int64
	parent    *providerCallBudget
}

// WithProviderCallBudget installs an operation-owned limit shared by all model
// HTTP sends, including provider recovery. Metadata requests are not charged.
// Install it once on the operation context, before constructing its models.
func WithProviderCallBudget(ctx context.Context, calls int) context.Context {
	parent, _ := ctx.Value(providerCallBudgetKey{}).(*providerCallBudget)
	budget := &providerCallBudget{parent: parent}
	budget.remaining.Store(int64(max(calls, 0)))
	return context.WithValue(ctx, providerCallBudgetKey{}, budget)
}

func hasProviderCallBudget(ctx context.Context) bool {
	return ctx.Value(providerCallBudgetKey{}) != nil
}

type providerCallBudgetTransport struct{ base http.RoundTripper }

func (t providerCallBudgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	budget, _ := req.Context().Value(providerCallBudgetKey{}).(*providerCallBudget)
	_, invocation := req.Context().Value(providerIdentityKey{}).(providerTransportIdentity)
	active := budget != nil && invocation
	if active {
		var err error
		if err = req.Context().Err(); err == nil {
			for current := budget; current != nil; current = current.parent {
				err = ErrProviderCallBudget
				for remaining := current.remaining.Load(); remaining > 0; remaining = current.remaining.Load() {
					if current.remaining.CompareAndSwap(remaining, remaining-1) {
						err = nil
						break
					}
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
	}
	response, err := t.base.RoundTrip(req)
	if active {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = fmt.Errorf("%w: %w", ErrProviderTransportEOF, err)
		}
		if response != nil && response.Body != nil {
			response.Body = &providerInterruptedBody{ReadCloser: response.Body}
		}
	}
	return response, err
}

type providerInterruptedBody struct{ io.ReadCloser }

func (b *providerInterruptedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = fmt.Errorf("%w: %w", ErrProviderTransportEOF, err)
	}
	return n, err
}
