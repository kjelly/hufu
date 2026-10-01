// Package systemone adapts the DecisionPrimitive Backend contract to the
// native System One decision protocol served at POST /v1/systemone.
//
// The contract is defined in docs/architecture/decision-primitive.md §18A.
package systemone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const (
	backendName       = "systemone"
	maximumModelRunes = 128
	maximumModelBytes = 256
)

// Configuration errors returned (wrapped in a decisionrt.RuntimeError of kind
// ErrorConfiguration) by New. New checks them in this order.
var (
	ErrMissingModel    = errors.New("systemone model is required")
	ErrInvalidModel    = errors.New("invalid systemone model")
	ErrInvalidEndpoint = errors.New("invalid systemone endpoint")
	ErrInvalidAPIKey   = errors.New("invalid systemone API key")
)

// Config configures one System One backend.
type Config struct {
	// Endpoint is the exact URL that receives the POST request.
	Endpoint string
	// APIKey is sent as a bearer token when it is nonempty.
	APIKey string
	// Model is the decision model sent with every request.
	Model string
	// HTTPClient is cloned before use; nil selects an internal default.
	HTTPClient *http.Client
}

type backend struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// New validates config without network I/O and returns a Backend named
// systemone.
func New(config Config) (decisionrt.Backend, error) {
	if config.Model == "" {
		return nil, configurationError(ErrMissingModel)
	}
	if !validModel(config.Model) {
		return nil, configurationError(ErrInvalidModel)
	}
	if !validEndpoint(config.Endpoint) {
		return nil, configurationError(ErrInvalidEndpoint)
	}
	if !validAPIKey(config.APIKey) {
		return nil, configurationError(ErrInvalidAPIKey)
	}
	client := &http.Client{}
	if config.HTTPClient != nil {
		cloned := *config.HTTPClient
		client = &cloned
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &backend{endpoint: config.Endpoint, apiKey: config.APIKey, model: config.Model, client: client}, nil
}

func (b *backend) Name() string {
	return backendName
}

func (b *backend) Decide(ctx context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
	body, candidates, err := encodeRequest(b.model, request)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	response, err := b.post(ctx, body)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	result, err := decodeResult(response, request.Spec.Kind, candidates)
	if err != nil {
		return decisionrt.BackendResult{}, err
	}
	result.Model = b.model
	return result, nil
}

func validModel(model string) bool {
	if strings.TrimSpace(model) != model || len(model) > maximumModelBytes || !utf8.ValidString(model) ||
		utf8.RuneCountInString(model) > maximumModelRunes {
		return false
	}
	return !strings.ContainsFunc(model, unicode.IsControl)
}

func validEndpoint(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.Opaque != "" {
		return false
	}
	return parsed.User == nil && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "" && !strings.Contains(raw, "#")
}

func validAPIKey(key string) bool {
	return !strings.ContainsFunc(key, unicode.IsControl)
}

func configurationError(cause error) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorConfiguration, Backend: backendName, Err: cause}
}

func backendFailure(cause error) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendFailure, Backend: backendName, Err: cause}
}

func backendUnavailable(cause error) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendUnavailable, Backend: backendName, Err: cause}
}

func invalidOutput(message string) error {
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorInvalidBackendOutput, Backend: backendName, Err: fmt.Errorf("systemone: %s", message)}
}
