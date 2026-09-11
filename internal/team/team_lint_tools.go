package team

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
)

var offlineRuntimeToolNames = []string{
	"context_query", "context_get", "load_skill",
	"stm_write", "ltm_update", "memory_save",
}

var offlineProtocolToolNames = []string{"submit_plan", "submit_result"}

func lintOfflineTools(session *TeamSession, sources *TeamSourceIndex, policy EffectiveTeamContractContext, directives []promptDirective) []TeamLintFinding {
	if session == nil {
		return nil
	}
	knownBase := append(tools.BuiltinToolNames(false, false), offlineRuntimeToolNames...)
	knownRegistry := append(slices.Clone(knownBase), offlineProtocolToolNames...)
	var findings []TeamLintFinding
	for _, def := range authoredAgentDefinitions(session, sources) {
		known := append(slices.Clone(knownBase), localMCPToolNames(def)...)
		resolution, err := ResolveStaticWorkerTools(StaticToolResolutionInput{
			Session: session, Agent: def, Policy: policy, BaseTools: known,
			SupplementalTools: localMCPToolNames(def), WorkflowPhase: PhaseExecute,
		})
		if err != nil {
			continue
		}
		findings = append(findings, lintDeclaredTools(def, sources, knownRegistry, session.MCPServers)...)
		for _, directive := range directives {
			if directive.Kind != promptDirectiveTool || normalizedName(directive.Agent) != normalizedName(def.Name) {
				continue
			}
			findings = append(findings, lintToolDirective(directive, session, def, knownRegistry, resolution, policy)...)
		}
	}
	return findings
}

func authoredAgentDefinitions(session *TeamSession, sources *TeamSourceIndex) []*agent.AgentDef {
	if session == nil || sources == nil {
		return nil
	}
	seen := make(map[*agent.AgentDef]bool)
	definitions := make([]*agent.AgentDef, 0, len(sources.Agents))
	for alias := range sources.Agents {
		def := session.Agents[normalizedName(alias)]
		if def != nil && !seen[def] {
			seen[def] = true
			definitions = append(definitions, def)
		}
	}
	return definitions
}

func localMCPToolNames(def *agent.AgentDef) []string {
	names := make([]string, 0, len(def.MCPTools))
	for name := range def.MCPTools {
		names = append(names, normalizedName(name))
	}
	return names
}

func lintDeclaredTools(def *agent.AgentDef, sources *TeamSourceIndex, known []string, servers map[string]mcp.MCPServerConfig) []TeamLintFinding {
	knownSet := normalizedSet(known)
	for name := range def.MCPTools {
		knownSet[normalizedName(name)] = true
	}
	var findings []TeamLintFinding
	for name := range strings.SplitSeq(def.Tools, ",") {
		name = normalizeToolAlias(strings.TrimSpace(name))
		if name == "" || name == "all" || knownSet[name] || mcpServerForTool(name, servers) != "" {
			continue
		}
		loc := agentFieldLocation(sources, def, "tools")
		findings = append(findings, projectedLintFinding(FindingDeclaredToolMissing, FindingSeverityError, def.Name, "agents."+normalizedName(def.Name)+".tools", "", loc,
			fmt.Sprintf("declared tool %q is not present in any offline registry", name), "remove the declaration or register the tool"))
	}
	return findings
}

func lintToolDirective(directive promptDirective, session *TeamSession, def *agent.AgentDef, known []string, resolution StaticToolResolution, policy EffectiveTeamContractContext) []TeamLintFinding {
	name := normalizeToolAlias(directive.Name)
	if server := mcpServerForTool(name, session.MCPServers); server != "" {
		return lintMCPDirective(directive, name, server, session, policy)
	}
	if strings.Contains(name, "__") {
		return []TeamLintFinding{projectedLintFinding(FindingMCPToolMissing, FindingSeverityError, def.Name, directive.FieldPath, "not_found", directive.Location,
			fmt.Sprintf("MCP tool %q refers to an undeclared server", name), "declare the MCP server or correct the tool name")}
	}
	knownSet := normalizedSet(known)
	if !knownSet[name] {
		return []TeamLintFinding{projectedLintFinding(FindingPromptUnknownTool, FindingSeverityWarning, def.Name, directive.FieldPath, "not_found", directive.Location,
			fmt.Sprintf("prompt directs the agent to use unknown tool %q", name), "declare the tool or remove the directive")}
	}
	if toolExplicitlyDenied(name, session, policy) || resolution.Tools[name] == ToolDenied {
		return []TeamLintFinding{projectedLintFinding(FindingPromptDeniedTool, FindingSeverityError, def.Name, directive.FieldPath, "", directive.Location,
			fmt.Sprintf("prompt directs the agent to use denied tool %q", name), "remove the directive or change the effective policy")}
	}
	if !slices.Contains(resolution.Names, name) {
		return []TeamLintFinding{projectedLintFinding(FindingPromptToolNotGranted, FindingSeverityWarning, def.Name, directive.FieldPath, "", directive.Location,
			fmt.Sprintf("prompt directs the agent to use tool %q, but it is not in the effective grant", name), "add the tool to the agent grant or remove the directive")}
	}
	return nil
}

func lintMCPDirective(directive promptDirective, name, server string, session *TeamSession, policy EffectiveTeamContractContext) []TeamLintFinding {
	cfg := session.MCPServers[server]
	_, toolName, _ := strings.Cut(name, "__")
	if toolExplicitlyDenied(name, session, policy) || slices.Contains(cfg.ExcludedTools, toolName) {
		return []TeamLintFinding{projectedLintFinding(FindingPromptDeniedTool, FindingSeverityError, directive.Agent, directive.FieldPath, "", directive.Location,
			fmt.Sprintf("prompt directs the agent to use denied MCP tool %q", name), "remove the directive or change the effective policy")}
	}
	return []TeamLintFinding{projectedLintFinding(FindingMCPToolUnknown, FindingSeverityInfo, directive.Agent, directive.FieldPath, "unknown", directive.Location,
		fmt.Sprintf("MCP server %q is declared, but offline metadata cannot prove tool %q exists", server, toolName), "verify the server catalog at runtime")}
}

func mcpServerForTool(name string, servers map[string]mcp.MCPServerConfig) string {
	server, _, ok := strings.Cut(name, "__")
	if !ok {
		return ""
	}
	for declared := range servers {
		if normalizedName(declared) == normalizedName(server) {
			return declared
		}
	}
	return ""
}

func toolExplicitlyDenied(name string, session *TeamSession, policy EffectiveTeamContractContext) bool {
	if slices.ContainsFunc(session.Config.ToolsDenied, func(denied string) bool { return normalizedName(denied) == name }) {
		return true
	}
	if policy.NoNet && slices.Contains([]string{"fetch", "download", "agentic_fetch"}, name) {
		return true
	}
	return policy.ForceMCP && tools.ForceMCPBlockedTools[name]
}

func normalizeToolAlias(name string) string {
	switch normalizedName(name) {
	case "read":
		return "view"
	case "find":
		return "glob"
	default:
		return normalizedName(name)
	}
}

func agentFieldLocation(sources *TeamSourceIndex, def *agent.AgentDef, field string) TeamSourceLocation {
	if sources == nil || def == nil {
		return TeamSourceLocation{Status: LocationFieldOnly}
	}
	if source, ok := sources.Agents[def.FileAlias]; ok {
		if loc, ok := source.Fields[field]; ok {
			return loc
		}
		return TeamSourceLocation{File: source.File, Status: LocationFieldOnly}
	}
	return TeamSourceLocation{Status: LocationFieldOnly}
}

func projectedLintFinding(code, severity, agentName, field, resolution string, loc TeamSourceLocation, message, suggestion string) TeamLintFinding {
	return TeamLintFinding{
		Code: code, Severity: severity, File: loc.File, Line: loc.Line, Column: loc.Column,
		LocationStatus: loc.Status, Agent: agentName, FieldPath: field, Resolution: resolution,
		Message: message, Suggestion: suggestion,
	}
}
