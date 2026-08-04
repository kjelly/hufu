package team

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/sidecar"
)

func sidecarContractCoordinator(t *testing.T, tools string) *Coordinator {
	t.Helper()
	return &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "team"},
			Agents: map[string]*agent.AgentDef{
				"helper": {Name: "Helper", Tools: tools},
			},
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
}

// TestSidecarTaskWithSideEffectAndNoVerifierIsRejected reproduces the observed
// false success. The coordinator, blocked by repeated worker failures, reran the
// same deployment goal with sidecar:true. A sidecar has no tools, so the model
// replied "I cannot execute system commands" — and that prose was recorded as a
// completed task, letting the DAG advance past a deployment that never ran.
func TestSidecarTaskWithSideEffectAndNoVerifierIsRejected(t *testing.T) {
	c := sidecarContractCoordinator(t, "bash,terminal")
	task := TaskDef{
		Agent:   "Helper",
		Goal:    "Execute the pilot deploy site-wide command. This is a long-running task (30+ minutes).",
		Sidecar: true,
	}

	err := c.validateSidecarTaskContract(task)
	if err == nil {
		t.Fatal("a tool-less sidecar must not be trusted to perform a workspace-mutating task")
	}
	for _, want := range []string{"no tools", "sidecar:true", "verifier"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection should tell the planner how to fix it (missing %q): %v", want, err)
		}
	}

	if err := c.validateSidecarTaskContracts([]TaskDef{task}); err == nil {
		t.Error("the plan-time gate must reject the same task before any model call")
	}
}

func TestSidecarTaskContractAllowsAnalysisAndVerifiedWork(t *testing.T) {
	tests := []struct {
		name  string
		tools string
		task  TaskDef
	}{
		{
			name:  "read-only agent has nothing to prove",
			tools: "view,grep",
			task:  TaskDef{Agent: "Helper", Goal: "Summarize the failure classes in this run", Sidecar: true},
		},
		{
			name:  "an objective verifier, not the model's prose, decides the outcome",
			tools: "bash,terminal",
			task:  TaskDef{Agent: "Helper", Goal: "Render the report", Sidecar: true, Verify: "test -s report.md"},
		},
		{
			name:  "a verify spec counts as an objective verifier",
			tools: "bash,terminal",
			task:  TaskDef{Agent: "Helper", Goal: "Render the report", Sidecar: true, VerifySpec: &VerificationSpec{Type: VerifyCommandExit, Command: "test -s report.md"}},
		},
		{
			name:  "an explicit non-mutating declaration wins over tool inference",
			tools: "bash,terminal",
			task:  TaskDef{Agent: "Helper", Goal: "Classify these errors", Sidecar: true, SideEffect: SideEffectNone},
		},
		{
			name:  "worker tasks are untouched by this gate",
			tools: "bash,terminal",
			task:  TaskDef{Agent: "Helper", Goal: "Execute the deploy"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := sidecarContractCoordinator(t, tc.tools)
			if err := c.validateSidecarTaskContract(tc.task); err != nil {
				t.Fatalf("validateSidecarTaskContract = %v, want nil", err)
			}
		})
	}
}

// TestSidecarTaskContractRejectsHighRiskTools keeps the gate aligned with the
// recovery policy: a task judged infra-mutating for recovery purposes must not
// be judged harmless here.
func TestSidecarTaskContractRejectsHighRiskTools(t *testing.T) {
	for _, tools := range []string{"all", "bash,sudo", "ssh"} {
		c := sidecarContractCoordinator(t, tools)
		task := TaskDef{Agent: "Helper", Goal: "Restart the cluster", Sidecar: true}
		if err := c.validateSidecarTaskContract(task); err == nil {
			t.Errorf("tools=%q: an unverified sidecar with %s side effects must be rejected", tools, InferSideEffectClass(tools))
		}
	}
}

// TestExecuteSidecarTaskRefusesUnverifiedMutation covers the runtime path, which
// is reachable from the DAG scheduler independently of ExecuteTasks.
func TestExecuteSidecarTaskRefusesUnverifiedMutation(t *testing.T) {
	c := sidecarContractCoordinator(t, "bash,terminal")
	// A configured sidecar is required to reach the sidecar path at all: with
	// none configured the task correctly falls back to a tool-equipped worker.
	// The guard rejects before Execute is ever called, so a zero Sidecar is
	// enough — and if the guard regressed, the nil agent inside would panic
	// rather than silently record a success.
	c.SetAgentPool(&mockAgentPool{
		resolveDef: &agent.AgentDef{Name: "Helper", Tools: "bash,terminal"},
		resolveKey: "helper",
		sidecar:    &sidecar.Sidecar{},
	})
	todo := c.taskTracker.TodoList()
	added := todo.AddBatch([]TodoSpec{{Agent: "helper", Desc: "Execute the pilot deploy site-wide command"}})
	if len(added) != 1 {
		t.Fatalf("AddBatch created %d items, want 1", len(added))
	}
	id := added[0].ID

	task := TaskDef{Agent: "Helper", Goal: "Execute the pilot deploy site-wide command", Sidecar: true}
	out, err := c.executeSidecarTask(t.Context(), task, id)
	if err == nil {
		t.Fatal("executeSidecarTask must refuse an unverified mutating task")
	}
	if out != "" {
		t.Errorf("no output may be recorded for a refused task, got %q", out)
	}
	for _, item := range todo.Items() {
		if item.ID == id && item.Status == TaskDone {
			t.Error("a refused sidecar task must never reach TaskDone")
		}
	}
}
