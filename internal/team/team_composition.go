package team

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team/preset"
	"gopkg.in/yaml.v3"
)

const FindingTeamPresetUnknown = "team_preset_unknown"

type compositionError struct{ finding ContractFinding }

func (e *compositionError) Error() string {
	return e.finding.Field + ": " + e.finding.Message + "; " + e.finding.Hint
}

type compositionAgentSource struct {
	file, field, detail, preset string
	origin                      ValueSource
	frontmatter                 agentFrontmatter
}

type compositionAgentCandidate struct {
	def    *agent.AgentDef
	path   string
	source *compositionAgentSource
}

func compositionScalar(node *yaml.Node) (*yaml.Node, error) {
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil, fmt.Errorf("cyclic YAML alias")
		}
		seen[node] = true
		node = node.Alias
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return nil, fmt.Errorf("expected scalar")
	}
	return node, nil
}

// HasTeamComposition uses the same rendered, strict manifest decoder as loading.
// It performs no model, tool, provider, or workspace initialization.
func HasTeamComposition(dir string, vars map[string]string) (bool, error) {
	data, file, found, err := readTeamManifestSource(dir, vars)
	if err != nil || !found {
		return false, err
	}
	cfg, _, err := decodeTeamManifestYAML(file, data)
	if err != nil {
		return false, fmt.Errorf("%s: %w", file, err)
	}
	return cfg.Preset != "" || cfg.Agents != nil, nil
}

func loadTeamComposition(dir string, vars map[string]string) ([]compositionAgentCandidate, error) {
	data, file, found, err := readTeamManifestSource(dir, vars)
	if err != nil || !found {
		return nil, err
	}
	cfg, version, err := decodeTeamManifestYAML(file, data)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if version == SchemaVersionV1Alpha1 {
		prefix = "spec."
	}
	inline, err := normalizeInlineAgents(cfg.Agents, file, prefix, dir)
	if err != nil {
		return nil, err
	}
	if cfg.Preset == "" {
		return inline, nil
	}
	field := "preset"
	if version == SchemaVersionV1Alpha1 {
		field = "spec.preset"
	}
	teamPreset, ok := preset.LookupTeam(cfg.Preset)
	if !ok {
		return nil, &compositionError{finding: ContractFinding{
			Severity: FindingSeverityError, Code: FindingTeamPresetUnknown, Field: field,
			Message: fmt.Sprintf("%s: unknown Team Preset %q", file, cfg.Preset),
			Hint:    "choose one of: " + strings.Join(preset.TeamNames(), ", "),
		}}
	}
	candidates := make([]compositionAgentCandidate, 0, len(teamPreset.Agents))
	for _, declaration := range teamPreset.Agents {
		alias := agentFilenameBase(declaration.FileName)
		fm := agentFrontmatter{Description: declaration.Description, Role: declaration.Role, Preset: declaration.AgentPreset}
		// Filename inference is logical only; no virtual file is read or written.
		def, err := normalizeAgentDefinition(fm, strings.TrimSpace(declaration.System), declaration.FileName, inferAgentRoleFromFilename(declaration.FileName))
		if err != nil {
			return nil, fmt.Errorf("%s:%s: %w", file, field, err)
		}
		if strings.TrimSpace(def.Tools) == "" {
			return nil, fmt.Errorf("%s:%s: preset %q produced an empty tool grant", file, field, teamPreset.Name)
		}
		def.FileAlias = alias
		source := &compositionAgentSource{file: file, field: field, detail: "team-preset:" + teamPreset.Name + "/" + alias, preset: declaration.AgentPreset, origin: SourcePreset, frontmatter: fm}
		candidates = append(candidates, compositionAgentCandidate{def: def, path: filepath.Join(dir, file) + ":" + field + "[" + alias + "]", source: source})
	}
	return append(candidates, inline...), nil
}

// IsComposedAgent distinguishes manifest declarations from authored Markdown.
// It is a source query, not a runtime authorization decision.
func (s *TeamSession) IsComposedAgent(identity string) bool {
	if s == nil {
		return false
	}
	def := s.Agents[normalizedName(identity)]
	if def == nil {
		return false
	}
	_, exists := s.compositionSources[normalizedName(def.FileAlias)]
	return exists
}

func (s *TeamSession) addCompositionSourceIndex(index *TeamSourceIndex) {
	if index == nil {
		return
	}
	for alias, source := range s.compositionSources {
		def := s.Agents[alias]
		if def == nil {
			continue
		}
		loc := index.Location(source.field)
		// The declaration is exact; generated prompt text has no authored span.
		bodyLocation := loc
		bodyLocation.Status = LocationFieldOnly
		agentSource := AgentSource{File: source.file, Body: def.System, BodyField: source.field, BodyStatus: LocationFieldOnly, Fields: map[string]TeamSourceLocation{
			"name": loc, "role": loc, "tools": loc, "preset": loc, "side_effect": loc,
			"system": bodyLocation,
		}}
		if source.origin == SourceTeam {
			agentSource.FieldPath = source.field
			for field, location := range index.Manifest {
				if leaf, ok := strings.CutPrefix(field, source.field+"."); ok {
					agentSource.Fields[leaf] = location
				}
			}
			if location, exists := index.Manifest[source.field+".instructions"]; exists {
				agentSource.BodyField = source.field + ".instructions"
				// Point prompt findings to the owning scalar, including folded or escaped YAML.
				// A decoded prompt does not have a generally exact per-character YAML span.
				if text, exists := index.ManifestText[agentSource.BodyField]; exists {
					location.Status = text.Status
				}
				agentSource.Fields["system"] = location
			}
		}
		index.Agents[def.FileAlias] = agentSource
	}
}

func (source compositionAgentSource) effectiveAgent(def *agent.AgentDef) EffectiveAgentSpec {
	detail := source.detail + "; " + source.file + ":" + source.field
	toolsDetail := detail + "; preset:" + source.preset
	if denied := parseDeniedTools(source.frontmatter.Tools); len(denied) > 0 {
		toolsDetail += "; denied by agent: " + strings.Join(denied, ",")
	}
	return EffectiveAgentSpec{
		Name:       ResolvedValue[string]{Value: def.Name, Source: source.origin, Detail: detail},
		Role:       ResolvedValue[string]{Value: def.Role, Source: source.origin, Detail: detail},
		Tools:      ResolvedValue[string]{Value: def.Tools, Source: SourcePreset, Detail: toolsDetail},
		SideEffect: ResolvedValue[string]{Value: def.SideEffect, Source: SourcePreset, Detail: toolsDetail},
	}
}

func normalizeInlineAgents(agents *teamAgentDeclarations, file, prefix, dir string) ([]compositionAgentCandidate, error) {
	if agents == nil {
		return nil, nil
	}
	candidates := make([]compositionAgentCandidate, 0, len(*agents))
	for _, id := range slices.Sorted(maps.Keys(*agents)) {
		declaration := (*agents)[id]
		field := prefix + "agents." + id
		invalid := func(code, leaf, message, hint string) error {
			errorField := field
			if leaf != "" {
				errorField += "." + leaf
			}
			return &compositionError{finding: ContractFinding{Severity: FindingSeverityError, Code: code, Field: errorField, Message: file + ": " + message, Hint: hint}}
		}
		key := normalizedName(id)
		if key == "helper" || strings.Contains(key, "coordinat") || strings.Contains(key, "orchestr") {
			return nil, invalid(FindingInlineAgentInvalid, "", "reserved inline worker identifier "+id, "choose another worker identifier; use coordinator.md for a custom coordinator")
		}
		fm := declaration.frontmatter
		if _, ok := preset.Lookup(fm.Preset); !ok {
			leaf := ""
			if declaration.node.Kind == yaml.MappingNode {
				leaf = "preset"
			}
			return nil, invalid(FindingInlineAgentInvalid, leaf, fmt.Sprintf("unknown Agent Preset %q", fm.Preset), "choose one of: "+strings.Join(preset.Names(), ", "))
		}
		fm.Name, fm.Role = id, "worker"
		if fm.Description == "" {
			fm.Description = id
		}
		body := declaration.instructions
		if !declaration.hasInstructions {
			body = "Perform the " + id + " role for this team."
		}
		def, err := normalizeAgentDefinition(fm, strings.TrimSpace(body), file+":"+field, "worker")
		if err != nil {
			return nil, invalid(FindingInlineAgentInvalid, "", err.Error(), "correct the inline fields or use Markdown for advanced contracts")
		}
		if strings.TrimSpace(def.Tools) == "" {
			return nil, invalid(FindingInlineAgentToolsEmpty, "tools", "inline worker has no remaining tools; empty grants must not reach the wildcard selector", "retain at least one concrete tool from the Agent Preset")
		}
		def.FileAlias = id
		source := &compositionAgentSource{file: file, field: field, detail: "inline:" + file + ":" + field, preset: fm.Preset, origin: SourceTeam, frontmatter: fm}
		candidates = append(candidates, compositionAgentCandidate{def: def, path: filepath.Join(dir, file) + ":" + field, source: source})
	}
	return candidates, nil
}
