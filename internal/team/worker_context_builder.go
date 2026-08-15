package team

import "github.com/kjelly/hufu/internal/agent"

// buildWorkerContextInput is the single normative-fragment builder shared by
// DAG, direct, extra-model, and nested worker dispatch. Callers add historical
// and file/dependency sources after this helper returns.
func buildWorkerContextInput(request ContextRequest, task TaskDef, agentDef *agent.AgentDef, approvedPlan, instructions, verification, runtimeContext string, skills []ContextItem) WorkerContextInput {
	return WorkerContextInput{
		Request: request, Goal: task.Goal, Constraints: task.Constraints, ApprovedPlan: approvedPlan,
		AgentInstructions: instructions, VerificationCriteria: verification, RuntimeContext: runtimeContext,
		SkillContext: skills, TaskDef: task, AgentDef: agentDef,
	}
}
