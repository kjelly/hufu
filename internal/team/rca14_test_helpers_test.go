package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// createAdmittedTestTask models the durable creation boundary used by the
// runtime. The admission must be durable before CommitTaskCreationResolved
// makes the task visible to execution or recovery.
func createAdmittedTestTask(t *testing.T, c *Coordinator, task TaskDef) (TaskDef, *TodoItem) {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	if task.ID == "" {
		task.ID = ids[0]
	}

	var agentDef *agent.AgentDef
	if c.session != nil {
		agentDef = c.session.Agents[task.Agent]
	}
	resolvedModel := task.Model
	if agentDef != nil && resolvedModel == "" {
		resolvedModel = c.resolveAgentModel(agentDef, "")
	}
	task = c.canonicalizeTaskOccurrence(task, agentDef, resolvedModel)
	if agentDef != nil {
		task.ModelTopology = initialTaskModelTopology(agentDef, resolvedModel)
	}

	spec := todoSpecForTestTask(task)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	if len(items) != 1 || items[0] == nil {
		t.Fatalf("created items = %#v, want one task", items)
	}
	createdProjection, err := taskOccurrenceProjectionFromSpec(spec, items[0].ID)
	if err != nil {
		t.Fatalf("created task occurrence projection: %v", err)
	}
	gotDigest, err := decisionTaskInputDigest(createdProjection)
	if err != nil {
		t.Fatalf("digest created task: %v", err)
	}
	wantDigest, err := decisionTaskInputDigest(projection)
	if err != nil {
		t.Fatalf("digest admitted task: %v", err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("created task projection does not match admitted spec: got digest %q, want %q", gotDigest, wantDigest)
	}
	return task, items[0]
}

func todoSpecForTestTask(task TaskDef) TodoSpec {
	desc := task.Goal
	if task.Constraints != "" {
		desc += "\nconstraints: " + task.Constraints
	}
	return TodoSpec{
		PlanTaskID:          task.ID,
		PlanFirst:           task.PlanFirst,
		PlanID:              task.PlanID,
		Phase:               task.Phase,
		Action:              cloneActionPtr(task.Action),
		ContractID:          task.ContractID,
		ContractHash:        task.ContractHash,
		ContractRevision:    task.ContractRevision,
		Agent:               strings.ToLower(task.Agent),
		Desc:                desc,
		Goal:                task.Goal,
		Constraints:         task.Constraints,
		Model:               task.Model,
		ModelTopology:       cloneModelTopology(task.ModelTopology),
		Sidecar:             task.Sidecar,
		Summarize:           task.Summarize,
		OutputMode:          task.OutputMode,
		ContextFiles:        append([]string(nil), task.ContextFiles...),
		Requires:            append([]string(nil), task.Requires...),
		Source:              TaskSourceCoordinator,
		Verify:              task.Verify,
		VerifyMode:          task.VerifyMode,
		VerifySpec:          cloneVerificationSpecPtr(task.VerifySpec),
		WorksetBinding:      cloneWorksetBinding(task.WorksetBinding),
		WorksetReceipt:      cloneWorksetReceipt(task.WorksetReceipt),
		MaxRetries:          task.MaxRetries,
		Escalate:            task.Escalate,
		AdversarialVerify:   task.AdversarialVerify,
		SideEffect:          task.SideEffect,
		Recovery:            task.Recovery,
		ReconcileTool:       task.ReconcileTool,
		Kind:                task.Kind,
		Advances:            append([]string(nil), task.Advances...),
		ExpectedStateChange: task.ExpectedStateChange,
		RecoveryHypothesis:  cloneRecoveryHypothesis(task.RecoveryHypothesis),
		Execution:           task.Execution,
		Optional:            task.Optional,
		ResourceClaims:      append([]string(nil), task.ResourceClaims...),
		Resources:           append([]ResourceClaim(nil), task.Resources...),
		DecisionProfile:     task.DecisionProfile,
		DecisionOptions:     append([]DecisionOption(nil), task.DecisionOptions...),
		DecisionAssumptions: cloneDecisionAssumptions(task.DecisionAssumptions),
		DecisionFacts:       cloneDecisionFacts(task.DecisionFacts),
		DecisionArtifacts:   append([]ArtifactRef(nil), task.DecisionArtifacts...),
		DecisionBaseRates:   cloneBaseRateEvidence(task.DecisionBaseRates),
		DecisionProvenance:  cloneEvidenceProvenance(task.DecisionProvenance),
	}
}
