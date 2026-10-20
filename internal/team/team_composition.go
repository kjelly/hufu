package team

import (
	"fmt"
	"path/filepath"
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
	return cfg.Preset != "", nil
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
	if cfg.Preset == "" {
		return nil, nil
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
	return candidates, nil
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
		index.Agents[def.FileAlias] = AgentSource{File: source.file, Body: def.System, BodyStatus: LocationFieldOnly, Fields: map[string]TeamSourceLocation{
			"name": loc, "role": loc, "tools": loc, "preset": loc, "side_effect": loc,
		}}
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
