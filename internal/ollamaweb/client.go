package ollamaweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	searchURL       = "https://ollama.com/api/web_search"
	fetchURL        = "https://ollama.com/api/web_fetch"
	requestTimeout  = 30 * time.Second
	maxSearchBytes  = 2 << 20
	maxFetchBytes   = 4 << 20
	maxQueryBytes   = 2048
	maxURLBytes     = 4096
	maximumAttempts = 3
)

// Client is the hosted web service dependency used by Hufu's agent tools.
type Client interface {
	Search(context.Context, SearchRequest) (SearchResponse, error)
	Fetch(context.Context, FetchRequest) (FetchResponse, error)
}

type SearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

type FetchRequest struct {
	URL string `json:"url"`
}

type FetchResponse struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Links   []string `json:"links"`
}

// Error has a stable, secret-free message. HTTP response bodies and transport
// errors are deliberately not retained because they may contain credentials.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func serviceError(code, message string) error { return &Error{Code: code, Message: message} }

// HTTPClient connects only to Ollama's hosted web endpoints. An injected
// transport lets tests exercise the complete HTTP path without network access.
type HTTPClient struct {
	apiKey string
	http   *http.Client
}

func NewHTTPClient(apiKey string, transport http.RoundTripper) *HTTPClient {
	if transport == nil {
		if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = defaults.Clone()
		} else {
			transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
	}
	return &HTTPClient{
		apiKey: strings.TrimSpace(apiKey),
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func ValidateSearchRequest(req SearchRequest) (SearchRequest, error) {
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" || len(req.Query) > maxQueryBytes || !utf8.ValidString(req.Query) {
		return SearchRequest{}, serviceError("invalid_arguments", "query must contain 1 to 2048 UTF-8 bytes")
	}
	if req.MaxResults < 1 || req.MaxResults > 10 {
		return SearchRequest{}, serviceError("invalid_arguments", "max_results must be an integer from 1 to 10")
	}
	return req, nil
}

func ValidateFetchRequest(req FetchRequest) (FetchRequest, error) {
	if len(req.URL) == 0 || len(req.URL) > maxURLBytes || !utf8.ValidString(req.URL) {
		return FetchRequest{}, serviceError("invalid_arguments", "url must contain 1 to 4096 UTF-8 bytes")
	}
	parsed, err := url.Parse(req.URL)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() == "" || parsed.Host == "" {
		return FetchRequest{}, serviceError("invalid_arguments", "url must be an absolute HTTP or HTTPS URL without userinfo")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return FetchRequest{}, serviceError("invalid_arguments", "url must be an absolute HTTP or HTTPS URL without userinfo")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return FetchRequest{}, serviceError("invalid_arguments", "url host is not allowed")
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
		addr = addr.Unmap()
		if addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
			return FetchRequest{}, serviceError("invalid_arguments", "url host is not allowed")
		}
	} else if strings.Trim(host, "0123456789.") == "" {
		// Refuse non-canonical numeric hosts, whose eventual resolver may
		// interpret them as an alternate spelling of a private IPv4 address.
		return FetchRequest{}, serviceError("invalid_arguments", "url host is not allowed")
	}
	return req, nil
}

func (c *HTTPClient) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	req, err := ValidateSearchRequest(req)
	if err != nil {
		return SearchResponse{}, err
	}
	var wire struct {
		Results json.RawMessage `json:"results"`
	}
	if err := c.request(ctx, searchURL, req, maxSearchBytes, &wire); err != nil {
		return SearchResponse{}, err
	}
	if len(wire.Results) == 0 || bytes.Equal(wire.Results, []byte("null")) {
		return SearchResponse{}, serviceError("ollama_web_bad_response", "Ollama web search returned an invalid response")
	}
	var results []SearchResult
	if err := json.Unmarshal(wire.Results, &results); err != nil {
		return SearchResponse{}, serviceError("ollama_web_bad_response", "Ollama web search returned an invalid response")
	}
	return SearchResponse{Results: results}, nil
}

func (c *HTTPClient) Fetch(ctx context.Context, req FetchRequest) (FetchResponse, error) {
	req, err := ValidateFetchRequest(req)
	if err != nil {
		return FetchResponse{}, err
	}
	var wire struct {
		Title   *string   `json:"title"`
		Content *string   `json:"content"`
		Links   *[]string `json:"links"`
	}
	if err := c.request(ctx, fetchURL, req, maxFetchBytes, &wire); err != nil {
		return FetchResponse{}, err
	}
	if wire.Title == nil || wire.Content == nil || wire.Links == nil {
		return FetchResponse{}, serviceError("ollama_web_bad_response", "Ollama web fetch returned an invalid response")
	}
	return FetchResponse{Title: *wire.Title, Content: *wire.Content, Links: *wire.Links}, nil
}

func (c *HTTPClient) request(parent context.Context, endpoint string, input any, limit int64, output any) error {
	if c == nil || c.apiKey == "" || c.http == nil {
		return serviceError("ollama_web_api_key_missing", "OLLAMA_API_KEY is not configured")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return serviceError("invalid_arguments", "invalid web request")
	}
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	for attempt := range maximumAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return serviceError("invalid_arguments", "invalid web request")
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if attempt+1 < maximumAttempts {
				if err := waitRetry(ctx, attempt, ""); err != nil {
					return err
				}
				continue
			}
			return serviceError("ollama_web_unavailable", "Ollama web service is unavailable")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			_ = resp.Body.Close()
			if retryableStatus(resp.StatusCode) && attempt+1 < maximumAttempts {
				if err := waitRetry(ctx, attempt, resp.Header.Get("Retry-After")); err != nil {
					if errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil {
						return statusError(resp.StatusCode)
					}
					return err
				}
				continue
			}
			return statusError(resp.StatusCode)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			return serviceError("ollama_web_unavailable", "Ollama web service response could not be read")
		}
		if int64(len(data)) > limit {
			return serviceError("ollama_web_response_too_large", "Ollama web service response is too large")
		}
		if err := json.Unmarshal(data, output); err != nil {
			return serviceError("ollama_web_bad_response", "Ollama web service returned invalid JSON")
		}
		return nil
	}
	return serviceError("ollama_web_unavailable", "Ollama web service is unavailable")
}

func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return serviceError("ollama_web_auth_failed", "Ollama web service rejected its API key")
	case http.StatusTooManyRequests:
		return serviceError("ollama_web_rate_limited", "Ollama web service is rate limited")
	default:
		return serviceError("ollama_web_unavailable", "Ollama web service is unavailable")
	}
}

func waitRetry(ctx context.Context, attempt int, retryAfter string) error {
	delay := time.Duration(250+attempt*500) * time.Millisecond
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
		if seconds > int(requestTimeout/time.Second) {
			return context.DeadlineExceeded
		}
		delay = time.Duration(seconds) * time.Second
	} else if when, err := http.ParseTime(retryAfter); err == nil {
		delay = max(0, time.Until(when))
	}
	if deadline, ok := ctx.Deadline(); ok && delay > time.Until(deadline) {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
