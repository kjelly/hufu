package team

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// EffectiveTeamContractContext is the runtime policy snapshot after config,
// profile, and CLI overrides have been merged. Nil EnvironmentLookup or an
// empty AllowedPaths slice disables only that environment-dependent check.
type EffectiveTeamContractContext struct {
	Unattended        bool
	ForceMCP          bool
	NoNet             bool
	PlanFirst         *bool
	AllowedPaths      []string
	EnvironmentLookup func(string) (string, bool)
}

// ValidateTeamPolicyContracts checks domain-neutral team structure and
// machine-readable requirements. It never interprets coordinator/worker prose.
func ValidateTeamPolicyContracts(session *TeamSession) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	findings = append(findings, validateDelegationReferences(session)...)
	findings = append(findings, validateToolPolicy(session.Config.ToolsAllowed, session.Config.ToolsDenied)...)
	findings = append(findings, validateRequirements("requires", session.Config.Requirements)...)

	workers := reachableWorkers(session)
	for _, def := range workers {
		field := fmt.Sprintf("agents.%s.requires", normalizedName(def.Name))
		findings = append(findings, validateRequirements(field, def.Requirements)...)
		findings = append(findings, validateRequiredTools(field, def.Requirements.Tools, def, session.Config)...)
	}
	findings = append(findings, validateTeamRequiredTools(session.Config.Requirements.Tools, workers, session.Config)...)
	return findings
}

// LintEffectiveTeamContracts checks requirements against the resolved runtime
// policy. Static findings are produced separately by ValidateTeamPolicyContracts.
func LintEffectiveTeamContracts(session *TeamSession, ctx EffectiveTeamContractContext) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	forceMCP := ctx.ForceMCP || session.Config.ForceMCP
	noNet := ctx.NoNet || session.Config.NoNet
	unattended := ctx.Unattended || session.Config.Unattended
	findings = append(findings, lintEffectiveRequirements("requires", session.Config.Requirements, nil, unattended, noNet, ctx.PlanFirst, forceMCP, ctx)...)
	for _, def := range reachableWorkers(session) {
		field := fmt.Sprintf("agents.%s.requires", normalizedName(def.Name))
		findings = append(findings, lintEffectiveRequirements(field, def.Requirements, def, unattended, noNet || def.NoNet, ctx.PlanFirst, forceMCP || def.ForceMCP, ctx)...)
	}
	return findings
}

func validateDelegationReferences(session *TeamSession) []ContractFinding {
	policy := session.Config.Delegation
	var findings []ContractFinding
	allowed := make(map[string]bool)
	for index, name := range policy.AllowedWorkers {
		key := normalizedName(name)
		field := fmt.Sprintf("delegation.allowed-workers[%d]", index)
		findings = append(findings, validateWorkerReference(session, field, name)...)
		if key != "" {
			if allowed[key] {
				findings = append(findings, errorFinding(field, FindingRequirementInvalid, fmt.Sprintf("worker %q is listed more than once", name)))
			}
			allowed[key] = true
		}
	}
	seenInitial := make(map[string]bool)
	for index, name := range policy.InitialBatch {
		key := normalizedName(name)
		field := fmt.Sprintf("delegation.initial-batch.agents[%d]", index)
		findings = append(findings, validateWorkerReference(session, field, name)...)
		if seenInitial[key] && key != "" {
			findings = append(findings, errorFinding(field, FindingRequirementInvalid, fmt.Sprintf("worker %q appears more than once in the initial batch", name)))
		}
		seenInitial[key] = true
		if len(allowed) > 0 && key != "" && !allowed[key] {
			findings = append(findings, errorFinding(field, FindingDelegationWorkerDenied, fmt.Sprintf("initial worker %q is excluded by delegation.allowed-workers", name)))
		}
	}
	for index, name := range policy.NoRedispatchAfterSuccess {
		field := fmt.Sprintf("delegation.no-redispatch-after-success[%d]", index)
		findings = append(findings, validateWorkerReference(session, field, name)...)
	}
	return findings
}

func validateWorkerReference(session *TeamSession, field, name string) []ContractFinding {
	key := normalizedName(name)
	if key == "" {
		return []ContractFinding{errorFinding(field, FindingDelegationWorkerUnknown, "worker name must not be empty")}
	}
	def := session.Agents[key]
	if def == nil {
		return []ContractFinding{errorFinding(field, FindingDelegationWorkerUnknown, fmt.Sprintf("worker %q is not defined by this team", name))}
	}
	if role := normalizedName(def.Role); role == "coordinator" || role == "orchestrator" {
		return []ContractFinding{errorFinding(field, FindingDelegationWorkerRole, fmt.Sprintf("agent %q has role %q and cannot be delegated as a worker", name, def.Role))}
	}
	return nil
}

func validateToolPolicy(allowed, denied []string) []ContractFinding {
	allowedSet := normalizedSet(allowed)
	var findings []ContractFinding
	for index, name := range denied {
		key := normalizedName(name)
		if key != "" && allowedSet[key] {
			findings = append(findings, errorFinding(fmt.Sprintf("tools.denied[%d]", index), FindingToolPolicyConflict, fmt.Sprintf("tool %q is both allowed and denied", name)))
		}
	}
	return findings
}

func validateRequirements(field string, req agent.ContractRequirements) []ContractFinding {
	var findings []ContractFinding
	groups := []struct {
		label  string
		values []string
	}{{"tools", req.Tools}, {"environment", req.Environment}, {"paths", req.Paths}}
	for _, group := range groups {
		seen := make(map[string]bool)
		for index, value := range group.values {
			key := normalizedName(value)
			entryField := fmt.Sprintf("%s.%s[%d]", field, group.label, index)
			if key == "" {
				findings = append(findings, errorFinding(entryField, FindingRequirementInvalid, "requirement must not be empty"))
			} else if seen[key] {
				findings = append(findings, errorFinding(entryField, FindingRequirementInvalid, fmt.Sprintf("requirement %q is duplicated", value)))
			}
			seen[key] = true
		}
	}
	return findings
}

func validateRequiredTools(field string, required []string, def *agent.AgentDef, cfg agent.TeamConfig) []ContractFinding {
	denied := normalizedSet(cfg.ToolsDenied)
	declared, unconstrained := declaredWorkerTools(def)
	var findings []ContractFinding
	for index, name := range required {
		key := normalizedName(name)
		entryField := fmt.Sprintf("%s.tools[%d]", field, index)
		if denied[key] {
			findings = append(findings, errorFinding(entryField, FindingRequiredToolDenied, fmt.Sprintf("required tool %q is denied by team policy", name)))
		} else if !unconstrained && !declared[key] {
			findings = append(findings, errorFinding(entryField, FindingRequiredToolUnavailable, fmt.Sprintf("required tool %q is not declared by worker %q", name, def.Name)))
		}
	}
	return findings
}

func validateTeamRequiredTools(required []string, workers []*agent.AgentDef, cfg agent.TeamConfig) []ContractFinding {
	denied := normalizedSet(cfg.ToolsDenied)
	var findings []ContractFinding
	for index, name := range required {
		key := normalizedName(name)
		field := fmt.Sprintf("requires.tools[%d]", index)
		if denied[key] {
			findings = append(findings, errorFinding(field, FindingRequiredToolDenied, fmt.Sprintf("required team tool %q is denied by team policy", name)))
			continue
		}
		available := false
		for _, def := range workers {
			declared, unconstrained := declaredWorkerTools(def)
			if unconstrained || declared[key] {
				available = true
				break
			}
		}
		if !available {
			findings = append(findings, errorFinding(field, FindingRequiredToolUnavailable, fmt.Sprintf("required team tool %q is unavailable to every reachable worker", name)))
		}
	}
	return findings
}

func lintEffectiveRequirements(field string, req agent.ContractRequirements, def *agent.AgentDef, unattended, noNet bool, planFirst *bool, forceMCP bool, ctx EffectiveTeamContractContext) []ContractFinding {
	var findings []ContractFinding
	if req.Interactive && unattended {
		findings = append(findings, errorFinding(field+".interactive", FindingInteractiveUnattended, "interactive execution is required but the effective run is unattended"))
	}
	if req.Network && noNet {
		findings = append(findings, errorFinding(field+".network", FindingNetworkDisabled, "network access is required but no-net is enabled"))
	}
	if req.PlanFirst && planFirst != nil && !*planFirst {
		findings = append(findings, errorFinding(field+".plan-first", FindingPlanFirstRequired, "plan-first execution is required but is not enabled"))
	}
	if forceMCP {
		for index, name := range req.Tools {
			if tools.ForceMCPBlockedTools[normalizedName(name)] {
				findings = append(findings, errorFinding(fmt.Sprintf("%s.tools[%d]", field, index), FindingRequiredToolDenied, fmt.Sprintf("required tool %q is disabled by force-mcp", name)))
			}
		}
	}
	if ctx.EnvironmentLookup != nil {
		for index, name := range req.Environment {
			if value, ok := ctx.EnvironmentLookup(strings.TrimSpace(name)); !ok || value == "" {
				findings = append(findings, errorFinding(fmt.Sprintf("%s.environment[%d]", field, index), FindingRequiredEnvMissing, fmt.Sprintf("required environment variable %q is not set", name)))
			}
		}
	}
	if len(ctx.AllowedPaths) > 0 {
		for index, path := range req.Paths {
			if !pathCoveredByRoots(path, ctx.AllowedPaths) {
				owner := "team"
				if def != nil {
					owner = fmt.Sprintf("worker %q", def.Name)
				}
				findings = append(findings, errorFinding(fmt.Sprintf("%s.paths[%d]", field, index), FindingRequiredPathDenied, fmt.Sprintf("required path %q for %s is outside the effective allowed paths", path, owner)))
			}
		}
	}
	return findings
}

func reachableWorkers(session *TeamSession) []*agent.AgentDef {
	byName := make(map[string]*agent.AgentDef)
	if len(session.Config.Delegation.AllowedWorkers) > 0 {
		for _, allowedName := range session.Config.Delegation.AllowedWorkers {
			def := session.Agents[normalizedName(allowedName)]
			if def == nil {
				continue
			}
			role := normalizedName(def.Role)
			if role != "coordinator" && role != "orchestrator" {
				byName[normalizedName(def.Name)] = def
			}
		}
	} else {
		for _, def := range session.Agents {
			if def == nil {
				continue
			}
			role := normalizedName(def.Role)
			if role != "coordinator" && role != "orchestrator" {
				byName[normalizedName(def.Name)] = def
			}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]*agent.AgentDef, 0, len(names))
	for _, name := range names {
		result = append(result, byName[name])
	}
	return result
}

func declaredWorkerTools(def *agent.AgentDef) (map[string]bool, bool) {
	if def == nil || strings.TrimSpace(def.Tools) == "" || strings.EqualFold(strings.TrimSpace(def.Tools), "all") {
		return nil, true
	}
	declared := normalizedSet(strings.Split(def.Tools, ","))
	declared["submit_result"] = true
	for name := range def.MCPTools {
		declared[normalizedName(name)] = true
	}
	return declared, false
}

func pathCoveredByRoots(path string, roots []string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, root := range roots {
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, absPath)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func normalizedSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if key := normalizedName(value); key != "" {
			set[key] = true
		}
	}
	return set
}

func normalizedName(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func errorFinding(field, code, message string) ContractFinding {
	return ContractFinding{Severity: FindingSeverityError, Code: code, Field: field, Message: message}
}
