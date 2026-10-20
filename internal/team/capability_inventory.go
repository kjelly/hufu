package team

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/kjelly/hufu/internal/utils"
)

type CapabilityDiagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
}

type CapabilityToolDeclaration struct {
	Allowed  []string `json:"allowed"`
	Excluded []string `json:"excluded"`
}

type CapabilityInventorySource struct {
	Scope    string `json:"scope"`
	Selected bool   `json:"selected"`
	Draft    bool   `json:"draft"`
}

// StaticCapability is declaration metadata, never an authorization decision.
// A nil digest or declaration means that information is not known offline.
type StaticCapability struct {
	SchemaVersion  int                         `json:"schema_version"`
	ID             string                      `json:"id"`
	Name           string                      `json:"name"`
	Origin         string                      `json:"origin"`
	Owner          string                      `json:"owner"`
	ContentDigest  *string                     `json:"content_digest"`
	Availability   string                      `json:"availability"`
	DeclaredTools  *CapabilityToolDeclaration  `json:"declared_tools"`
	DeclaredGrants map[string][]string         `json:"declared_grants"`
	EffectClass    string                      `json:"effect_class"`
	Diagnostics    []CapabilityDiagnostic      `json:"diagnostics"`
	Sources        []CapabilityInventorySource `json:"sources,omitempty"`
}

type CapabilityInventory struct {
	SchemaVersion int                    `json:"schema_version"`
	Team          string                 `json:"team"`
	Complete      bool                   `json:"complete"`
	Capabilities  []StaticCapability     `json:"capabilities"`
	Diagnostics   []CapabilityDiagnostic `json:"diagnostics"`
}

// InspectCapabilities uses the static compiler and lint pipeline. It starts
// no runtime services, installs nothing, and creates no workspace.
func InspectCapabilities(teamDir string) (CapabilityInventory, error) {
	inspection, err := InspectTeam(teamDir, nil, nil, DefaultProviderRegistry, TeamCompileLint)
	if err != nil {
		// Compiler errors can contain source text, absolute paths or argv.
		return CapabilityInventory{}, fmt.Errorf("team capability definitions could not be inspected")
	}
	lint, err := lintTeamInspection(teamDir, TeamLintOptions{}, inspection)
	if err != nil {
		return CapabilityInventory{}, fmt.Errorf("team capability policy could not be inspected")
	}
	result := CapabilityInventory{SchemaVersion: 1, Team: capabilityLabel(lint.Team),
		Complete: lint.Complete, Capabilities: []StaticCapability{}, Diagnostics: []CapabilityDiagnostic{}}
	for _, finding := range lint.Findings {
		result.Diagnostics = append(result.Diagnostics, CapabilityDiagnostic{finding.Code, finding.Severity})
		if finding.Severity == FindingSeverityError && !finding.Ignored {
			result.Complete = false
		}
	}
	if session := inspection.Session; session != nil {
		result.Capabilities = append(result.Capabilities, capabilitySkills(session)...)
		result.Capabilities = append(result.Capabilities, capabilityProviders(session, lint.Findings)...)
		result.Capabilities = append(result.Capabilities, capabilityMCP(session)...)
	}
	slices.SortFunc(result.Capabilities, func(a, b StaticCapability) int {
		return cmp.Or(cmp.Compare(a.Origin, b.Origin), cmp.Compare(a.Owner, b.Owner), cmp.Compare(a.ID, b.ID))
	})
	result.Diagnostics = compactCapabilityDiagnostics(result.Diagnostics)
	return result, nil
}

func capabilityBase(origin, owner, id, name string) StaticCapability {
	return StaticCapability{
		SchemaVersion: 1, Origin: origin, Owner: capabilityLabel(owner), ID: capabilityLabel(id),
		Name: capabilityLabel(name), Availability: "unknown", EffectClass: "unknown",
		Diagnostics: []CapabilityDiagnostic{},
	}
}

// Only identifiers cross this boundary. Unusual/path-like values and known
// credentials retain a stable opaque identity instead of leaking source text.
func capabilityLabel(value string) string {
	value = strings.TrimSpace(value)
	safe := value != "" && len(value) <= 160 && utils.RedactSecrets(value) == value
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_.-*:", r) {
			safe = false
		}
	}
	if safe {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("opaque-%x", sum[:12])
}

func capabilityNames(names []string) []string {
	if names == nil {
		return nil
	}
	result := make([]string, len(names))
	for i, name := range names {
		result[i] = capabilityLabel(name)
	}
	return result
}

func compactCapabilityDiagnostics(values []CapabilityDiagnostic) []CapabilityDiagnostic {
	slices.SortFunc(values, func(a, b CapabilityDiagnostic) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.Severity, b.Severity))
	})
	return slices.Compact(values)
}

func capabilityProviders(session *TeamSession, findings []TeamLintFinding) []StaticCapability {
	var result []StaticCapability
	owner := capabilityLabel(session.Config.Name)
	for _, name := range slices.Sorted(maps.Keys(session.Config.ActionProviders)) {
		row := capabilityBase("action_provider", owner, "provider:"+name, name)
		row.Availability = "available"
		if digest := actionProviderIdentityHash(session, name); digest != "" {
			row.ContentDigest = new(digest)
		}
		if session.Config.ActionProviders[name].Runtime == mcpActionRuntime {
			row.Availability = "unknown"
			row.Diagnostics = append(row.Diagnostics, CapabilityDiagnostic{"remote_descriptor_unavailable", "info"})
		}
		result = append(result, row)
	}
	if session.ActionCatalog == nil {
		return result
	}
	for _, entry := range session.ActionCatalog.Entries {
		row := capabilityBase("action_provider", owner, "action:"+entry.ID, entry.ID)
		row.Availability, row.ContentDigest = "available", new(entry.Hash)
		row.DeclaredGrants = map[string][]string{
			"discover": capabilityNames(entry.Discover), "propose": capabilityNames(entry.Propose),
			"executor": {capabilityLabel(entry.Agent)},
		}
		row.EffectClass = capabilityEffect(entry.SideEffect)
		for _, finding := range findings {
			if finding.FieldPath == actionCatalogField(entry.ID, "") || strings.HasPrefix(finding.FieldPath, actionCatalogField(entry.ID, "")+".") {
				row.Diagnostics = append(row.Diagnostics, CapabilityDiagnostic{finding.Code, finding.Severity})
				if finding.Severity == FindingSeverityError {
					row.Availability, row.EffectClass = "unavailable", "unknown"
				}
			}
		}
		if _, ok := mcpActionProviderConfig(session, entry.Capability); ok && row.Availability == "available" {
			row.Availability = "unknown"
			row.Diagnostics = append(row.Diagnostics, CapabilityDiagnostic{"remote_descriptor_unavailable", "info"})
		}
		result = append(result, row)
	}
	return result
}

func capabilityEffect(effect SideEffectClass) string {
	switch effect {
	case SideEffectNone:
		return "none"
	case SideEffectWorkspaceWrite:
		return "workspace_write"
	case SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential:
		return "external"
	default:
		return "unknown"
	}
}
