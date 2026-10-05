package ollamaweb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func webResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var webErr *Error
	if !errors.As(err, &webErr) || webErr.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

func TestSearchUsesFixedHostedEndpointAndBearerKey(t *testing.T) {
	const key = "test-secret-ollama-key"
	calls := 0
	client := NewHTTPClient(key, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != searchURL {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+key {
			t.Fatalf("authorization = %q", got)
		}
		if req.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", req.Header.Get("Content-Type"))
		}
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(payload); got != `{"query":"current release","max_results":3}` {
			t.Fatalf("payload = %s", got)
		}
		return webResponse(http.StatusOK, `{"results":[{"title":"Source","url":"https://example.com/","content":"Facts"}]}`), nil
	}))
	result, err := client.Search(t.Context(), SearchRequest{Query: " current release ", MaxResults: 3})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(result.Results) != 1 || result.Results[0].URL != "https://example.com/" {
		t.Fatalf("calls = %d, result = %+v", calls, result)
	}
}

func TestFetchUsesHostedEndpoint(t *testing.T) {
	client := NewHTTPClient("key", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != fetchURL {
			t.Fatalf("request URL = %s", req.URL)
		}
		return webResponse(http.StatusOK, `{"title":"Page","content":"Body","links":["https://example.com/a"]}`), nil
	}))
	result, err := client.Fetch(t.Context(), FetchRequest{URL: "https://example.com/"})
	if err != nil || result.Title != "Page" || len(result.Links) != 1 {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}

func TestDefaultTransportKeepsTLSVerification(t *testing.T) {
	client := NewHTTPClient("key", nil)
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("default transport does not verify TLS certificates: %#v", client.http.Transport)
	}
}

func TestFetchRejectsPrivateAndMalformedURLsBeforeRequest(t *testing.T) {
	client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid URL reached HTTP transport")
		return nil, nil
	}))
	for _, raw := range []string{
		"", "file:///etc/passwd", "https://", "https://user:pass@example.com/",
		"http://localhost/", "http://service.localhost/", "http://127.0.0.1/",
		"http://10.1.2.3/", "http://169.254.1.1/", "http://0.0.0.0/",
		"http://127.0.0.1./", "http://127.1/", "http://2130706433/",
		"http://[::1]/", "http://[fe80::1]/", "http://[::ffff:127.0.0.1]/",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := client.Fetch(t.Context(), FetchRequest{URL: raw})
			requireCode(t, err, "invalid_arguments")
		})
	}
}

func TestSearchValidatesBeforeRequest(t *testing.T) {
	client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid query reached HTTP transport")
		return nil, nil
	}))
	for _, req := range []SearchRequest{
		{Query: " ", MaxResults: 3},
		{Query: strings.Repeat("x", 2049), MaxResults: 3},
		{Query: "valid", MaxResults: 0},
		{Query: "valid", MaxResults: 11},
	} {
		_, err := client.Search(t.Context(), req)
		requireCode(t, err, "invalid_arguments")
	}
}

func TestSearchRetries429WithoutLeakingUpstreamBody(t *testing.T) {
	const key = "test-secret-ollama-key"
	calls := 0
	client := NewHTTPClient(key, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		resp := webResponse(http.StatusTooManyRequests, "upstream echoed "+key)
		resp.Header.Set("Retry-After", "0")
		return resp, nil
	}))
	_, err := client.Search(t.Context(), SearchRequest{Query: "release", MaxResults: 3})
	requireCode(t, err, "ollama_web_rate_limited")
	if calls != maximumAttempts || strings.Contains(err.Error(), key) {
		t.Fatalf("calls = %d, error = %q", calls, err)
	}
}

func TestSearchRetriesTransientServerFailure(t *testing.T) {
	calls := 0
	client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			resp := webResponse(http.StatusServiceUnavailable, "")
			resp.Header.Set("Retry-After", "0")
			return resp, nil
		}
		return webResponse(http.StatusOK, `{"results":[]}`), nil
	}))
	result, err := client.Search(t.Context(), SearchRequest{Query: "release", MaxResults: 3})
	if err != nil || calls != 2 || len(result.Results) != 0 {
		t.Fatalf("calls = %d, result = %+v, error = %v", calls, result, err)
	}
}

func TestSearchDoesNotRetryAuthOrRedirect(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				resp := webResponse(status, "sensitive response")
				resp.Header.Set("Location", "https://other.example/receive")
				return resp, nil
			}))
			_, err := client.Search(t.Context(), SearchRequest{Query: "release", MaxResults: 3})
			if calls != 1 || err == nil || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
		})
	}
}

func TestSearchBoundsAndValidatesResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "oversize", body: strings.Repeat("x", maxSearchBytes+1), code: "ollama_web_response_too_large"},
		{name: "bad json", body: "{", code: "ollama_web_bad_response"},
		{name: "missing results", body: `{}`, code: "ollama_web_bad_response"},
		{name: "wrong results type", body: `{"results":{}}`, code: "ollama_web_bad_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
				return webResponse(http.StatusOK, tc.body), nil
			}))
			_, err := client.Search(t.Context(), SearchRequest{Query: "release", MaxResults: 3})
			requireCode(t, err, tc.code)
		})
	}
}

func TestFetchBoundsAndValidatesResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "oversize", body: strings.Repeat("x", maxFetchBytes+1), code: "ollama_web_response_too_large"},
		{name: "missing content", body: `{"title":"Page","links":[]}`, code: "ollama_web_bad_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
				return webResponse(http.StatusOK, tc.body), nil
			}))
			_, err := client.Fetch(t.Context(), FetchRequest{URL: "https://example.com"})
			requireCode(t, err, tc.code)
		})
	}
}

func TestSearchMissingKeyAndCancellationDoNotRequest(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return webResponse(http.StatusOK, `{"results":[]}`), nil
	})
	_, err := NewHTTPClient("", transport).Search(t.Context(), SearchRequest{Query: "release", MaxResults: 3})
	requireCode(t, err, "ollama_web_api_key_missing")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewHTTPClient("key", transport).Search(ctx, SearchRequest{Query: "release", MaxResults: 3})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("calls = %d, error = %v", calls, err)
	}
}

func TestSearchStopsInFlightTransportOnParentDeadline(t *testing.T) {
	client := NewHTTPClient("key", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := client.Search(ctx, SearchRequest{Query: "release", MaxResults: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want parent deadline", err)
	}
}

func TestRetryAfterLongerThanDeadlineStops(t *testing.T) {
	for _, value := range []string{"30", "999999999999"} {
		t.Run(value, func(t *testing.T) {
			client := NewHTTPClient("key", roundTripFunc(func(*http.Request) (*http.Response, error) {
				resp := webResponse(http.StatusTooManyRequests, "")
				resp.Header.Set("Retry-After", value)
				return resp, nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := client.Search(ctx, SearchRequest{Query: "release", MaxResults: 3})
			requireCode(t, err, "ollama_web_rate_limited")
		})
	}
}
