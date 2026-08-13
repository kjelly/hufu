package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestTeamToolDenyRemovesAlwaysIncludedStateWriters(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			ToolsDenied: []string{"stm_write", "ltm_update", "memory_save"},
		}},
		coreTools: workerInvariantCoreTools(t),
	}
	def := &agent.AgentDef{Name: "reader", Tools: "view"}
	exposed := agentToolNames(c.selectWorkerTools(def))
	for _, denied := range c.session.Config.ToolsDenied {
		if slices.Contains(exposed, denied) {
			t.Fatalf("denied tool %q was exposed to worker: %v", denied, exposed)
		}
	}
	if !slices.Contains(exposed, "view") {
		t.Fatalf("declared read tool was removed: %v", exposed)
	}
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(t.Context(), def, exposed))
	for _, denied := range c.session.Config.ToolsDenied {
		if slices.Contains(allowed, denied) {
			t.Fatalf("denied tool %q was retained in runtime allowlist: %v", denied, allowed)
		}
	}
}

func TestParseTeamToolDeny(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: deny-test\ntools:\n  denied: [stm_write]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ToolsDenied, []string{"stm_write"}) {
		t.Fatalf("ToolsDenied = %v, want [stm_write]", cfg.ToolsDenied)
	}
}

func TestTemplateScopedToolGrantDoesNotLoosenTeamDeny(t *testing.T) {
	c := &Coordinator{
		session:   &TeamSession{Config: agent.TeamConfig{ToolsDenied: []string{"terminal"}}},
		coreTools: workerInvariantCoreTools(t),
	}
	def := &agent.AgentDef{Name: "observer", Tools: "view,terminal"}
	plain := agentToolNames(c.selectWorkerTools(def))
	if slices.Contains(plain, "terminal") {
		t.Fatalf("team-denied terminal was exposed without a static contract: %v", plain)
	}

	task := TaskDef{ContractID: "static-observer", Execution: ExecutionContract{
		ToolSequence:       []string{"terminal", "submit_result"},
		TemplateToolGrants: []string{"terminal"},
	}}
	granted := agentToolNames(c.selectWorkerToolsForTask(def, task))
	if !slices.Contains(granted, "terminal") {
		t.Fatalf("template-scoped terminal grant was not exposed: %v", granted)
	}
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowedForTask(t.Context(), def, granted, task))
	if !slices.Contains(allowed, "terminal") {
		t.Fatalf("template-scoped terminal grant was not permitted at runtime: %v", allowed)
	}

	forged := task
	forged.ContractID = ""
	if exposed := agentToolNames(c.selectWorkerToolsForTask(def, forged)); slices.Contains(exposed, "terminal") {
		t.Fatalf("unbound task execution bypassed team deny: %v", exposed)
	}
	forgedAllowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowedForTask(t.Context(), def, granted, forged))
	if slices.Contains(forgedAllowed, "terminal") {
		t.Fatalf("unbound task grant bypassed runtime deny: %v", forgedAllowed)
	}
}

func TestDeniedToolInstructionsAreRemovedWithoutSyntheticToolNames(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			ToolsDenied: []string{"custom_memory_tool", "load_skill"},
		}},
	}
	prompt := c.filterDeniedPromptLines("keep this rule\ncall load_skill for details\nuse custom_memory_tool to save\n")
	if strings.Contains(prompt, "load_skill") || strings.Contains(prompt, "custom_memory_tool") {
		t.Fatalf("denied tool instructions remained in prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "keep this rule") {
		t.Fatalf("allowed prompt content was removed: %q", prompt)
	}
}

func TestPhaseCapabilityOverridesTemplateGrant(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{}}
	w, err := newRuntimeWorkflow(&TeamSession{
		Config: agent.TeamConfig{
			Name: "test-team",
			Workflow: agent.WorkflowConfig{Phases: []string{"prepare", "audit", "execute", "verify"}},
			Policies: agent.WorkflowPolicies{AllowPhaseSkip: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Initial state is PhasePrepare
	w.state = PhasePrepare
	c := &Coordinator{
		session:       session,
		coreTools:     workerInvariantCoreTools(t),
		phaseWorkflow: w,
	}

	def := &agent.AgentDef{Name: "preparer", Tools: "bash,view"}
	task := TaskDef{ContractID: "static-prep", Execution: ExecutionContract{
		TemplateToolGrants: []string{"bash"},
	}}

	// Even though the template grants 'bash', it must be removed in PhasePrepare
	granted := agentToolNames(c.selectWorkerToolsForTask(def, task))
	if slices.Contains(granted, "bash") {
		t.Fatalf("phase capability bypass: bash was exposed via template grant in %s phase", w.state)
	}
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowedForTask(t.Context(), def, granted, task))
	if slices.Contains(allowed, "bash") {
		t.Fatalf("phase capability bypass: bash was permitted in runtime allowlist in %s phase", w.state)
	}

	// Change to VERIFY and check again
	w.state = PhaseVerify
	granted = agentToolNames(c.selectWorkerToolsForTask(def, task))
	if slices.Contains(granted, "bash") {
		t.Fatalf("phase capability bypass: bash was exposed via template grant in %s phase", w.state)
	}
	allowed = tools.GetToolsAllowed(c.withEffectiveToolsAllowedForTask(t.Context(), def, granted, task))
	if slices.Contains(allowed, "bash") {
		t.Fatalf("phase capability bypass: bash was permitted in runtime allowlist in %s phase", w.state)
	}

	// Change to EXECUTE and verify it is allowed
	w.state = PhaseExecute
	granted = agentToolNames(c.selectWorkerToolsForTask(def, task))
	if !slices.Contains(granted, "bash") {
		t.Fatalf("bash was incorrectly removed in %s phase", w.state)
	}
	allowed = tools.GetToolsAllowed(c.withEffectiveToolsAllowedForTask(t.Context(), def, granted, task))
	if !slices.Contains(allowed, "bash") {
		t.Fatalf("bash was incorrectly removed from allowlist in %s phase", w.state)
	}
}
