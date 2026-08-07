package skill

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// skillReferenceRE finds repository- or home-relative references to a skill
// file. References are resolved against the already discovered skill catalog;
// this prevents a skill document from expanding the file-reading scope to an
// arbitrary path.
var skillReferenceRE = regexp.MustCompile(`(?:\$\{HOME\}|\$HOME|~|\.{0,2})?/?(?:[A-Za-z0-9_.-]+/)*skills/[A-Za-z0-9_.-]+/SKILL\.md`)

// ExpandSkillDependencies returns root followed by every skill referenced by
// root, recursively. The catalog is authoritative: references to files that
// were not discovered through the configured skill roots are ignored.
func ExpandSkillDependencies(root *SkillDef, catalog []*SkillDef) []*SkillDef {
	return ExpandSkillDependenciesForSet([]*SkillDef{root}, catalog, nil)
}

// ExpandSkillDependenciesForSet returns the selected skills plus their
// transitive references. Explicitly excluded names always win over an
// implicit dependency.
func ExpandSkillDependenciesForSet(selected, catalog []*SkillDef, excluded []string) []*SkillDef {
	if len(selected) == 0 {
		return nil
	}

	excludeSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(name))] = true
	}
	seen := make(map[string]bool)
	result := make([]*SkillDef, 0, len(selected))
	var visit func(*SkillDef)
	visit = func(current *SkillDef) {
		if current == nil {
			return
		}
		if excludeSet[strings.ToLower(current.Name)] {
			return
		}
		key := strings.ToLower(current.Name)
		if key == "" {
			key = strings.ToLower(filepath.Clean(current.Path))
		}
		if seen[key] {
			return
		}
		seen[key] = true
		result = append(result, current)

		for _, reference := range referencedSkillPaths(current.Content) {
			if dependency := resolveSkillReference(reference, catalog); dependency != nil {
				visit(dependency)
			}
		}
	}

	for _, root := range selected {
		visit(root)
	}
	return result
}

func referencedSkillPaths(content string) []string {
	matches := skillReferenceRE.FindAllString(content, -1)
	seen := make(map[string]bool, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		match = strings.TrimRight(match, ".,;:")
		if match == "" || seen[match] {
			continue
		}
		seen[match] = true
		result = append(result, match)
	}
	return result
}

func resolveSkillReference(reference string, catalog []*SkillDef) *SkillDef {
	refPath := expandSkillPath(reference)
	refSlash := filepath.ToSlash(strings.TrimPrefix(refPath, "./"))
	refName := skillNameFromReference(reference)

	for _, candidate := range catalog {
		if candidate == nil {
			continue
		}
		candidatePath, err := filepath.Abs(candidate.Path)
		if err == nil {
			candidateSlash := filepath.ToSlash(filepath.Clean(candidatePath))
			if refSlash == candidateSlash || strings.HasSuffix(candidateSlash, "/"+refSlash) {
				return candidate
			}
		}
		if refName != "" && strings.EqualFold(candidate.Name, refName) {
			return candidate
		}
	}
	return nil
}

func expandSkillPath(path string) string {
	expanded := os.ExpandEnv(path)
	if strings.HasPrefix(expanded, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~"))
		}
	}
	if abs, err := filepath.Abs(expanded); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(expanded)
}

func skillNameFromReference(reference string) string {
	path := filepath.ToSlash(strings.TrimRight(reference, "/"))
	const suffix = "/SKILL.md"
	if !strings.HasSuffix(path, suffix) {
		return ""
	}
	path = strings.TrimSuffix(path, suffix)
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[len(parts)-2] != "skills" {
		return ""
	}
	return parts[len(parts)-1]
}
