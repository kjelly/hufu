package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

func lintRuntimeSemantics(session *TeamSession, policy EffectiveTeamContractContext, existing []ContractFinding) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	findings = append(findings, lintAgentRecoveryDefaults(session, policy)...)
	findings = append(findings, lintUnattendedAcceptance(session, policy)...)
	for index, task := range session.ContractTasks {
		field := fmt.Sprintf("tasks[%d]", index)
		def := session.Agents[normalizedName(task.Agent)]
		findings = append(findings, lintTaskRecovery(field, task, def, policy)...)
		findings = append(findings, lintTaskResultAndVerifier(field, task, policy, existing)...)
		findings = append(findings, lintTaskTimeout(field, task, def, session.Config)...)
	}
	findings = append(findings, lintResourceClaimConflicts(session.ContractTasks)...)
	return findings
}

func lintAgentRecoveryDefaults(session *TeamSession, policy EffectiveTeamContractContext) []ContractFinding {
	var findings []ContractFinding
	seen := make(map[*agent.AgentDef]bool)
	for _, def := range session.Agents {
		if def == nil || seen[def] || normalizedName(def.Role) == "coordinator" || normalizedName(def.Role) == "orchestrator" {
			continue
		}
		seen[def] = true
		effect := SideEffectClass(def.SideEffect)
		if effect == "" {
			effect = InferSideEffectClass(def.Tools)
		}
		recovery := ResolveRecoveryPolicy(RecoveryPolicy(def.Recovery), effect, policy.Unattended, policy.ExecutionProfile)
		if nonReplayableSideEffect(effect) && recovery == RecoveryRetry && strings.TrimSpace(def.ReconcileTool) == "" {
			findings = append(findings, errorFinding("agents."+normalizedName(def.Name)+".recovery", FindingSideEffectRetryWithoutReconcile,
				fmt.Sprintf("agent %q defaults to retry for %s side effects without a reconcile tool", def.Name, effect)))
		}
	}
	return findings
}

func lintTaskRecovery(field string, task TaskDef, def *agent.AgentDef, policy EffectiveTeamContractContext) []ContractFinding {
	effect, declaredRecovery, reconcileTool := resolveTaskRecovery(def, task)
	recovery := ResolveRecoveryPolicy(declaredRecovery, effect, policy.Unattended, policy.ExecutionProfile)
	if !nonReplayableSideEffect(effect) || recovery != RecoveryRetry || strings.TrimSpace(reconcileTool) != "" || taskHasBlockingVerifier(task) {
		return nil
	}
	return []ContractFinding{errorFinding(field+".recovery", FindingSideEffectRetryWithoutReconcile,
		fmt.Sprintf("task retries %s side effects without a reconcile tool or blocking verifier", effect))}
}

func lintTaskResultAndVerifier(field string, task TaskDef, policy EffectiveTeamContractContext, existing []ContractFinding) []ContractFinding {
	var findings []ContractFinding
	if (policy.ExecutionProfile.StrictPolicy || task.Execution.RequiresVerification) && !task.Execution.RequiresResult {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityWarning, Code: FindingStrictTaskWithoutTypedResult,
			Field:   field + ".execution.requires-result",
			Message: "task requires verification but does not author requires-result=true",
			Hint:    "set execution.requires-result: true so verification consumes a typed result",
		})
	}
	if len(task.Execution.Steps) > 0 {
		if task.Execution.RequiresVerification && !structuredStepsContainEffect(task.Execution.Steps, ExecutionEffectVerify) && !hasFindingForField(existing, field+".execution.steps", FindingExecutionStepsVerifierMissing) {
			findings = append(findings, errorFinding(field+".execution.steps", FindingExecutionStepsVerifierMissing,
				"structured execution with requires_verification=true must declare a verify step"))
		}
		return findings
	}
	if task.Execution.RequiresVerification && !taskHasBlockingVerifier(task) && !hasFindingForField(existing, field+".verify", FindingVerifierMissing) {
		findings = append(findings, errorFinding(field+".verify", FindingVerifierMissing,
			"requires_verification=true requires an objective blocking verifier"))
	}
	return findings
}

func lintUnattendedAcceptance(session *TeamSession, policy EffectiveTeamContractContext) []ContractFinding {
	if !policy.Unattended {
		return nil
	}
	mode := policy.ExecutionProfile.AcceptanceMode
	if configured := AcceptanceMode(strings.ToLower(strings.TrimSpace(session.Config.AcceptanceMode))); configured != "" {
		if configured != AcceptanceAdvisory || mode != AcceptanceBlocking {
			mode = configured
		}
	}
	hasChecks := strings.TrimSpace(session.Config.Acceptance) != ""
	if session.Config.AcceptanceSpec != nil {
		hasChecks = hasChecks || AcceptanceSpecHasChecks(*session.Config.AcceptanceSpec)
	}
	if hasChecks && mode != AcceptanceAdvisory {
		return nil
	}
	return []ContractFinding{errorFinding("acceptance", FindingAcceptanceMissingUnattended,
		"effective unattended execution requires a non-advisory acceptance contract")}
}

func lintResourceClaimConflicts(tasks []TaskDef) []ContractFinding {
	var findings []ContractFinding
	for left := range tasks {
		for right := left + 1; right < len(tasks); right++ {
			if claimsConflict(resourceClaims(tasks[left]), resourceClaims(tasks[right])) && !dependsEither(tasks, left, right) {
				findings = append(findings, errorFinding(fmt.Sprintf("tasks[%d].resources", right), FindingResourceClaimConflict,
					fmt.Sprintf("static tasks %d and %d have conflicting resource claims without dependency ordering", left, right)))
			}
		}
	}
	return findings
}

func lintTaskTimeout(field string, task TaskDef, def *agent.AgentDef, cfg agent.TeamConfig) []ContractFinding {
	if !taskHasBlockingVerifier(task) {
		return nil
	}
	taskTimeout := cfg.Timeout
	if def != nil && def.Timeout > 0 {
		taskTimeout = def.Timeout
	}
	if taskTimeout <= 0 {
		taskTimeout = 600
	}
	verifyTimeout := cfg.VerifyTimeout
	if verifyTimeout <= 0 {
		verifyTimeout = 120
	}
	if verifyTimeout < taskTimeout {
		return nil
	}
	return []ContractFinding{{
		Severity: FindingSeverityWarning, Code: FindingTimeoutImpossible, Field: field + ".verify",
		Message: fmt.Sprintf("verify-timeout (%ds) is greater than or equal to agent task timeout (%ds)", verifyTimeout, taskTimeout),
		Hint:    "set verify-timeout below the effective agent timeout",
	}}
}

func taskHasBlockingVerifier(task TaskDef) bool {
	if task.VerifySpec == nil && strings.TrimSpace(task.Verify) == "" {
		return false
	}
	var spec VerificationSpec
	if task.VerifySpec != nil {
		spec = *task.VerifySpec
	}
	spec = NormalizeVerificationSpec(spec, task.Verify, task.VerifyMode)
	if strings.EqualFold(strings.TrimSpace(spec.Mode), "observation") || strings.EqualFold(strings.TrimSpace(spec.Mode), "none") {
		return false
	}
	if err := validateVerificationSpec(spec); err != nil {
		return false
	}
	for _, finding := range LintVerifierWithMode(spec, task.Verify, task.VerifyMode) {
		if finding.Severity == FindingSeverityError {
			return false
		}
	}
	return true
}
