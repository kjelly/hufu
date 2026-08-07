package team

import (
	"context"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
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
