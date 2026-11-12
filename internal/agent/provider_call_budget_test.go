package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProviderCallBudgetBoundsConcurrentSendsAndExcludesMetadata(t *testing.T) {
	ctx := WithProviderCallBudget(t.Context(), 3)
	var sends, rejected atomic.Int32
	transport := toolCallIndexTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	metadata, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://fixture/models", nil)
	response, err := transport.RoundTrip(metadata)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	ctx = withProviderTransportIdentity(ctx, ProviderRequest{ModelID: "fixture"})
	var workers sync.WaitGroup
	for range 10 {
		workers.Go(func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/chat/completions", nil)
			response, err := transport.RoundTrip(req)
			if errors.Is(err, ErrProviderCallBudget) {
				rejected.Add(1)
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			_ = response.Body.Close()
		})
	}
	workers.Wait()
	if sends.Load() != 4 || rejected.Load() != 7 {
		t.Fatalf("sends=%d rejected=%d", sends.Load(), rejected.Load())
	}
}

func TestProviderCallBudgetNestingAndCancellationCannotResetLimit(t *testing.T) {
	parent := WithProviderCallBudget(t.Context(), 2)
	child := WithProviderCallBudget(parent, 1)
	var sends int
	transport := toolCallIndexTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) { sends++; return nil, nil })}
	send := func(ctx context.Context) error {
		ctx = withProviderTransportIdentity(ctx, ProviderRequest{ModelID: "fixture"})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/chat/completions", nil)
		_, err := transport.RoundTrip(req)
		return err
	}
	cancelled, cancel := context.WithCancel(child)
	cancel()
	if !errors.Is(send(cancelled), context.Canceled) {
		t.Fatal("cancelled send reached transport")
	}
	if send(child) != nil || !errors.Is(send(child), ErrProviderCallBudget) || send(parent) != nil {
		t.Fatal("nested scope reset or lost the shared limit")
	}
	if !errors.Is(send(WithProviderCallBudget(parent, 100)), ErrProviderCallBudget) || sends != 2 {
		t.Fatalf("parent cap bypassed: sends=%d", sends)
	}
}

func TestProviderCallBudgetTagsOnlyTrustedInterruptedResponses(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		active, body, incomplete bool
	}{
		{"unscoped EOF", false, false, false},
		{"model EOF", true, false, false},
		{"incomplete model body", true, true, true},
		{"normal model body end", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.active {
				ctx = withProviderTransportIdentity(WithProviderCallBudget(ctx, 1), ProviderRequest{ModelID: "fixture"})
			}
			transport := toolCallIndexTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if !tc.body {
					return nil, io.EOF
				}
				cause := error(io.EOF)
				if tc.incomplete {
					cause = io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(budgetErrorReader{cause})}, nil
			})}
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture/chat/completions", nil)
			response, err := transport.RoundTrip(req)
			if response != nil {
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			want := tc.active && (!tc.body || tc.incomplete)
			if errors.Is(err, ErrProviderTransportEOF) != want {
				t.Fatalf("trusted interruption=%v err=%v", want, err)
			}
		})
	}
}

type budgetErrorReader struct{ err error }

func (r budgetErrorReader) Read([]byte) (int, error) { return 0, r.err }
