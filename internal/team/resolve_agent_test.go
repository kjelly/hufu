package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResolveAgentNameExactMatch(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"senior developer":             {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
				"engineering-senior-developer": {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
				"code reviewer":                {Name: "Code Reviewer", FileAlias: "engineering-code-reviewer", Role: "worker"},
				"engineering-code-reviewer":    {Name: "Code Reviewer", FileAlias: "engineering-code-reviewer", Role: "worker"},
				"orchestrator":                 {Name: "orchestrator", FileAlias: "orchestrator", Role: "orchestrator"},
			},
		},
	}

	def, key, err := c.resolveAgentName("Senior Developer")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if def.Name != "Senior Developer" {
		t.Errorf("expected def.Name=Senior Developer, got %s", def.Name)
	}
	if key != "senior developer" {
		t.Errorf("expected key=senior developer, got %s", key)
	}

	def, key, err = c.resolveAgentName("engineering-senior-developer")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if def.Name != "Senior Developer" {
		t.Errorf("expected def.Name=Senior Developer, got %s", def.Name)
	}
}

func TestResolveAgentNameWordMatch(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"senior developer":             {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
				"engineering-senior-developer": {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
				"code reviewer":                {Name: "Code Reviewer", FileAlias: "engineering-code-reviewer", Role: "worker"},
				"engineering-code-reviewer":    {Name: "Code Reviewer", FileAlias: "engineering-code-reviewer", Role: "worker"},
			},
		},
	}

	def, _, err := c.resolveAgentName("developer")
	if err != nil {
		t.Fatalf("expected 'developer' to match Senior Developer, got error: %v", err)
	}
	if def.Name != "Senior Developer" {
		t.Errorf("expected def.Name=Senior Developer, got %s", def.Name)
	}

	def, _, err = c.resolveAgentName("reviewer")
	if err != nil {
		t.Fatalf("expected 'reviewer' to match Code Reviewer, got error: %v", err)
	}
	if def.Name != "Code Reviewer" {
		t.Errorf("expected def.Name=Code Reviewer, got %s", def.Name)
	}
}

func TestResolveAgentNameSegmentMatch(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"software architect":             {Name: "Software Architect", FileAlias: "engineering-software-architect", Role: "worker"},
				"engineering-software-architect": {Name: "Software Architect", FileAlias: "engineering-software-architect", Role: "worker"},
			},
		},
	}

	def, _, err := c.resolveAgentName("architect")
	if err != nil {
		t.Fatalf("expected 'architect' to match, got error: %v", err)
	}
	if def.Name != "Software Architect" {
		t.Errorf("expected def.Name=Software Architect, got %s", def.Name)
	}
}

func TestResolveAgentNameAmbiguous(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"code reviewer":        {Name: "Code Reviewer", FileAlias: "code-reviewer", Role: "worker"},
				"code-reviewer":        {Name: "Code Reviewer", FileAlias: "code-reviewer", Role: "worker"},
				"senior code reviewer": {Name: "Senior Code Reviewer", FileAlias: "senior-code-reviewer", Role: "worker"},
				"senior-code-reviewer": {Name: "Senior Code Reviewer", FileAlias: "senior-code-reviewer", Role: "worker"},
			},
		},
	}

	_, _, err := c.resolveAgentName("code")
	if err == nil {
		t.Error("expected ambiguous error for 'code', got nil")
	}
}

func TestResolveAgentNameNotFound(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"developer": {Name: "developer", FileAlias: "developer", Role: "worker"},
			},
		},
	}

	_, _, err := c.resolveAgentName("designer")
	if err == nil {
		t.Error("expected error for unknown agent, got nil")
	}
}

func TestResolveAgentNameCoordinatorBlocked(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"orchestrator": {Name: "orchestrator", FileAlias: "orchestrator", Role: "orchestrator"},
			},
		},
	}

	_, _, err := c.resolveAgentName("orchestrator")
	if err == nil {
		t.Error("expected error for coordinator agent, got nil")
	}
}

func TestResolveAgentNameEmptyInput(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"senior developer":             {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
				"engineering-senior-developer": {Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
			},
		},
	}

	_, _, err := c.resolveAgentName("")
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestResolveAgentName_ByDisplayName(t *testing.T) {
	// Built-in Helper is registered under the "helper" map key (FileAlias),
	// not under its display name "Helper". LLM coordinators may dispatch
	// using the display name, so resolveAgentName must fall back to a
	// case-insensitive match against def.Name.
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"helper": {Name: "Helper", FileAlias: "helper", Role: "worker"},
			},
		},
	}

	for _, input := range []string{"Helper", "helper", "HELPER"} {
		def, key, err := c.resolveAgentName(input)
		if err != nil {
			t.Errorf("resolveAgentName(%q) returned error: %v", input, err)
			continue
		}
		if def == nil || def.Name != "Helper" {
			t.Errorf("resolveAgentName(%q) returned %v, want Helper", input, def)
		}
		if key != "helper" {
			t.Errorf("resolveAgentName(%q) returned key %q, want %q", input, key, "helper")
		}
	}
}
