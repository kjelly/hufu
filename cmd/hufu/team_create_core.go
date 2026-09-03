package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	internalteam "github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/team/preset"
)

// templateToTeamPreset maps hufu init's legacy --template values onto the
// new team preset names (spec.md Specification 01 §8a). "minimal" has no
// team preset: it maps to an empty file set (Level 1 authoring, no
// scaffolded agents), not to a default worker.
var templateToTeamPreset = map[string]string{
	"default":  "coding-single",
	"dev":      "coding-reviewed",
	"research": "research",
	"ops":      "safe-ops",
	"minimal":  "",
}

// templateNames returns the legacy --template values, sorted, for error
// messages.
func templateNames() []string {
	names := make([]string, 0, len(templateToTeamPreset))
	for name := range templateToTeamPreset {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolvePresetFiles resolves a --preset value into the agent files a new
// team scaffolds: a team preset (Specification 01 §8) is tried first, then
// a single-worker agent preset (Specification 01 §6) as a fallback. An
// empty name resolves to no files at all — this is exactly what `hufu init
// --template minimal` must produce; a caller that wants a default
// single-worker team when nothing was requested should resolve
// "coding-single" explicitly rather than passing an empty name through
// unexamined.
func resolvePresetFiles(presetName string) (map[string]string, error) {
	name := strings.TrimSpace(presetName)
	if name == "" {
		return map[string]string{}, nil
	}
	if teamPreset, ok := preset.LookupTeam(name); ok {
		return teamPreset.Render(), nil
	}
	if agentPreset, ok := preset.Lookup(name); ok {
		return map[string]string{
			"worker.md": fmt.Sprintf("---\ndescription: General-purpose worker\npreset: %s\n---\nImplement the requested change end to end. Prefer reading existing files before editing them, keep changes minimal and idiomatic, and report concisely what you did.\n", agentPreset.Name),
		}, nil
	}
	return nil, fmt.Errorf("unknown --preset %q: available team presets are %s; available agent presets are %s", presetName, strings.Join(preset.TeamNames(), ", "), strings.Join(preset.Names(), ", "))
}

// applyExpandedDefaults prepends the built-in defaults --expanded pins
// (spec.md Specification 03 §9) to an existing team.yaml body, so a
// preference for configuration pinning does not silently drop values
// buildGeneratedTeam or resolvePresetFiles already put there (e.g. model).
func applyExpandedDefaults(teamYAML string) string {
	const defaults = "max-rounds: 10\ntimeout: 600\nmax-retries: 2\nworkspace: workspace\n"
	return defaults + teamYAML
}

// writeTeamFilesWithValidation writes files (agent Markdown plus an
// optional "team.yaml") into teamDir, refusing to touch an existing
// directory unless force is set, then compiles and validates the result.
//
// On failure, teamDir is removed again only when this call created it
// fresh — never when it already existed (a --force overwrite of a
// pre-existing directory must not risk deleting content unrelated to this
// write on validation failure; only the files this call wrote are at
// stake). This is the "generated teams validate automatically" /
// "rollback the newly written file if validation fails" guarantee applied
// to team creation (Specification 05 Phase 6, Specification 03 §5).
func writeTeamFilesWithValidation(teamDir string, files map[string]string, force bool) error {
	preexisting := false
	if _, err := os.Stat(teamDir); err == nil {
		preexisting = true
		if !force {
			return fmt.Errorf("team directory %s already exists; pass --force to overwrite", teamDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect team directory %s: %w", teamDir, err)
	}

	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return fmt.Errorf("create team directory: %w", err)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(teamDir, name), []byte(files[name]), 0o644); err != nil {
			if !preexisting {
				_ = os.RemoveAll(teamDir)
			}
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	if len(names) == 0 {
		// hufu init --template minimal: no agents were requested, so there
		// is nothing to compile/validate yet (Level 1 authoring expects the
		// user to add agent files afterward).
		return nil
	}

	if _, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry); err != nil {
		if !preexisting {
			_ = os.RemoveAll(teamDir)
		}
		return fmt.Errorf("generated team is invalid: %w", err)
	}
	return nil
}
