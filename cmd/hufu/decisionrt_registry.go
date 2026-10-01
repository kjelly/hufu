package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/rule"
	decisionrtsidecar "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/backend/systemone"
	hufusidecar "github.com/kjelly/hufu/internal/sidecar"
)

type BackendRegistry interface {
	Resolve(context.Context, string) (decisionrt.Backend, error)
	List() []BackendInfo
}

type RegistryOptions struct {
	SidecarModel    string
	ProviderURL     string
	ProviderAPIKey  string
	SystemOneModel  string
	SystemOneURL    string
	SystemOneAPIKey string
}

type BackendInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Type      string `json:"type"`
	Reason    string `json:"reason,omitempty"`
}

type defaultDecisionRTRegistry struct {
	options RegistryOptions
}

func NewDefaultRegistry(options RegistryOptions) BackendRegistry {
	return &defaultDecisionRTRegistry{options: options}
}

func (r *defaultDecisionRTRegistry) Resolve(ctx context.Context, name string) (decisionrt.Backend, error) {
	switch name {
	case "rule":
		return rule.AlwaysAbstain(), nil
	case "sidecar":
		if reason := sidecarAvailabilityReason(r.options); reason != "" {
			return nil, decisionRTBackendUnavailable(reason, nil)
		}
		provider, err := agent.NewOpenAICompatibleProvider(r.options.ProviderURL, r.options.ProviderAPIKey, "local")
		if err != nil {
			return nil, decisionRTBackendUnavailable("sidecar_provider_unavailable", err)
		}
		generator, err := hufusidecar.NewSidecar(ctx, provider, r.options.SidecarModel)
		if err != nil {
			return nil, decisionRTBackendUnavailable("sidecar_initialization_failed", err)
		}
		backend, err := decisionrtsidecar.New(generator)
		if err != nil {
			return nil, decisionRTBackendUnavailable("sidecar_adapter_unavailable", err)
		}
		return backend, nil
	case "systemone":
		backend, err := systemone.New(systemone.Config{
			Endpoint: r.options.SystemOneURL, APIKey: r.options.SystemOneAPIKey, Model: r.options.SystemOneModel,
		})
		if err != nil {
			return nil, decisionRTBackendUnavailable(systemOneUnavailableReason(err), err)
		}
		return backend, nil
	default:
		return nil, decisionRTBackendUnavailable("unknown_backend", nil)
	}
}

func (r *defaultDecisionRTRegistry) List() []BackendInfo {
	sidecarReason := sidecarAvailabilityReason(r.options)
	systemOneReason := systemOneAvailabilityReason(r.options)
	return []BackendInfo{
		{Name: "rule", Available: true, Type: "deterministic"},
		{Name: "sidecar", Available: sidecarReason == "", Type: "generative", Reason: sidecarReason},
		{Name: "systemone", Available: systemOneReason == "", Type: "decision-native", Reason: systemOneReason},
	}
}

// systemOneAvailabilityReason reuses the adapter's own configuration checks.
// Constructing the adapter performs no network I/O, and listing never needs
// the API key.
func systemOneAvailabilityReason(options RegistryOptions) string {
	_, err := systemone.New(systemone.Config{Endpoint: options.SystemOneURL, Model: options.SystemOneModel})
	return systemOneUnavailableReason(err)
}

func systemOneUnavailableReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, systemone.ErrMissingModel):
		return "missing_systemone_model"
	case errors.Is(err, systemone.ErrInvalidModel):
		return "invalid_systemone_model"
	case errors.Is(err, systemone.ErrInvalidEndpoint):
		return "invalid_systemone_url"
	case errors.Is(err, systemone.ErrInvalidAPIKey):
		return "invalid_systemone_api_key"
	default:
		return "systemone_adapter_unavailable"
	}
}

func sidecarAvailabilityReason(options RegistryOptions) string {
	if options.SidecarModel == "" {
		return "missing_sidecar_model"
	}
	if !validDecisionRTSidecarModel(options.SidecarModel) {
		return "invalid_sidecar_model"
	}
	if !validDecisionRTProviderURL(options.ProviderURL) {
		return "invalid_provider_url"
	}
	return ""
}

func validDecisionRTSidecarModel(model string) bool {
	if model == "" || strings.TrimSpace(model) != model || len(model) > 256 || !utf8.ValidString(model) {
		return false
	}
	for _, character := range model {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validDecisionRTProviderURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func decisionRTBackendUnavailable(reason string, cause error) error {
	if cause == nil {
		cause = errors.New(reason)
	}
	return &decisionrt.RuntimeError{Kind: decisionrt.ErrorBackendUnavailable, Err: cause}
}
