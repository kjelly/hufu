package team

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

const (
	FindingLegacyExecutionProvider = "legacy_execution_provider_field"
	FindingUnknownTaskAgent        = "unknown_task_agent"
	FindingDependencyCycle         = "dependency_cycle"
)

// TeamLintFinding is the stable, source-located projection exposed by the CLI.
type TeamLintFinding struct {
	Code           string `json:"code"`
	Severity       string `json:"severity"`
	File           string `json:"file,omitempty"`
	Line           int    `json:"line,omitzero"`
	Column         int    `json:"column,omitzero"`
	LocationStatus string `json:"location_status"`
	Agent          string `json:"agent,omitempty"`
	FieldPath      string `json:"field_path,omitempty"`
	Resolution     string `json:"resolution,omitempty"`
	Message        string `json:"message"`
	Suggestion     string `json:"suggestion,omitempty"`
	Ignored        bool   `json:"ignored,omitzero"`
}

// TeamLintResult is the package-level output before CLI formatting.
type TeamLintResult struct {
	Team     string
	Complete bool
	Findings []TeamLintFinding
}

// TeamLintOptions carries invocation-scoped policy overrides. Pointer booleans
// preserve an explicit --flag=false, which must override a true manifest value.
type TeamLintOptions struct {
	Vars             map[string]string
	ForcedSkills     []string
	Registry         *ProviderRegistry
	Unattended       *bool
	NoNet            *bool
	ForceMCP         *bool
	PlanFirst        *bool
	ExecutionProfile string
}

// LintTeam runs the deterministic offline rules with manifest policy only.
func LintTeam(teamDir string, vars map[string]string, forcedSkills []string, registry *ProviderRegistry) (TeamLintResult, error) {
	return LintTeamWithOptions(teamDir, TeamLintOptions{Vars: vars, ForcedSkills: forcedSkills, Registry: registry})
}

// LintTeamWithOptions runs the complete offline lint pipeline with resolved
// invocation policy and without creating runtime services.
func LintTeamWithOptions(teamDir string, options TeamLintOptions) (TeamLintResult, error) {
	registry := options.Registry
	if registry == nil {
		registry = DefaultProviderRegistry
	}
	inspection, err := InspectTeam(teamDir, options.Vars, options.ForcedSkills, registry, TeamCompileLint)
	if err != nil {
		return TeamLintResult{}, err
	}
	result := TeamLintResult{Team: filepath.Base(filepath.Clean(teamDir)), Complete: inspection.Complete, Findings: make([]TeamLintFinding, 0)}
	if inspection.Session != nil {
		policy, policyErr := resolveTeamLintPolicy(inspection.Session, options)
		if policyErr != nil {
			return TeamLintResult{}, policyErr
		}
		result.Team = inspection.Session.Config.Name
		inspection.Diagnostics = append(inspection.Diagnostics, LintTeamContracts(inspection.Session)...)
		inspection.Diagnostics = append(inspection.Diagnostics, LintEffectiveTeamContracts(inspection.Session, policy)...)
		inspection.Diagnostics = append(inspection.Diagnostics, lintStaticTopology(inspection.Session, inspection.Diagnostics)...)
		inspection.Diagnostics = append(inspection.Diagnostics, lintLegacyExecutionFields(inspection)...)
		inspection.Diagnostics = append(inspection.Diagnostics, lintRuntimeSemantics(inspection.Session, policy, inspection.Diagnostics)...)
		directives := scanTeamPromptDirectives(inspection.Session, inspection.Sources)
		result.Findings = append(result.Findings, lintOfflineTools(inspection.Session, inspection.Sources, policy, directives)...)
		result.Findings = append(result.Findings, lintOfflineSkills(inspection.Session, inspection.Sources, directives)...)
	}
	result.Findings = append(result.Findings, ProjectContractFindings(inspection.Diagnostics, inspection.Sources, inspection.Session)...)
	SortTeamLintFindings(result.Findings)
	return result, nil
}

func resolveTeamLintPolicy(session *TeamSession, options TeamLintOptions) (EffectiveTeamContractContext, error) {
	profile, err := ResolveExecutionProfile(options.ExecutionProfile, session.Config.ExecutionProfile)
	if err != nil {
		return EffectiveTeamContractContext{}, err
	}
	policy := EffectiveTeamContractContext{
		Unattended: session.Config.Unattended || profile.IsUnattended(),
		NoNet:      session.Config.NoNet, ForceMCP: session.Config.ForceMCP,
		PlanFirst: options.PlanFirst, ExecutionProfile: profile, Resolved: true,
	}
	if options.Unattended != nil {
		policy.Unattended = *options.Unattended
	}
	if options.NoNet != nil {
		policy.NoNet = *options.NoNet
	}
	if options.ForceMCP != nil {
		policy.ForceMCP = *options.ForceMCP
	}
	return policy, nil
}

// ProjectContractFindings is the sole adapter from runtime diagnostics to the
// public lint schema.
func ProjectContractFindings(findings []ContractFinding, sources *TeamSourceIndex, session *TeamSession) []TeamLintFinding {
	projected := make([]TeamLintFinding, 0, len(findings))
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		key := finding.Code + "\x00" + finding.Field + "\x00" + finding.Message
		if seen[key] {
			continue
		}
		seen[key] = true
		loc, agentName := findingSourceLocation(finding.Field, sources, session)
		projected = append(projected, TeamLintFinding{
			Code: finding.Code, Severity: finding.Severity,
			File: loc.File, Line: loc.Line, Column: loc.Column, LocationStatus: loc.Status,
			Agent: agentName, FieldPath: finding.Field, Message: finding.Message, Suggestion: finding.Hint,
		})
	}
	return projected
}

func findingSourceLocation(field string, sources *TeamSourceIndex, session *TeamSession) (TeamSourceLocation, string) {
	if sources == nil {
		return TeamSourceLocation{Status: LocationFieldOnly}, ""
	}
	if rest, ok := strings.CutPrefix(field, "agents."); ok {
		identity, agentField, found := strings.Cut(rest, ".")
		if !found {
			return TeamSourceLocation{Status: LocationFieldOnly}, identity
		}
		alias := identity
		agentName := identity
		if session != nil {
			if def := session.Agents[normalizedName(identity)]; def != nil {
				alias = def.FileAlias
				agentName = def.Name
			}
		}
		if source, ok := sources.Agents[alias]; ok {
			if loc, ok := source.Fields[agentField]; ok {
				return loc, agentName
			}
			return TeamSourceLocation{File: source.File, Status: LocationFieldOnly}, agentName
		}
		return TeamSourceLocation{Status: LocationFieldOnly}, agentName
	}
	loc := sources.Location(field)
	return loc, ""
}

func lintStaticTopology(session *TeamSession, existing []ContractFinding) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	for index, task := range session.ContractTasks {
		field := fmt.Sprintf("tasks[%d].agent", index)
		if session.Agents[normalizedName(task.Agent)] == nil && !hasFindingForField(existing, field, "initial_contract_agent_unknown", "goal_contract_agent_unknown") {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError, Code: FindingUnknownTaskAgent, Field: field,
				Message: fmt.Sprintf("static task agent %q is not a loaded worker", task.Agent),
			})
		}
	}
	if detectTaskCycle(session.ContractTasks) {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError, Code: FindingDependencyCycle, Field: "tasks",
			Message: "static tasks contain a dependency cycle",
		})
	}
	return findings
}

func hasFindingForField(findings []ContractFinding, field string, codes ...string) bool {
	return slices.ContainsFunc(findings, func(finding ContractFinding) bool {
		return finding.Field == field && slices.Contains(codes, finding.Code)
	})
}

func lintLegacyExecutionFields(inspection *TeamInspection) []ContractFinding {
	if inspection == nil || inspection.Sources == nil || inspection.Session == nil {
		return nil
	}
	prefix := ""
	if inspection.Sources.SchemaVersion == SchemaVersionV1Alpha1 {
		prefix = "spec."
	}
	manifest := inspection.Sources.Manifest
	var findings []ContractFinding
	if _, authored := manifest[prefix+"model"]; authored {
		_, workerAuthored := manifest[prefix+"worker-model"]
		_, coordinatorAuthored := manifest[prefix+"coordinator-model"]
		if !workerAuthored || !coordinatorAuthored {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityWarning, Code: FindingLegacyExecutionProvider, Field: prefix + "model",
				Message: "shared model is a legacy execution selector when role-specific selectors are omitted",
				Hint:    "set worker-model and coordinator-model explicitly",
			})
		}
	}
	for _, entry := range []struct{ field, replacement string }{{"providers", "backends"}, {"subagent-providers", "backends"}} {
		if _, authored := manifest[prefix+entry.field]; authored {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityWarning, Code: FindingLegacyExecutionProvider, Field: prefix + entry.field,
				Message: fmt.Sprintf("%s is a legacy execution provider field", entry.field),
				Hint:    "migrate this declaration to " + entry.replacement,
			})
		}
	}
	return findings
}

// SortTeamLintFindings applies the public deterministic ordering contract.
func SortTeamLintFindings(findings []TeamLintFinding) {
	slices.SortFunc(findings, func(a, b TeamLintFinding) int {
		if n := cmp.Compare(a.File, b.File); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Line, b.Line); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Column, b.Column); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Code, b.Code); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Agent, b.Agent); n != 0 {
			return n
		}
		if n := cmp.Compare(a.FieldPath, b.FieldPath); n != 0 {
			return n
		}
		return cmp.Compare(a.Message, b.Message)
	})
}

// TeamLintReachesThreshold reports whether an unignored finding blocks the
// requested severity threshold.
func TeamLintReachesThreshold(findings []TeamLintFinding, threshold string) bool {
	rank := map[string]int{FindingSeverityInfo: 1, FindingSeverityWarning: 2, FindingSeverityError: 3}
	want, ok := rank[threshold]
	if threshold == "none" {
		return false
	}
	if !ok {
		return false
	}
	return slices.ContainsFunc(findings, func(finding TeamLintFinding) bool {
		return !finding.Ignored && rank[finding.Severity] >= want
	})
}
