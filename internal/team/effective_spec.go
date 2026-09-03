package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValueSource identifies where a resolved value on an EffectiveTeamSpec
// came from (spec.md Specification 02 §4/§8).
type ValueSource string

const (
	SourceBuiltin  ValueSource = "builtin"
	SourcePreset   ValueSource = "preset"
	SourceFilename ValueSource = "filename"
	SourceTeam     ValueSource = "team"
	SourceAgent    ValueSource = "agent"
)

// ResolvedValue pairs a resolved value with the layer that produced it, so
// a future `hufu team explain` (Specification 05 Phase 5) can show why a
// value is what it is.
type ResolvedValue[T any] struct {
	Value  T           `json:"value" yaml:"value"`
	Source ValueSource `json:"source" yaml:"source"`
	Detail string      `json:"detail,omitempty" yaml:"detail,omitempty"`
}

// EffectiveAgentSpec is one agent's resolved identity/capability summary
// with provenance.
type EffectiveAgentSpec struct {
	Name       ResolvedValue[string]
	Role       ResolvedValue[string]
	Tools      ResolvedValue[string]
	SideEffect ResolvedValue[string]
}

// EffectiveTeamSpec is the immutable, provenance-annotated result of
// compiling a team directory (spec.md Specification 02). It is produced
// once by CompileTeam and never mutated afterward; every exported field is
// a value, not a pointer into mutable state, except RuntimeSession, which
// is the single explicit, documented projection point back into the
// existing runtime session (Specification 02 §9).
//
// EffectiveTeamSpec does not reimplement team loading: today it is a
// provenance-tracking wrapper around the existing LoadTeam pipeline, which
// already performs parse, normalize, preset expansion (inside
// parseAgentContent), merge, static contract validation
// (ValidateTeamTaskContracts/ValidateTeamPolicyContracts), and lint.
//
// Per-dispatch contract compilation/binding (CompileInitialTaskContracts/
// CompileTaskGoalContracts, in contract_compile.go) is a distinct, later
// stage that runs once per coordinator dispatch, not at load time — see the
// "Relationship to Existing EffectiveTaskContract" note in spec.md
// Specification 02 §4. EffectiveTeamSpec does not (and should not) run it
// eagerly; a compiled spec describes the team, not one in-flight dispatch.
//
// Consolidating the tool allow/deny enforcement still scattered across the
// runtime (tool_deny.go, tool_policy_gate.go, coordinator_prompt.go,
// coordinator_run.go, plan_revision.go, and others — see Specification 05
// Phase 4 "Known Scope Risk") into this one merge point is intentionally
// not part of this type either; it remains a distinct, independently
// reviewable follow-up.
type EffectiveTeamSpec struct {
	Name        ResolvedValue[string]
	Description ResolvedValue[string]
	Model       ResolvedValue[string]
	MaxRounds   ResolvedValue[int]
	Timeout     ResolvedValue[int64]
	MaxRetries  ResolvedValue[int]

	Agents map[string]EffectiveAgentSpec

	// Diagnostics is populated by ValidateEffectiveTeam, not by CompileTeam
	// itself, so a caller that only needs the resolved spec (e.g. `team
	// show`) never pays for a lint pass it did not ask for.
	Diagnostics []ContractFinding

	session *TeamSession
}

// RuntimeSession returns the already-loaded runtime session backing this
// spec. This is the sole explicit projection point from the compiled spec
// back into agent.TeamConfig/AgentDef/TaskDef; callers must not reach past
// it to reconstruct runtime state some other way (Specification 02 §7, §9).
func (e *EffectiveTeamSpec) RuntimeSession() *TeamSession {
	if e == nil {
		return nil
	}
	return e.session
}

// CompileTeam parses, normalizes, expands presets, merges, compiles
// contracts, and lints a team directory into one immutable
// EffectiveTeamSpec. It performs no model call — compile failure surfaces
// exactly the same errors LoadTeam already would, before any dispatch.
func CompileTeam(teamDir string, vars map[string]string, forcedSkills []string, registry *ProviderRegistry) (*EffectiveTeamSpec, error) {
	session, err := LoadTeam(teamDir, vars, forcedSkills, registry)
	if err != nil {
		return nil, err
	}
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("resolve team directory: %w", err)
	}
	return newEffectiveTeamSpec(absDir, session)
}

// ValidateEffectiveTeam runs the same static contract/policy/verifier lint
// used by `team validate` against an already-compiled spec, without any
// model call. It deliberately re-runs ValidateTeamTaskContracts/
// ValidateTeamPolicyContracts (which also ran once, error-only, inside
// LoadTeam) so warning-severity findings — silently discarded by LoadTeam
// today — become visible here.
func ValidateEffectiveTeam(spec *EffectiveTeamSpec) []ContractFinding {
	if spec == nil || spec.session == nil {
		return nil
	}
	findings := append(ValidateTeamTaskContracts(spec.session), ValidateTeamPolicyContracts(spec.session)...)
	findings = append(findings, LintTeamContracts(spec.session)...)
	return findings
}

// newEffectiveTeamSpec wraps an already-loaded session with provenance,
// determined by independently re-reading the raw team.yaml/agent
// frontmatter for key presence. This never influences the effective
// configuration itself — LoadTeam's own parse remains solely authoritative
// for values — it only labels where each value came from.
func newEffectiveTeamSpec(absDir string, session *TeamSession) (*EffectiveTeamSpec, error) {
	teamRaw, err := readRawYAMLKeys(absDir, "team.yml", "team.yaml")
	if err != nil {
		return nil, fmt.Errorf("read team.yaml for provenance: %w", err)
	}
	cfg := session.Config

	spec := &EffectiveTeamSpec{session: session}

	if hasNonEmptyKey(teamRaw, "name") {
		spec.Name = ResolvedValue[string]{Value: cfg.Name, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.Name = ResolvedValue[string]{Value: cfg.Name, Source: SourceFilename, Detail: "directory name"}
	}
	if hasNonEmptyKey(teamRaw, "description") {
		spec.Description = ResolvedValue[string]{Value: cfg.Description, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.Description = ResolvedValue[string]{Value: cfg.Description, Source: SourceBuiltin, Detail: "no description authored"}
	}
	if hasNonEmptyKey(teamRaw, "model") {
		spec.Model = ResolvedValue[string]{Value: cfg.Generation.Model, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.Model = ResolvedValue[string]{Value: cfg.Generation.Model, Source: SourceBuiltin, Detail: "built-in default"}
	}
	if hasNonEmptyKey(teamRaw, "max-rounds") {
		spec.MaxRounds = ResolvedValue[int]{Value: cfg.MaxRounds, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.MaxRounds = ResolvedValue[int]{Value: cfg.MaxRounds, Source: SourceBuiltin, Detail: "built-in default"}
	}
	if hasNonEmptyKey(teamRaw, "timeout") {
		spec.Timeout = ResolvedValue[int64]{Value: cfg.Timeout, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.Timeout = ResolvedValue[int64]{Value: cfg.Timeout, Source: SourceBuiltin, Detail: "built-in default"}
	}
	if hasNonEmptyKey(teamRaw, "max-retries") {
		spec.MaxRetries = ResolvedValue[int]{Value: cfg.MaxRetries, Source: SourceTeam, Detail: "team.yaml"}
	} else {
		spec.MaxRetries = ResolvedValue[int]{Value: cfg.MaxRetries, Source: SourceBuiltin, Detail: "built-in default"}
	}

	spec.Agents = make(map[string]EffectiveAgentSpec, len(session.Agents))
	for key, def := range session.Agents {
		if def == nil {
			continue
		}
		agentRaw, hasFile := readAgentRawFrontmatter(absDir, def.FileAlias)

		agentSpec := EffectiveAgentSpec{}
		if hasNonEmptyKey(agentRaw, "name") {
			agentSpec.Name = ResolvedValue[string]{Value: def.Name, Source: SourceAgent, Detail: "explicit frontmatter"}
		} else if hasFile {
			agentSpec.Name = ResolvedValue[string]{Value: def.Name, Source: SourceFilename, Detail: def.FileAlias + ".md"}
		} else {
			agentSpec.Name = ResolvedValue[string]{Value: def.Name, Source: SourceBuiltin, Detail: "built-in agent"}
		}

		if hasNonEmptyKey(agentRaw, "role") {
			agentSpec.Role = ResolvedValue[string]{Value: def.Role, Source: SourceAgent, Detail: "explicit frontmatter"}
		} else if hasFile && strings.EqualFold(def.FileAlias, "coordinator") {
			agentSpec.Role = ResolvedValue[string]{Value: def.Role, Source: SourceFilename, Detail: "coordinator.md convention"}
		} else {
			agentSpec.Role = ResolvedValue[string]{Value: def.Role, Source: SourceBuiltin, Detail: "worker default"}
		}

		var toolsRaw interface{}
		if agentRaw != nil {
			toolsRaw = agentRaw["tools"]
		}
		deniedDetail := ""
		if denied := parseDeniedTools(toolsRaw); len(denied) > 0 {
			deniedDetail = "; denied by agent: " + strings.Join(denied, ",")
		}

		presetName, hasPreset := stringKey(agentRaw, "preset")
		switch {
		case hasPreset:
			agentSpec.Tools = ResolvedValue[string]{Value: def.Tools, Source: SourcePreset, Detail: "preset:" + presetName + deniedDetail}
			agentSpec.SideEffect = ResolvedValue[string]{Value: def.SideEffect, Source: SourcePreset, Detail: "preset:" + presetName}
		case hasNonEmptyKey(agentRaw, "tools"):
			agentSpec.Tools = ResolvedValue[string]{Value: def.Tools, Source: SourceAgent, Detail: "explicit frontmatter" + deniedDetail}
			agentSpec.SideEffect = ResolvedValue[string]{Value: def.SideEffect, Source: SourceBuiltin, Detail: "no side_effect authored"}
		default:
			agentSpec.Tools = ResolvedValue[string]{Value: def.Tools, Source: SourceBuiltin, Detail: "built-in agent"}
			agentSpec.SideEffect = ResolvedValue[string]{Value: def.SideEffect, Source: SourceBuiltin, Detail: "no side_effect authored"}
		}
		if hasNonEmptyKey(agentRaw, "side_effect") {
			agentSpec.SideEffect = ResolvedValue[string]{Value: def.SideEffect, Source: SourceAgent, Detail: "explicit frontmatter"}
		}

		spec.Agents[key] = agentSpec
	}

	return spec, nil
}

// readRawYAMLKeys reads the first existing candidate filename in dir and
// decodes it into a generic map, purely for provenance key-presence checks.
// A missing file (all candidates absent) is not an error: it returns nil.
func readRawYAMLKeys(dir string, candidates ...string) (map[string]interface{}, error) {
	for _, name := range candidates {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var raw map[string]interface{}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			// A parse error here would already have failed LoadTeam; treat
			// as "no provenance available" rather than failing a second time.
			return nil, nil
		}
		return raw, nil
	}
	return nil, nil
}

// readAgentRawFrontmatter reads one agent's raw frontmatter block (if any)
// for provenance key-presence checks. It returns hasFile=false only when
// the underlying .md file itself could not be found (e.g. the built-in
// Helper, which has no on-disk file).
func readAgentRawFrontmatter(teamDir, fileAlias string) (raw map[string]interface{}, hasFile bool) {
	if fileAlias == "" {
		return nil, false
	}
	data, err := os.ReadFile(filepath.Join(teamDir, fileAlias+".md"))
	if err != nil {
		return nil, false
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, true
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return nil, true
	}
	var fm map[string]interface{}
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		return nil, true
	}
	return fm, true
}

// hasNonEmptyKey reports whether key is present in raw with a non-empty
// value. A string value is trimmed before the emptiness check, matching how
// an explicit `name: ""` is treated the same as an omitted name elsewhere
// in this package.
func hasNonEmptyKey(raw map[string]interface{}, key string) bool {
	if raw == nil {
		return false
	}
	v, ok := raw[key]
	if !ok || v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return true
}

// stringKey returns raw[key] as a trimmed string, and whether it was
// present and non-empty.
func stringKey(raw map[string]interface{}, key string) (string, bool) {
	if raw == nil {
		return "", false
	}
	v, ok := raw[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	return s, s != ""
}
