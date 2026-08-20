package team

// shouldWarnPromptWorkflowDeprecation reports whether a team has configured
// prompt-driven orchestration constraints that should migrate to the runtime
// workflow contract. Plain coordinators, including short-lived preflight
// coordinators used for team selection, intentionally do not need this warning.
func shouldWarnPromptWorkflowDeprecation(session *TeamSession) bool {
	if session == nil || len(session.Config.Workflow.Phases) > 0 {
		return false
	}
	delegation := session.Config.Delegation
	return len(session.ContractTasks) > 0 ||
		delegation.BindTaskGoalContracts ||
		len(delegation.InitialBatch) > 0 ||
		len(delegation.TaskGoalInvariants) > 0
}
