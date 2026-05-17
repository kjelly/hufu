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

	// 1. Test valid agents
	// We don't want to run the whole thing, just check if it passes validation
	// Since we can't easily mock the whole ExecuteTasks, we just verify the validation part
	// is called by checking the error when an invalid agent is passed.

	// 2. Test invalid agent
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

	// 3. Test mixed valid/invalid
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
