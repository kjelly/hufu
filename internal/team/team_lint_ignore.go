package team

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var teamLintCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TeamLintIgnoreSelector is an invocation-scoped waiver. File uses the public
// team-relative slash path, and a zero Line matches every line in that file.
type TeamLintIgnoreSelector struct {
	Code string
	File string
	Line int
}

var teamLintKnownCodes = func() map[string]bool {
	codes := []string{
		FindingVerifierNotAsserting, FindingVerifierInvalid, FindingExecutableUnresolved,
		FindingAcceptanceVacuous, FindingCompletionToolDenied, FindingDelegationWorkerUnknown,
		FindingDelegationWorkerRole, FindingDelegationWorkerDenied, FindingToolPolicyConflict,
		FindingDeprecatedMemoryTool, FindingRequiredToolDenied, FindingRequiredToolUnavailable,
		FindingRequiredEnvMissing, FindingRequiredPathDenied, FindingInteractiveUnattended,
		FindingNetworkDisabled, FindingPlanFirstRequired, FindingRequirementInvalid,
		FindingWorksetSourceConflict, FindingWorksetReceiptSource, FindingWorksetChildVerify,
		FindingActionProviderMissing, FindingActionRecoveryConflict, FindingUnattendedWorksetBudget,
		FindingUnattendedAcceptance, FindingWorksetCommandBinding, FindingLegacyFanOutDeprecated,
		FindingDecisionEvidenceInvalid, FindingUnsupportedSchemaVersion, FindingDuplicateAgent,
		FindingMissingCoordinator, FindingMultipleCoordinators, FindingPromptUnknownTool,
		FindingPromptDeniedTool, FindingPromptToolNotGranted, FindingDeclaredToolMissing,
		FindingMCPToolMissing, FindingMCPToolUnknown, FindingPromptUnknownSkill,
		FindingRequiredSkillMissing, FindingSkillNotAvailable, FindingSideEffectRetryWithoutReconcile,
		FindingStrictTaskWithoutTypedResult, FindingAcceptanceMissingUnattended,
		FindingVerifierMissing, FindingExecutionStepsVerifierMissing, FindingResourceClaimConflict,
		FindingTimeoutImpossible, FindingDeadlineConflict, FindingObservationMode,
		"deprecated_field", "legacy_execution_provider_field", "unknown_task_agent", "dependency_cycle",
		"max_rounds_below_minimum_coordinator_rounds", "goal_contract_agent_missing",
		"goal_contract_id_duplicate", "goal_contract_agent_unknown", "goal_contract_output_mode",
		"initial_contract_batch_missing", "initial_contract_agent_missing", "initial_contract_duplicate",
		"initial_contract_agent_unknown", "initial_contract_output_mode", "initial_contract_missing",
		"initial_contract_id_duplicate", "initial_contract_tool_denied", "initial_contract_tool_unauthorized",
		"task_goal_invariant_agent_missing", "task_goal_invariant_selector_missing", "task_goal_invariant_empty",
		"task_goal_invariant_literal_empty", "task_goal_invariant_execution_field_unknown",
		"task_goal_reference_prefix_duplicate", "task_goal_reference_prefix_missing",
		"task_goal_reference_agent_missing", "task_goal_reference_selector_missing", "on_failure_classes_unknown",
		"invalid_execution_kind", "execution_contract_mixed_modes", "tool_sequence_plan_first",
		"tool_sequence_empty_tool", "tool_sequence_invalid_name", "tool_sequence_terminal_result",
		"tool_sequence_requires_result", "tool_input_sequence_length", "tool_input_sequence_requires_tools",
		"template_tool_grants_requires_tools", "template_tool_grants_invalid", "template_tool_grant_not_in_sequence",
		"tool_input_canonical_sequence_requires_tools", "tool_input_canonical_sequence_length",
		"tool_input_canonical_sequence_empty_slot", "tool_input_transform_sequence_requires_tools",
		"tool_input_transform_sequence_length", "tool_input_transform_sequence_unknown",
		"tool_input_value_sequence_mixed_tools", "tool_input_value_sequence_invalid",
		"tool_input_value_sequence_terminal_field", "tool_expected_exit_codes_invalid",
		"tool_expected_exit_codes_terminal", "tool_expected_exit_codes_zero", "tool_expected_exit_codes_duplicate",
	}
	known := make(map[string]bool, len(codes))
	for _, code := range codes {
		known[code] = true
	}
	return known
}()

// ParseTeamLintIgnoreSelector validates CODE, CODE@FILE, or CODE@FILE:LINE.
func ParseTeamLintIgnoreSelector(raw string) (TeamLintIgnoreSelector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Count(raw, "@") > 1 {
		return TeamLintIgnoreSelector{}, fmt.Errorf("invalid ignore selector %q", raw)
	}
	code, location, hasLocation := strings.Cut(raw, "@")
	if !teamLintCodePattern.MatchString(code) || !teamLintKnownCodes[code] {
		return TeamLintIgnoreSelector{}, fmt.Errorf("unknown team lint code %q", code)
	}
	selector := TeamLintIgnoreSelector{Code: code}
	if !hasLocation {
		return selector, nil
	}
	if location == "" {
		return TeamLintIgnoreSelector{}, fmt.Errorf("invalid ignore selector %q: file is required after @", raw)
	}
	file := location
	if colon := strings.LastIndexByte(location, ':'); colon >= 0 {
		file = location[:colon]
		line, err := strconv.Atoi(location[colon+1:])
		if err != nil || line <= 0 {
			return TeamLintIgnoreSelector{}, fmt.Errorf("invalid ignore selector %q: line must be a positive integer", raw)
		}
		selector.Line = line
	}
	if file == "" || strings.Contains(file, `\`) || strings.HasPrefix(file, "/") || path.Clean(file) != file || file == "." || file == ".." || strings.HasPrefix(file, "../") || strings.Contains(file, "/../") {
		return TeamLintIgnoreSelector{}, fmt.Errorf("invalid ignore selector %q: file must be a clean team-relative slash path", raw)
	}
	selector.File = file
	return selector, nil
}

// ApplyTeamLintIgnores marks matching findings without changing their order.
func ApplyTeamLintIgnores(findings []TeamLintFinding, selectors []TeamLintIgnoreSelector) {
	for index := range findings {
		for _, selector := range selectors {
			if findings[index].Code != selector.Code || selector.File != "" && findings[index].File != selector.File || selector.Line > 0 && findings[index].Line != selector.Line {
				continue
			}
			findings[index].Ignored = true
			break
		}
	}
}
