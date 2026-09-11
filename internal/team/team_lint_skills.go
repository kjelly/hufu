package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

type lintSkillCatalog struct {
	production map[string]*skill.SkillDef
	drafts     map[string]*skill.SkillDef
	available  map[string]*skill.SkillDef
}

func buildLintSkillCatalog(session *TeamSession) lintSkillCatalog {
	catalog := lintSkillCatalog{production: make(map[string]*skill.SkillDef), drafts: make(map[string]*skill.SkillDef), available: make(map[string]*skill.SkillDef)}
	if session == nil {
		return catalog
	}
	dirs := teamSkillSearchPaths(session.Dir)
	for _, definition := range skill.DiscoverSkills(dirs, false) {
		catalog.production[normalizedName(definition.Name)] = definition
	}
	for _, definition := range skill.DiscoverSkills(dirs, true) {
		key := normalizedName(definition.Name)
		if definition.Draft {
			catalog.drafts[key] = definition
		}
	}
	for _, definition := range session.Skills {
		catalog.available[normalizedName(definition.Name)] = definition
	}
	return catalog
}

func (c lintSkillCatalog) status(name string) string {
	key := normalizedName(name)
	if c.available[key] != nil {
		return "available"
	}
	if c.production[key] != nil {
		return "out_of_scope"
	}
	if c.drafts[key] != nil {
		return "draft_only"
	}
	return "not_found"
}

func lintOfflineSkills(session *TeamSession, sources *TeamSourceIndex, directives []promptDirective) []TeamLintFinding {
	if session == nil {
		return nil
	}
	catalog := buildLintSkillCatalog(session)
	var findings []TeamLintFinding
	for index, name := range skill.ParseSkillList(session.Config.Skills) {
		field := fmt.Sprintf("skills[%d]", index)
		findings = append(findings, lintRequiredSkill(name, "", field, sources.Location(field), catalog)...)
	}
	for _, def := range authoredAgentDefinitions(session, sources) {
		for index, name := range skill.ParseSkillList(def.Skills) {
			field := fmt.Sprintf("agents.%s.skills[%d]", normalizedName(def.Name), index)
			findings = append(findings, lintRequiredSkill(name, def.Name, field, agentFieldLocation(sources, def, "skills"), catalog)...)
		}
	}
	for index, resource := range session.Config.RequiredResources {
		if resource.Kind != agent.ResourceSkill {
			continue
		}
		field := fmt.Sprintf("required-resources[%d].name", index)
		findings = append(findings, lintRequiredSkill(resource.Name, "", field, sources.Location(field), catalog)...)
	}
	for _, directive := range directives {
		if directive.Kind != promptDirectiveSkill {
			continue
		}
		status := catalog.status(directive.Name)
		switch status {
		case "not_found", "draft_only":
			findings = append(findings, projectedLintFinding(FindingPromptUnknownSkill, FindingSeverityWarning, directive.Agent, directive.FieldPath, status, directive.Location,
				fmt.Sprintf("prompt directs the agent to load skill %q, resolved as %s", directive.Name, status), "publish or include the skill, or remove the directive"))
		case "out_of_scope":
			findings = append(findings, projectedLintFinding(FindingSkillNotAvailable, FindingSeverityError, directive.Agent, directive.FieldPath, status, directive.Location,
				fmt.Sprintf("skill %q exists but is outside the effective team scope", directive.Name), "include the skill for this team or remove the directive"))
		}
	}
	return findings
}

func lintRequiredSkill(name, agentName, field string, loc TeamSourceLocation, catalog lintSkillCatalog) []TeamLintFinding {
	status := catalog.status(name)
	switch status {
	case "not_found", "draft_only":
		return []TeamLintFinding{projectedLintFinding(FindingRequiredSkillMissing, FindingSeverityError, agentName, field, status, loc,
			fmt.Sprintf("required skill %q resolved as %s", strings.TrimSpace(name), status), "publish the skill in a runtime search path")}
	case "out_of_scope":
		return []TeamLintFinding{projectedLintFinding(FindingSkillNotAvailable, FindingSeverityError, agentName, field, status, loc,
			fmt.Sprintf("required skill %q exists but is outside the effective scope", strings.TrimSpace(name)), "update skills/skills-exclude so the skill is available")}
	default:
		return nil
	}
}
