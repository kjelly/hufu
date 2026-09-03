// Package providerintrospection discovers bounded runtime metadata from model
// providers. It deliberately has no dependency on the invocation transport.
package providerintrospection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRequestTimeout = 3 * time.Second
	maxResponseBody       = 1 << 20
)

var errRedirectRefused = errors.New("provider redirect refused")

// ProviderRef identifies the provider endpoint used for introspection.
// APIKey is only copied into an Authorization header and is never included in
// errors or RuntimeModelInfo.Raw.
type ProviderRef struct {
	Name     string
	Provider string
	Type     string
	BaseURL  string
	APIKey   string
}

// ModelIntrospector discovers runtime and provider metadata for one model.
type ModelIntrospector interface {
	InspectModel(context.Context, ProviderRef, string) (RuntimeModelInfo, error)
}

// RuntimeModelInfo is the provider-facing metadata collected for one model.
// A zero RuntimeContext means that the model was not loaded by the provider.
type RuntimeModelInfo struct {
	ModelID           string
	ConfiguredContext int
	ModelMaxContext   int
	RuntimeContext    int
	MaxOutputTokens   int

	Capabilities  []string
	Family        string
	ParameterSize string
	Quantization  string

	Parameters map[string]string
	ModelInfo  map[string]any
	Size       int64
	SizeVRAM   int64
	ExpiresAt  string
	Raw        map[string]any
}

// OllamaIntrospector reads Ollama's native /api/show and /api/ps endpoints.
type OllamaIntrospector struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// NewOllamaIntrospector constructs an Ollama introspector.
func NewOllamaIntrospector(baseURL, apiKey string) *OllamaIntrospector {
	return &OllamaIntrospector{BaseURL: baseURL, APIKey: apiKey}
}

// OpenAICompatibleIntrospector reads the common GET /v1/models endpoint.
type OpenAICompatibleIntrospector struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

// NewOpenAICompatibleIntrospector constructs an OpenAI-compatible
// introspector.
func NewOpenAICompatibleIntrospector(baseURL, apiKey string) *OpenAICompatibleIntrospector {
	return &OpenAICompatibleIntrospector{BaseURL: baseURL, APIKey: apiKey}
}

// OpenAIIntrospector is a concise alias for the generic OpenAI-compatible
// implementation.
type OpenAIIntrospector = OpenAICompatibleIntrospector

func (i *OllamaIntrospector) InspectModel(ctx context.Context, provider ProviderRef, modelID string) (RuntimeModelInfo, error) {
	baseURL := provider.BaseURL
	if baseURL == "" && i != nil {
		baseURL = i.BaseURL
	}
	apiKey := provider.APIKey
	if apiKey == "" && i != nil {
		apiKey = i.APIKey
	}
	client := (*http.Client)(nil)
	if i != nil {
		client = i.Client
	}

	showBody, err := requestJSON(ctx, client, baseURL, "/api/show", http.MethodPost, apiKey, map[string]string{"model": unqualifiedModelID(modelID)})
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("ollama show introspection failed: %w", err)
	}
	info, err := ParseOllamaShow(showBody)
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("ollama show metadata invalid: %w", err)
	}
	if info.ModelID == "" {
		info.ModelID = modelID
	}

	psBody, err := requestJSON(ctx, client, baseURL, "/api/ps", http.MethodGet, apiKey, nil)
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("ollama process metadata failed: %w", err)
	}
	runtime, found, err := ParseOllamaPS(psBody, modelID)
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("ollama process metadata invalid: %w", err)
	}
	if found {
		info.RuntimeContext = runtime.RuntimeContext
		info.Size = runtime.Size
		info.SizeVRAM = runtime.SizeVRAM
		info.ExpiresAt = runtime.ExpiresAt
		if info.Raw == nil {
			info.Raw = make(map[string]any)
		}
		info.Raw["runtime"] = runtime.Raw
	}
	return info, nil
}

func (i *OpenAICompatibleIntrospector) InspectModel(ctx context.Context, provider ProviderRef, modelID string) (RuntimeModelInfo, error) {
	baseURL := provider.BaseURL
	if baseURL == "" && i != nil {
		baseURL = i.BaseURL
	}
	apiKey := provider.APIKey
	if apiKey == "" && i != nil {
		apiKey = i.APIKey
	}
	client := (*http.Client)(nil)
	if i != nil {
		client = i.Client
	}
	body, err := requestJSON(ctx, client, baseURL, "/v1/models", http.MethodGet, apiKey, nil)
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("provider model introspection failed: %w", err)
	}
	info, err := ParseOpenAIModels(body, modelID)
	if err != nil {
		return RuntimeModelInfo{}, fmt.Errorf("provider model metadata invalid: %w", err)
	}
	return info, nil
}

func requestJSON(ctx context.Context, configuredClient *http.Client, baseURL, endpoint, method, apiKey string, body any) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
	defer cancel()

	target, err := normalizedEndpoint(baseURL, endpoint)
	if err != nil {
		return nil, errors.New("invalid provider endpoint")
	}
	var requestBody io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, errors.New("could not encode provider request")
		}
		requestBody = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(requestContext, method, target, requestBody)
	if err != nil {
		return nil, errors.New("could not create provider request")
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := safeHTTPClient(configuredClient)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("provider request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider returned status %d", response.StatusCode)
	}

	limited := io.LimitReader(response.Body, maxResponseBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("could not read provider response")
	}
	if len(data) > maxResponseBody {
		return nil, errors.New("provider response exceeds size limit")
	}
	var decoded map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil || decoded == nil {
		return nil, errors.New("provider response is not a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("provider response contains invalid trailing data")
	}
	return redactSensitive(decoded, apiKey).(map[string]any), nil
}

func safeHTTPClient(configured *http.Client) *http.Client {
	if configured == nil {
		configured = &http.Client{}
	}
	client := *configured
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errRedirectRefused
	}
	return &client
}

func normalizedEndpoint(baseURL, endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid URL")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	basePath := strings.TrimRight(parsed.Path, "/")
	if endpoint == "/v1/models" {
		basePath = strings.TrimSuffix(basePath, "/v1")
		parsed.Path = basePath + "/v1/models"
	} else {
		basePath = strings.TrimSuffix(basePath, "/v1")
		parsed.Path = basePath + endpoint
	}
	return parsed.String(), nil
}

func redactSensitive(value any, apiKey string) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			lowerKey := strings.ToLower(key)
			if strings.Contains(lowerKey, "api_key") || strings.Contains(lowerKey, "apikey") || strings.Contains(lowerKey, "authorization") || strings.Contains(lowerKey, "access_token") || strings.Contains(lowerKey, "refresh_token") || strings.Contains(lowerKey, "client_secret") || strings.Contains(lowerKey, "password") || strings.Contains(lowerKey, "credential") || strings.Contains(lowerKey, "secret") || lowerKey == "token" {
				result[key] = "[REDACTED]"
				continue
			}
			result[key] = redactSensitive(item, apiKey)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = redactSensitive(item, apiKey)
		}
		return result
	case string:
		if apiKey != "" {
			return strings.ReplaceAll(typed, apiKey, "[REDACTED]")
		}
	}
	return value
}

func intValue(value any) (int, bool) {
	var integer int64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		integer = parsed
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		integer = int64(typed)
	case int:
		return typed, typed > 0
	case int64:
		integer = typed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		integer = parsed
	default:
		return 0, false
	}
	if integer <= 0 || int64(int(integer)) != integer {
		return 0, false
	}
	return int(integer), true
}
