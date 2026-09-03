// Package preset defines Hufu's built-in, deterministic agent presets
// (spec.md Specification 01 §6 / Specification 05 Phase 3). A preset is a
// high-level, named shorthand for a tool/side-effect grant that a team
// author can select instead of enumerating a raw tool allowlist.
//
// This package is intentionally a leaf: it has no dependency on
// internal/team, so internal/team can import it without an import cycle.
// SideEffectClass is therefore a local string type rather than an alias of
// team.SideEffectClass; callers convert with a plain string conversion.
package preset

import (
	"sort"
	"strings"
)

// SideEffectClass mirrors the values of team.SideEffectClass
// (none/workspace_write/external_write/infra_mutation/credential_mutation).
// It is declared locally to keep this package free of a dependency on
// internal/team.
type SideEffectClass string

// SchemaVersion identifies the current built-in preset definitions. It has
// no bearing on parsing today, but is exposed so a future `team explain`
// (spec.md Specification 05 Phase 5) can show which preset schema produced
// an agent's effective tools, per Specification 04 §9.
const SchemaVersion = 1

// AgentPreset is a deterministic, versioned expansion of a `preset:` name
// into tools and a default side-effect class.
type AgentPreset struct {
	Name       string
	Tools      []string
	SideEffect SideEffectClass
}

// registry holds the initial built-in presets (spec.md Specification 01
// §6). Security constraint: no preset here may grant "sudo" — see
// Specification 01 §6 and §11 Rule 3. A test in preset_test.go enforces
// this for every entry, so a future addition cannot silently violate it.
var registry = map[string]AgentPreset{
	"readonly": {
		Name:  "readonly",
		Tools: []string{"view", "grep", "glob", "ls"},
	},
	"coding": {
		Name:       "coding",
		Tools:      []string{"view", "write", "edit", "multiedit", "grep", "glob", "ls", "bash"},
		SideEffect: "workspace_write",
	},
	"review": {
		Name:       "review",
		Tools:      []string{"view", "grep", "glob", "ls", "bash"},
		SideEffect: "none",
	},
	"research": {
		Name:  "research",
		Tools: []string{"view", "grep", "glob", "ls", "fetch", "agentic_fetch"},
	},
	"writer": {
		Name:       "writer",
		Tools:      []string{"view", "write", "edit", "grep", "glob", "ls"},
		SideEffect: "workspace_write",
	},
	"ops": {
		Name:       "ops",
		Tools:      []string{"view", "grep", "glob", "ls", "bash", "ssh"},
		SideEffect: "external_write",
	},
}

// Lookup returns the named built-in preset. Matching is case-insensitive
// and trims surrounding whitespace, matching how other Hufu authoring
// fields (e.g. tool names) are normalized.
func Lookup(name string) (AgentPreset, bool) {
	p, ok := registry[normalizePresetName(name)]
	return p, ok
}

// normalizePresetName matches preset names case-insensitively and trims
// surrounding whitespace. Shared by agent and team preset lookup.
func normalizePresetName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Names returns every built-in preset name, sorted, for error messages and
// help text (e.g. "unknown preset %q: available presets are %s").
func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
