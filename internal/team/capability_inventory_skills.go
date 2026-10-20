package team

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func capabilitySkills(session *TeamSession) []StaticCapability {
	dirs := teamSkillSearchPaths(session.Dir)
	selected := skill.DiscoverSkills(dirs, false)
	sources := make([][]*skill.SkillDef, len(dirs))
	for i, dir := range dirs {
		sources[i] = skill.DiscoverSkills([]string{dir}, false)
	}
	active := map[string]bool{}
	for _, def := range session.Skills {
		active[normalizedName(def.Name)] = true
	}
	var result []StaticCapability
	seen := map[string]bool{}
	for _, def := range selected {
		row := capabilitySkill(def)
		key := normalizedName(def.Name)
		seen[key] = true
		if active[key] {
			row.Availability = "available"
		} else {
			row.Availability = "unavailable"
			row.Diagnostics = append(row.Diagnostics, CapabilityDiagnostic{FindingSkillNotAvailable, "info"})
		}
		chosen := false
		for i, candidates := range sources {
			for _, candidate := range candidates {
				if normalizedName(candidate.Name) == key {
					isSelected := !chosen && candidate.Path == def.Path
					row.Sources = append(row.Sources, CapabilityInventorySource{Scope: capabilitySkillScope(i), Selected: isSelected})
					if isSelected {
						chosen = true
						row.Owner = capabilitySkillScope(i)
					}
				}
			}
		}
		result = append(result, row)
	}
	// Draft discovery is separate: a higher-priority draft cannot hide a
	// lower-priority production skill, and never enters session.Skills.
	for i, dir := range dirs {
		for _, def := range skill.DiscoverSkills([]string{filepath.Join(dir, "drafts")}, false) {
			row := capabilitySkill(def)
			row.ID, row.Owner, row.Availability = "draft:"+capabilityLabel(def.Name), capabilitySkillScope(i), "draft"
			row.Sources = []CapabilityInventorySource{{Scope: row.Owner, Draft: true}}
			result = append(result, row)
		}
	}
	required := skill.ParseSkillList(session.Config.Skills)
	for _, def := range session.Agents {
		required = append(required, skill.ParseSkillList(def.Skills)...)
	}
	for _, resource := range session.Config.RequiredResources {
		if resource.Kind == agent.ResourceSkill {
			required = append(required, resource.Name)
		}
	}
	slices.Sort(required)
	for _, name := range required {
		key := normalizedName(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		row := capabilityBase("skill", "team", "missing:"+name, name)
		row.Availability = "unavailable"
		row.Diagnostics = []CapabilityDiagnostic{{FindingRequiredSkillMissing, "error"}}
		result = append(result, row)
	}
	return result
}

func capabilitySkill(def *skill.SkillDef) StaticCapability {
	row := capabilityBase("skill", "team", def.Name, def.Name)
	if digest, err := skill.ContentDigest(def); err == nil {
		row.ContentDigest = new(digest)
	}
	if strings.TrimSpace(def.AllowedTools) != "" {
		row.DeclaredTools = &CapabilityToolDeclaration{Allowed: capabilityNames(skill.ParseSkillList(def.AllowedTools))}
	}
	return row
}

func capabilitySkillScope(index int) string {
	switch index {
	case 0:
		return "team"
	case 1:
		return "project"
	default:
		return "user"
	}
}
