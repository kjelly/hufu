package improver

import (
	"maps"
	"path"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// validateRenderedTree closes the gap between authored YAML validation and the
// loader's template pass. A variable cannot inject an unsupported capability or
// an escaping schema reference before that reference is read.
func validateRenderedTree(tree Tree, overrides map[string]string) error {
	rendered := Tree{files: maps.Clone(tree.files)}
	vars := maps.Clone(overrides)
	if vars == nil {
		vars = map[string]string{}
	}
	teamName := "team" // The materializer's registered logical team directory.
	found := false
	for _, name := range []string{"team.yml", "team.yaml"} {
		raw, exists := tree.files[name]
		if !exists {
			continue
		}
		text, effectiveVars, err := team.RenderTeamManifest(raw, name, overrides)
		if err != nil {
			return err
		}
		rendered.files[name] = text
		if found {
			continue
		}
		found = true
		vars = effectiveVars
		object, err := yamlObject(text)
		if err != nil {
			return err
		}
		spec, err := manifestSpec(object)
		if err != nil {
			return err
		}
		if value, ok := spec["name"].(string); ok && value != "" {
			teamName = value
		}
		if metadata, ok := object["metadata"].(map[string]any); ok {
			if value, ok := metadata["name"].(string); ok && value != "" {
				teamName = value
			}
		}
	}
	if _, exists := vars["TEAM_NAME"]; !exists {
		vars["TEAM_NAME"] = teamName
	}
	for name, raw := range tree.files {
		if path.Dir(name) != "." || !strings.HasSuffix(name, ".md") {
			continue
		}
		text, err := team.RenderTemplateText(string(raw), name, vars)
		if err != nil {
			return err
		}
		rendered.files[name] = []byte(text)
	}
	return validateAuthoredTree(rendered)
}
