package systemone_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
)

func TestNewValidatesConfigurationWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network call")
	})
	client := &http.Client{Transport: transport}
	valid := systemone.Config{Endpoint: "http://127.0.0.1:11434/v1/systemone", Model: testModel, HTTPClient: client}
	tests := []struct {
		name   string
		mutate func(*systemone.Config)
		want   error
	}{
		{name: "valid"},
		{name: "https with port and path", mutate: func(c *systemone.Config) { c.Endpoint = "https://decide.example:8443/custom/path" }},
		{name: "API key", mutate: func(c *systemone.Config) { c.APIKey = "sk-test" }},
		{name: "128 runes", mutate: func(c *systemone.Config) { c.Model = strings.Repeat("é", 128) }},
		{name: "missing model", mutate: func(c *systemone.Config) { c.Model = "" }, want: systemone.ErrMissingModel},
		{name: "model whitespace", mutate: func(c *systemone.Config) { c.Model = " nimble" }, want: systemone.ErrInvalidModel},
		{name: "model control", mutate: func(c *systemone.Config) { c.Model = "nim\tble" }, want: systemone.ErrInvalidModel},
		{name: "model invalid UTF-8", mutate: func(c *systemone.Config) { c.Model = "nim\xffble" }, want: systemone.ErrInvalidModel},
		{name: "model 129 runes", mutate: func(c *systemone.Config) { c.Model = strings.Repeat("a", 129) }, want: systemone.ErrInvalidModel},
		{name: "model over 256 bytes", mutate: func(c *systemone.Config) { c.Model = strings.Repeat("語", 100) }, want: systemone.ErrInvalidModel},
		{name: "empty endpoint", mutate: func(c *systemone.Config) { c.Endpoint = "" }, want: systemone.ErrInvalidEndpoint},
		{name: "ftp endpoint", mutate: func(c *systemone.Config) { c.Endpoint = "ftp://host/v1/systemone" }, want: systemone.ErrInvalidEndpoint},
		{name: "no host", mutate: func(c *systemone.Config) { c.Endpoint = "http:///v1/systemone" }, want: systemone.ErrInvalidEndpoint},
		{name: "port without host", mutate: func(c *systemone.Config) { c.Endpoint = "http://:11434/v1/systemone" }, want: systemone.ErrInvalidEndpoint},
		{name: "userinfo", mutate: func(c *systemone.Config) { c.Endpoint = "http://user:pass@host/v1/systemone" }, want: systemone.ErrInvalidEndpoint},
		{name: "query", mutate: func(c *systemone.Config) { c.Endpoint = "http://host/v1/systemone?x=1" }, want: systemone.ErrInvalidEndpoint},
		{name: "empty query", mutate: func(c *systemone.Config) { c.Endpoint = "http://host/v1/systemone?" }, want: systemone.ErrInvalidEndpoint},
		{name: "fragment", mutate: func(c *systemone.Config) { c.Endpoint = "http://host/v1/systemone#x" }, want: systemone.ErrInvalidEndpoint},
		{name: "empty fragment", mutate: func(c *systemone.Config) { c.Endpoint = "http://host/v1/systemone#" }, want: systemone.ErrInvalidEndpoint},
		{name: "API key newline", mutate: func(c *systemone.Config) { c.APIKey = "sk\r\nX-Injected: 1" }, want: systemone.ErrInvalidAPIKey},
		{name: "API key control", mutate: func(c *systemone.Config) { c.APIKey = "sk\x00" }, want: systemone.ErrInvalidAPIKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			if test.mutate != nil {
				test.mutate(&config)
			}
			backend, err := systemone.New(config)
			if test.want == nil {
				if err != nil || backend.Name() != "systemone" {
					t.Fatalf("backend=%v err=%v", backend, err)
				}
				return
			}
			assertErrorKind(t, err, decisionrt.ErrorConfiguration)
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("network calls = %d", calls.Load())
	}
}

func TestNewDoesNotMutateTheSuppliedClient(t *testing.T) {
	client := &http.Client{}
	if _, err := systemone.New(systemone.Config{Endpoint: "http://127.0.0.1:1/v1/systemone", Model: testModel, HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	if client.CheckRedirect != nil {
		t.Fatal("New changed the caller's client")
	}
}

func TestRequestUsesExactEndpointAndHeaders(t *testing.T) {
	tests := []struct {
		name   string
		apiKey string
		want   string
	}{
		{name: "without API key"},
		{name: "with API key", apiKey: "sk-test", want: "Bearer sk-test"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRecordingServer(t, http.StatusOK, noulResponse("0.7"))
			endpoint := server.URL + "/custom/decide"
			backend := mustNew(t, systemone.Config{Endpoint: endpoint, APIKey: test.apiKey})
			if _, err := backend.Decide(t.Context(), booleanRequest()); err != nil {
				t.Fatal(err)
			}
			request := server.recorded()[0]
			if request.method != http.MethodPost || request.path != "/custom/decide" {
				t.Fatalf("%s %s", request.method, request.path)
			}
			if request.header.Get("Content-Type") != "application/json" || request.header.Get("Accept") != "application/json" {
				t.Fatalf("headers = %#v", request.header)
			}
			if got := request.header.Get("Authorization"); got != test.want {
				t.Fatalf("Authorization = %q, want %q", got, test.want)
			}
			if _, present := request.header["Authorization"]; present != (test.apiKey != "") {
				t.Fatalf("Authorization present = %v", present)
			}
		})
	}
}

func TestRedirectIsRejectedEvenWhenTheClientWouldFollow(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_, _ = io.WriteString(writer, noulResponse("0.9"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/v1/systemone", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	backend := mustNew(t, systemone.Config{Endpoint: redirect.URL + "/v1/systemone", HTTPClient: client})
	_, err := backend.Decide(t.Context(), booleanRequest())
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if followed.Load() {
		t.Fatal("redirect was followed")
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		status int
		want   decisionrt.ErrorKind
	}{
		{status: http.StatusBadRequest, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusUnauthorized, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusForbidden, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusNotFound, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusMethodNotAllowed, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusUnsupportedMediaType, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusUnprocessableEntity, want: decisionrt.ErrorBackendUnavailable},
		{status: http.StatusRequestTimeout, want: decisionrt.ErrorBackendFailure},
		{status: http.StatusTooManyRequests, want: decisionrt.ErrorBackendFailure},
		{status: http.StatusInternalServerError, want: decisionrt.ErrorBackendFailure},
		{status: http.StatusServiceUnavailable, want: decisionrt.ErrorBackendFailure},
		{status: 529, want: decisionrt.ErrorBackendFailure},
		{status: http.StatusConflict, want: decisionrt.ErrorBackendFailure},
		{status: http.StatusFound, want: decisionrt.ErrorBackendFailure},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := newRecordingServer(t, test.status, `{"error":"provider-secret-detail"}`)
			_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), booleanRequest())
			assertErrorKind(t, err, test.want)
			if strings.Contains(errorChainText(err), "provider-secret-detail") {
				t.Fatalf("error repeats provider text: %s", errorChainText(err))
			}
		})
	}
}

func TestNetworkFailureIsBackendFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL + "/v1/systemone"
	server.Close()
	_, err := mustNew(t, systemone.Config{Endpoint: endpoint}).Decide(t.Context(), booleanRequest())
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if strings.Contains(errorChainText(err), "/v1/systemone") {
		t.Fatalf("error repeats the endpoint path: %s", errorChainText(err))
	}
}

func TestCancellationStopsTheRequest(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := mustNew(t, systemone.Config{Endpoint: server.URL + "/v1/systemone"}).Decide(ctx, booleanRequest())
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in the chain", errorChainText(err))
	}
}

func TestResponseSizeLimit(t *testing.T) {
	padding := func(size int) string {
		body := noulResponse("0.7")
		return body[:len(body)-1] + `,"pad":"` + strings.Repeat("x", size-len(body)-9) + `"}`
	}
	exact := padding(64 << 10)
	if len(exact) != 64<<10 {
		t.Fatalf("fixture size = %d", len(exact))
	}
	server := newRecordingServer(t, http.StatusOK, exact)
	if _, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), booleanRequest()); err != nil {
		t.Fatalf("64 KiB response: %v", err)
	}
	server = newRecordingServer(t, http.StatusOK, padding(64<<10+1))
	_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint()}).Decide(t.Context(), booleanRequest())
	assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
}

func TestResponseBodyIsClosed(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &closeRecorder{Reader: strings.NewReader(noulResponse("0.7"))}
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: body, Header: http.Header{}, Request: request}, nil
			})
			backend := mustNew(t, systemone.Config{Endpoint: "http://decide.test/v1/systemone", HTTPClient: &http.Client{Transport: transport}})
			_, _ = backend.Decide(t.Context(), booleanRequest())
			if !body.closed.Load() {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestErrorsOmitSecretsAndState(t *testing.T) {
	const apiKey = "sk-very-secret-key"
	const stateValue = "private-state-value"
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := newRecordingServer(t, status, `{"error":"`+apiKey+` `+stateValue+`"}`)
			request := booleanRequest()
			request.Context = map[string]any{"note": stateValue}
			_, err := mustNew(t, systemone.Config{Endpoint: server.endpoint(), APIKey: apiKey}).Decide(t.Context(), request)
			if err == nil {
				t.Fatal("expected an error")
			}
			text := errorChainText(err)
			for _, secret := range []string{apiKey, stateValue, "Bearer"} {
				if strings.Contains(text, secret) {
					t.Fatalf("error contains %q: %s", secret, text)
				}
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeRecorder struct {
	io.Reader
	closed atomic.Bool
}

func (r *closeRecorder) Close() error {
	r.closed.Store(true)
	return nil
}
