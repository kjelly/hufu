package scripted

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBoundaryOwnsOfflineTransport(t *testing.T) {
	for _, stream := range []string{"false", "true"} {
		t.Run(stream, func(t *testing.T) {
			p := NewProvider("model", []Step{{Content: "answer", Usage: &Usage{InputTokens: 5, OutputTokens: 2}}})
			b, err := NewBoundary(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Stop() })
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.URL()+"/chat/completions", strings.NewReader(`{"stream":`+stream+`}`))
			if err != nil {
				t.Fatal(err)
			}
			response, err := b.HTTPClient().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), "answer") || !strings.Contains(string(raw), `"total_tokens":7`) || len(p.Issues()) != 0 {
				t.Fatalf("unexpected response %s, issues %v", raw, p.Issues())
			}
			if err := b.Abort(); err != nil {
				t.Fatal(err)
			}
			if _, err := response.Body.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("response body survived abort: %v", err)
			}
			if _, err := b.HTTPClient().Do(request); err == nil {
				t.Fatal("closed boundary accepted request")
			}
		})
	}
}

func TestBoundaryRejectsUnknownDestinationWithoutConsuming(t *testing.T) {
	p := NewProvider("model", []Step{{Content: "answer"}})
	b, err := NewBoundary(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	for _, target := range []string{
		"https://example.com/v1/chat/completions",
		"http://scripted.invalid/other",
		"http://scripted.invalid/v1/chat/completions?target=other",
		"http://user@scripted.invalid/v1/chat/completions",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.HTTPClient().Do(req); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
	if len(p.ObservedUsage()) != 0 {
		t.Fatal("rejected request consumed a response")
	}
}

func TestBoundaryAbortDrainsRequest(t *testing.T) {
	p := NewProvider("model", []Step{{Content: "answer"}})
	b, err := NewBoundary(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.URL()+"/chat/completions", reader)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := b.HTTPClient().Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	// A completed pipe write proves the handler has entered its blocking read.
	if _, err := writer.Write([]byte(`{"stream":false`)); err != nil {
		t.Fatal(err)
	}
	if err := b.Abort(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("abort error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not drain")
	}
	if len(p.ObservedUsage()) != 0 {
		t.Fatal("aborted read consumed response")
	}
}
