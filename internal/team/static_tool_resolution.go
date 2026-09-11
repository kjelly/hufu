package team

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// ToolAvailability is the offline answer for one authored tool reference.
type ToolAvailability string

const (
	ToolAvailable ToolAvailability = "available"
	ToolDenied    ToolAvailability = "denied"
	ToolMissing   ToolAvailability = "missing"
	ToolUnknown   ToolAvailability = "unknown"
)

// StaticToolResolutionInput contains only static data. BaseTools are subject
// to the agent declaration; SupplementalTools (for example already-loaded MCP
// handlers) are runtime additions that retain their existing exposure rules.
type StaticToolResolutionInput struct {
	Session           *TeamSession
	Agent             *agent.AgentDef
	Task              TaskDef
	LifecycleMode     WorkerToolResolutionMode
	Policy            EffectiveTeamContractContext
	BaseTools         []string
	SupplementalTools []string
	TrustedTaskGrants map[string]bool
	WorkflowEnabled   bool
	WorkflowPhase     Phase
}

// StaticToolResolution is ordered so runtime can project the decision back to
// concrete handlers without recomputing authorization policy.
type StaticToolResolution struct {
	Names []string
	Tools map[string]ToolAvailability
	// EffectiveSequence is the literal closed sequence for this lifecycle.
	EffectiveSequence []string
	ResultRequired    bool
	PlanRequired      bool
	ResultOnly        bool
}

// ResolveStaticWorkerTools resolves worker exposure without constructing a
// provider, fantasy tool, MCP client, workspace, or command process.
func ResolveStaticWorkerTools(input StaticToolResolutionInput) (StaticToolResolution, error) {
	if input.Agent == nil {
		return StaticToolResolution{}, fmt.Errorf("resolve static task tools: agent definition is required")
	}
	mode := input.LifecycleMode
	if mode == "" {
		mode = WorkerToolResolutionNormal
	}
	if !validWorkerToolResolutionMode(mode) {
		return StaticToolResolution{}, fmt.Errorf("resolve static task tools: unsupported worker lifecycle mode %q", mode)
	}

	result := StaticToolResolution{
		Tools: make(map[string]ToolAvailability),
	}
	result.ResultRequired = input.Task.Execution.RequiresResult
	switch mode {
	case WorkerToolResolutionInitialPlan:
		result.PlanRequired = true
		result.ResultRequired = false
	case WorkerToolResolutionApprovedPlan:
		result.ResultRequired = true
	case WorkerToolResolutionResultRepair, WorkerToolResolutionResume:
		result.ResultRequired = true
		result.ResultOnly = true
	}
	if mode == WorkerToolResolutionInitialPlan && len(input.Task.Execution.ToolSequence) > 0 {
		return StaticToolResolution{}, fmt.Errorf("resolve static task tools: initial-plan mode is incompatible with closed execution tool_sequence; remove tool_sequence or disable plan-first")
	}

	candidates := make([]string, 0, len(input.BaseTools)+len(input.SupplementalTools)+2)
	if !result.ResultOnly {
		candidates = append(candidates, agent.SelectToolNames(input.BaseTools, input.Agent.Tools)...)
		candidates = append(candidates, input.SupplementalTools...)
	}
	candidates = dedupeToolNames(candidates)
	denied := staticDeniedToolSet(input)
	for _, name := range candidates {
		status := ToolAvailable
		switch {
		case isCoordinatorOnlyWorkerTool(name):
			status = ToolDenied
		case isLegacyMemoryMutationTool(name) && !staticLegacyMemoryToolGranted(input, name):
			status = ToolDenied
		case denied[name]:
			status = ToolDenied
		case input.WorkflowEnabled && input.WorkflowPhase != PhaseExecute && executionCapabilityTools[name] && !input.TrustedTaskGrants[name]:
			status = ToolDenied
		}
		result.Tools[name] = status
		if status == ToolAvailable {
			result.Names = append(result.Names, name)
		}
	}

	if result.ResultRequired {
		if denied[submitResultToolName] {
			return StaticToolResolution{}, fmt.Errorf("resolve static task tools: required protocol tool %q is denied by team policy", submitResultToolName)
		}
		result.Names = append(result.Names, submitResultToolName)
		result.Tools[submitResultToolName] = ToolAvailable
	}
	if result.PlanRequired {
		if denied["submit_plan"] {
			return StaticToolResolution{}, fmt.Errorf("resolve static task tools: required protocol tool %q is denied by team policy", "submit_plan")
		}
		result.Names = append(result.Names, "submit_plan")
		result.Tools["submit_plan"] = ToolAvailable
	}
	result.Names = dedupeToolNames(result.Names)
	result.EffectiveSequence = slices.Clone(input.Task.Execution.ToolSequence)
	if result.ResultOnly {
		result.EffectiveSequence = []string{submitResultToolName}
	}
	if missing := missingToolNames(result.Names, result.EffectiveSequence); len(missing) > 0 {
		return StaticToolResolution{}, fmt.Errorf("execution tool_sequence requires unavailable tool(s) for agent %q: %s", input.Agent.Name, strings.Join(missing, ", "))
	}
	if len(result.EffectiveSequence) > 0 {
		result.Names = filterToolNamesForSequence(result.Names, result.EffectiveSequence)
	}
	if result.ResultOnly && !slices.Equal(result.Names, []string{submitResultToolName}) {
		return StaticToolResolution{}, fmt.Errorf("resolve static task tools: result-only repair surface must contain exactly %q", submitResultToolName)
	}
	return result, nil
}

func validWorkerToolResolutionMode(mode WorkerToolResolutionMode) bool {
	return mode == WorkerToolResolutionNormal || mode == WorkerToolResolutionInitialPlan || mode == WorkerToolResolutionApprovedPlan || mode == WorkerToolResolutionResultRepair || mode == WorkerToolResolutionResume
}

func staticDeniedToolSet(input StaticToolResolutionInput) map[string]bool {
	denied := make(map[string]bool)
	if input.Session != nil {
		for _, name := range input.Session.Config.ToolsDenied {
			if name = strings.TrimSpace(name); name != "" {
				denied[name] = true
			}
		}
	}
	applyPolicy := func(names []string) {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if input.Policy.NoNet && (name == "fetch" || name == "download" || name == "agentic_fetch") {
				denied[name] = true
			}
			if input.Policy.ForceMCP && tools.ForceMCPBlockedTools[name] {
				denied[name] = true
			}
		}
	}
	applyPolicy(input.BaseTools)
	applyPolicy(input.SupplementalTools)
	return denied
}

func staticLegacyMemoryToolGranted(input StaticToolResolutionInput, name string) bool {
	if !isLegacyMemoryMutationTool(name) {
		return true
	}
	if input.Session != nil {
		for _, allowed := range input.Session.Config.ToolsAllowed {
			if strings.TrimSpace(allowed) == name {
				return true
			}
		}
	}
	return explicitlyDeclaresTool(input.Agent.Tools, name)
}

func missingToolNames(available, sequence []string) []string {
	set := make(map[string]bool, len(available))
	for _, name := range available {
		set[name] = true
	}
	var missing []string
	for _, name := range sequence {
		if !set[name] && !slices.Contains(missing, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

func filterToolNamesForSequence(available, sequence []string) []string {
	if len(sequence) == 0 {
		return available
	}
	set := make(map[string]bool, len(sequence))
	for _, name := range sequence {
		set[name] = true
	}
	filtered := make([]string, 0, len(available))
	for _, name := range available {
		if set[name] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}
