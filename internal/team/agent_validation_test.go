package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestExecuteTasks_AgentValidation(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"developer": {Name: "Developer", Role: "worker"},
				"reviewer":  {Name: "Reviewer", Role: "worker"},
			},
			Config: agent.TeamConfig{
				MaxRounds: 5,
			},
		},
	}

	tasksInvalid := []TaskDef{
		{Agent: "hacker", Goal: "steal data"},
	}
	_, err := c.ExecuteTasks(context.Background(), tasksInvalid)
	if err == nil {
		t.Error("expected error for invalid agent, got nil")
	}
	if !strings.Contains(err.Error(), "agent validation failed") {
		t.Errorf("expected error message to contain 'agent validation failed', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown agent \"hacker\"") {
		t.Errorf("expected error message to contain 'unknown agent \"hacker\"', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Developer") || !strings.Contains(err.Error(), "Reviewer") {
		t.Error("error message should list available agents")
	}

	tasksMixed := []TaskDef{
		{Agent: "developer", Goal: "write code"},
		{Agent: "ghost", Goal: "be spooky"},
	}
	_, err = c.ExecuteTasks(context.Background(), tasksMixed)
	if err == nil {
		t.Error("expected error for mixed valid/invalid agents, got nil")
	}
	if !strings.Contains(err.Error(), "unknown agent \"ghost\"") {
		t.Errorf("expected error message to contain 'unknown agent \"ghost\"', got %q", err.Error())
	}
}

func TestAllowsFreeTextWorkerResultOnlyForExplicitReadOnlyAgent(t *testing.T) {
	c := &Coordinator{session: &TeamSession{
		Config: agent.TeamConfig{AllowFreeTextResults: true},
		Agents: map[string]*agent.AgentDef{
			"reader": {Name: "reader", Role: "worker", Tools: "view", SideEffect: "none"},
			"writer": {Name: "writer", Role: "worker", Tools: "write", SideEffect: "workspace_write"},
		},
	}}
	if !c.allowsFreeTextWorkerResult(TaskDef{Agent: "reader", Goal: "review"}) {
		t.Fatal("explicit read-only agent should permit free-text result")
	}
	if c.allowsFreeTextWorkerResult(TaskDef{Agent: "reader", Goal: "edit", SideEffect: SideEffectWorkspaceWrite}) {
		t.Fatal("workspace-write task must retain structured result requirement")
	}
	if c.allowsFreeTextWorkerResult(TaskDef{Agent: "writer", Goal: "edit"}) {
		t.Fatal("mutating agent must retain structured result requirement")
	}
	c.session.Config.AllowFreeTextResults = false
	if c.allowsFreeTextWorkerResult(TaskDef{Agent: "reader", Goal: "review"}) {
		t.Fatal("team opt-in is required for free-text result")
	}
}

func TestFreeTextResultNeedsSummaryOnlyForInvalidFinalOutput(t *testing.T) {
	if !freeTextResultNeedsSummary(TaskDef{}, "Let me inspect one more file") {
		t.Fatal("unfinished narration should require a summary repair")
	}
	if !freeTextResultNeedsSummary(TaskDef{}, "   ") {
		t.Fatal("empty output should require a summary repair")
	}
	if freeTextResultNeedsSummary(TaskDef{}, "### Findings\n\nNo blocking issues identified.") {
		t.Fatal("a complete review report should not require a summary repair")
	}
}

func TestIncompleteReadOnlyReviewSummaryIsExplicitAndComplete(t *testing.T) {
	summary := incompleteReadOnlyReviewSummary(20)
	if !strings.Contains(summary, "incomplete") || !strings.Contains(summary, "20 inspection step") {
		t.Fatalf("summary = %q, want explicit incomplete evidence", summary)
	}
	if err := validateTaskOutput(TaskDef{}, summary); err != nil {
		t.Fatalf("fallback summary must be a valid final output: %v", err)
	}
}

func TestExecuteTasks_AcceptanceRecoveryCanDelegateDuringWrapUp(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Agents: map[string]*agent.AgentDef{}, Config: agent.TeamConfig{}},
	}
	c.SetWrapUp()
	c.acceptanceRecovery.Store(true)

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "missing", Goal: "repair acceptance failure"}})
	if err == nil {
		t.Fatal("expected agent validation error")
	}
	if strings.Contains(err.Error(), "wrap-up in progress") {
		t.Fatalf("acceptance recovery was still blocked by wrap-up: %v", err)
	}
}

func TestResolveAgentName_NilSession(t *testing.T) {
	c := &Coordinator{session: nil}
	_, _, err := c.resolveAgentName("developer")
	if err == nil {
		t.Error("expected error for nil session, got nil")
	}
	if !strings.Contains(err.Error(), "session not initialized") {
		t.Errorf("expected 'session not initialized' error, got %q", err.Error())
	}
}

func TestResolveAgentName_CoordinatorRole(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"orchestrator": {Name: "Orchestrator", Role: "orchestrator"},
				"coordinator":  {Name: "Coordinator", Role: "coordinator"},
			},
		},
	}
	_, _, err := c.resolveAgentName("orchestrator")
	if err == nil {
		t.Error("expected error for coordinator agent, got nil")
	}
	if !strings.Contains(err.Error(), "cannot delegate to coordinator") {
		t.Errorf("expected 'cannot delegate to coordinator' error, got %q", err.Error())
	}

	_, _, err = c.resolveAgentName("coordinator")
	if err == nil {
		t.Error("expected error for coordinator agent, got nil")
	}
}

func TestResolveAgentName_FuzzyMatch(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"senior-developer": {Name: "Senior Developer", Role: "worker", FileAlias: "senior-developer"},
			},
		},
	}

	def, key, err := c.resolveAgentName("developer")
	if err != nil {
		t.Errorf("expected fuzzy match for 'developer', got error: %v", err)
	}
	if def.Name != "Senior Developer" {
		t.Errorf("expected 'Senior Developer', got %q", def.Name)
	}
	if key != "senior developer" {
		t.Errorf("expected key 'senior developer', got %q", key)
	}
}

func TestResolveAgentName_Ambiguous(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"dev-a": {Name: "Dev Alpha", Role: "worker"},
				"dev-b": {Name: "Dev Beta", Role: "worker"},
			},
		},
	}

	_, _, err := c.resolveAgentName("dev")
	if err == nil {
		t.Error("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' error, got %q", err.Error())
	}
}

func TestResolveAgentName_ExactMatchPriority(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"dev":       {Name: "Dev", Role: "worker"},
				"dev-alpha": {Name: "Dev Alpha", Role: "worker"},
			},
		},
	}

	def, key, err := c.resolveAgentName("dev")
	if err != nil {
		t.Errorf("expected exact match, got error: %v", err)
	}
	if def.Name != "Dev" {
		t.Errorf("expected exact match 'Dev', got %q", def.Name)
	}
	if key != "dev" {
		t.Errorf("expected key 'dev', got %q", key)
	}
}
