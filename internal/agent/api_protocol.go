package agent

import (
	"fmt"
	"net/http"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/charmbracelet/openai-go/option"

	"github.com/kjelly/hufu/internal/config"
)

// apiProtocolBindingVersion covers the pinned adapter capability rules and
// admission estimate semantics too. Bump it when those change after release.
const apiProtocolBindingVersion = 1

// APIProtocolResolution is copied into provider-owned transport policy. It
// never interprets a model name, endpoint name, or model-authored input.
type APIProtocolResolution = config.APIProtocolBinding

func ResolveAPIProtocol(raw string) (APIProtocolResolution, error) {
	requested, err := config.NormalizeAPIProtocol(raw)
	if err != nil {
		return APIProtocolResolution{}, err
	}
	resolution := APIProtocolResolution{Requested: requested, Effective: requested, Source: "explicit", Version: apiProtocolBindingVersion}
	if requested == "" {
		resolution.Effective = config.APIProtocolChatCompletions
		resolution.Source = "legacy"
	}
	return resolution, nil
}

// newConfiguredOpenAICompatibleProvider constructs an explicitly bound transport.
func newConfiguredOpenAICompatibleProvider(baseURL, apiKey, name, rawProtocol string) (*OpenAICompatibleProvider, error) {
	resolution, err := ResolveAPIProtocol(rawProtocol)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = config.DefaultProviderURL
	}
	if name == "" {
		name = "local"
	}
	provider, err := newOpenAICompatProviderWithProtocol(baseURL, apiKey, name, nil, resolution)
	if err != nil {
		return nil, fmt.Errorf("create OpenAI-compatible provider: %w", err)
	}
	return &OpenAICompatibleProvider{provider: provider, apiProtocol: resolution, baseURL: baseURL, apiKey: apiKey, name: name}, nil
}

func newOpenAICompatProviderWithProtocol(baseURL, apiKey, name string, client *http.Client, resolution APIProtocolResolution) (fantasy.Provider, error) {
	responses := resolution.Effective == config.APIProtocolResponses
	options := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithName(name),
		openaicompat.WithHTTPClient(withToolCallIndexes(client)),
		openaicompat.WithUseResponsesAPI(),
		openaicompat.WithResponsesAPIFunc(func(string) bool { return responses }),
	}
	if responses {
		// Prevent the SDK from introducing an environment credential outside the
		// provider identity and the Hufu-owned boundary's configured key.
		options = append(options, openaicompat.WithSDKOptions(option.WithAPIKey(apiKey)))
	}
	if apiKey != "" {
		options = append(options, openaicompat.WithAPIKey(apiKey))
	}
	return openaicompat.New(options...)
}

// ResolveAPIProtocol returns the effective provider's transport binding. It
// follows normal routing/fallback and never selects a protocol by model name.
func (pm *ProviderManager) ResolveAPIProtocol(modelID string) (APIProtocolResolution, error) {
	if pm == nil {
		return APIProtocolResolution{}, fmt.Errorf("provider manager unavailable")
	}
	provider := pm.GetProvider(modelID)
	if provider == nil {
		return APIProtocolResolution{}, fmt.Errorf("no provider for model %q", modelID)
	}
	if provider.apiProtocol.Effective == "" {
		return ResolveAPIProtocol("")
	}
	return provider.apiProtocol, nil
}
