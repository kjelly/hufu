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
	hufusidecar "github.com/kjelly/hufu/internal/sidecar"
)

type BackendRegistry interface {
	Resolve(context.Context, string) (decisionrt.Backend, error)
	List() []BackendInfo
}

type RegistryOptions struct {
	SidecarModel   string
	ProviderURL    string
	ProviderAPIKey string
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
	default:
		return nil, decisionRTBackendUnavailable("unknown_backend", nil)
	}
}

func (r *defaultDecisionRTRegistry) List() []BackendInfo {
	reason := sidecarAvailabilityReason(r.options)
	return []BackendInfo{
		{Name: "rule", Available: true, Type: "deterministic"},
		{Name: "sidecar", Available: reason == "", Type: "generative", Reason: reason},
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
