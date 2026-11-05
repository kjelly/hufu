package main

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// apiProtocolDiagnostic is a display projection, not a configuration value or
// durable binding. "legacy" labels an omitted requested value for operators.
type apiProtocolDiagnostic struct {
	Requested      string `json:"requested"`
	Effective      string `json:"effective"`
	Source         string `json:"source"`
	BindingVersion int    `json:"binding_version"`
}

func (d apiProtocolDiagnostic) message() string {
	return fmt.Sprintf("configured API protocol: requested=%s effective=%s source=%s binding_version=%d (no generation probe)", d.Requested, d.Effective, d.Source, d.BindingVersion)
}

type providerProtocolDiagnostic struct {
	backend  string
	protocol apiProtocolDiagnostic
}

// resolveProtocolDiagnostics consumes the canonical provider map produced by
// applyConfiguredBackends. It never constructs or contacts a provider.
func resolveProtocolDiagnostics(providers map[string]config.ProviderConfig) ([]providerProtocolDiagnostic, error) {
	names := slices.Sorted(maps.Keys(providers))
	if !slices.Contains(names, execution.OllamaBackendName) {
		names = append(names, execution.OllamaBackendName)
		slices.Sort(names)
	}
	result := make([]providerProtocolDiagnostic, 0, len(names))
	for _, name := range names {
		binding, err := agent.ResolveAPIProtocol(providers[name].APIProtocol)
		if err != nil {
			return nil, fmt.Errorf("backend %q: %w", name, err)
		}
		requested := binding.Requested
		if requested == "" {
			requested = "legacy"
		}
		result = append(result, providerProtocolDiagnostic{backend: name, protocol: apiProtocolDiagnostic{Requested: requested, Effective: binding.Effective, Source: binding.Source, BindingVersion: binding.Version}})
	}
	return result, nil
}

func collectDoctorAPIProtocols(report *doctorReport, cfg *config.Config) {
	session := new(team.TeamSession)
	if err := applyConfiguredBackends(session, cfg); err != nil {
		report.add("provider.api_protocol.configuration", "", "fail", "provider protocol configuration could not be resolved; inspect backend configuration")
		return
	}
	appendDoctorAPIProtocols(report, "", session.Config.Providers)
}

func appendDoctorAPIProtocols(report *doctorReport, scope string, providers map[string]config.ProviderConfig) {
	selections, err := resolveProtocolDiagnostics(providers)
	if err != nil {
		report.add("provider.api_protocol.configuration", scope, "fail", "invalid api-protocol; use chat-completions or responses for an LLM backend")
		return
	}
	for _, selection := range selections {
		subject := selection.backend
		if scope != "" {
			subject = scope + "/" + subject
		}
		report.Checks = append(report.Checks, doctorCheck{ID: "provider.api_protocol", Subject: subject, Status: "pass", Message: selection.protocol.message(), APIProtocol: new(selection.protocol)})
	}
}

func teamAPIProtocolChecks(providers map[string]config.ProviderConfig) []TeamCheckItem {
	selections, err := resolveProtocolDiagnostics(providers)
	if err != nil {
		return []TeamCheckItem{{ID: "static.api_protocol.configuration", Category: "static", Status: "failed", Required: true, ReasonCode: "api_protocol_invalid", Message: "Invalid api-protocol; use chat-completions or responses for an LLM backend."}}
	}
	items := make([]TeamCheckItem, 0, len(selections))
	for _, selection := range selections {
		items = append(items, TeamCheckItem{ID: "static.api_protocol." + selection.backend, Category: "static", Status: "passed", Required: true, ReasonCode: "api_protocol_selected", Message: selection.protocol.message(), APIProtocol: new(selection.protocol)})
	}
	return items
}
