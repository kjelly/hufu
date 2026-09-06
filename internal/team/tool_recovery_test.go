package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// deployerAgent is the agent every commitment test resolves. Its tool list is
// what makes a declared compensate tool real or imaginary.
func deployerAgent() *agent.AgentDef {
	return &agent.AgentDef{
		Name: "deployer", Role: "worker", Tools: "bash,view,delete-bridge",
		ToolRecovery: map[string]agent.ToolRecoveryDecl{
			"bash": {CompensateTool: "delete-bridge", RetrySafe: false},
		},
	}
}

func commitmentCoordinator(t *testing.T, def *agent.AgentDef) *Coordinator {
	t.Helper()
	c := &Coordinator{
		sessionTime:  time.Now(),
		eventJournal: &memoryJournal{},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "commitment"},
			Agents:    map[string]*agent.AgentDef{def.Name: def},
		},
	}
	return c
}

func TestResolveToolRecoveryPrefersTaskOverAgentPerField(t *testing.T) {
	def := deployerAgent()
	def.ToolRecovery["bash"] = agent.ToolRecoveryDecl{
		CompensateTool: "delete-bridge", IdempotencyKey: "agent-key", ReconcileTool: "agent-probe",
	}
	for _, tc := range []struct {
		name string
		task TaskDef
		want ToolRecoverySpec
	}{
		{
			name: "agent default applies when the task declares nothing",
			task: TaskDef{Agent: "deployer"},
			want: ToolRecoverySpec{CompensateTool: "delete-bridge", IdempotencyKey: "agent-key", ReconcileTool: "agent-probe"},
		},
		{
			name: "task overrides only the fields it names",
			task: TaskDef{Agent: "deployer", ToolRecovery: map[string]ToolRecoverySpec{
				"bash": {CompensateTool: "task-compensate"},
			}},
			want: ToolRecoverySpec{CompensateTool: "task-compensate", IdempotencyKey: "agent-key", ReconcileTool: "agent-probe"},
		},
		{
			name: "tool names match case-insensitively",
			task: TaskDef{Agent: "deployer", ToolRecovery: map[string]ToolRecoverySpec{
				"BASH": {RetrySafe: true},
			}},
			want: ToolRecoverySpec{CompensateTool: "delete-bridge", IdempotencyKey: "agent-key", ReconcileTool: "agent-probe", RetrySafe: true},
		},
		{
			name: "an undeclared tool inherits only the task-level probe",
			task: TaskDef{Agent: "deployer", ReconcileTool: "task-probe"},
			want: ToolRecoverySpec{ReconcileTool: "task-probe"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toolName := "bash"
			if strings.Contains(tc.name, "undeclared") {
				toolName = "write"
			}
			got := resolveToolRecovery(def, tc.task, toolName)
			if got != tc.want {
				t.Fatalf("resolved = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// Acceptance: a valid compensate tool passes require-rollback, and its
// absence is rejected. The spec reaching the gate is the invoked tool's, not
// a task-wide constant.
func TestCommitGateRequireRollbackUsesInvokedToolContract(t *testing.T) {
	def := deployerAgent()
	c := commitmentCoordinator(t, def)
	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{RequireRollback: true}, StopPolicy{}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash", `{"command":"ovs-vsctl add-br br0"}`); denial != "" {
		t.Fatalf("declared compensate tool was rejected: %q", denial)
	}
	// "write" declares no compensating operation. Clearing the gate for bash
	// must not authorize it.
	denial := c.commitGateDenial(context.Background(), "todo-1", "write", `{"file_path":"out.txt"}`)
	if denial == "" {
		t.Fatal("a tool with no compensating operation cleared require-rollback")
	}
	if !strings.Contains(denial, ReasonCommitGateMissingRecovery) {
		t.Fatalf("denial = %q, want %s", denial, ReasonCommitGateMissingRecovery)
	}
}

// Acceptance: commitment is evaluated at the true mutation boundary. A
// read-only observation must never be mistaken for committed execution.
func TestCommitGateSkipsReadOnlyObservation(t *testing.T) {
	def := deployerAgent()
	c := commitmentCoordinator(t, def)
	journal := c.eventJournal.(*memoryJournal)
	task := mutatingTask()
	task.Agent = "deployer"
	// A contract that cannot satisfy require-rollback for any tool.
	task.ToolRecovery = nil
	def.ToolRecovery = nil
	policy := disciplinePolicy(CommitGatePolicy{RequireRollback: true}, StopPolicy{}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	for _, call := range []struct{ tool, input string }{
		{"view", `{"file_path":"main.go"}`},
		{"ls", `{"path":"."}`},
		{"grep", `{"pattern":"x"}`},
		{"bash", `{"command":"cat /etc/hosts"}`},
	} {
		if denial := c.commitGateDenial(context.Background(), "todo-1", call.tool, call.input); denial != "" {
			t.Fatalf("read-only %s denied: %q", call.tool, denial)
		}
	}
	if journal.count(agent.EventCommitGateBlocked) != 0 {
		t.Fatal("a read-only observation produced a commit gate event")
	}
	// The observations did not latch anything: the first real mutation is
	// still gated.
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash", `{"command":"ovs-vsctl add-br br0"}`); denial == "" {
		t.Fatal("read-only calls latched the commit gate open for a mutation")
	}
}

// Acceptance: a rollback path the worker could not take is rejected before
// execution, not accepted as a rollback that does not exist.
func TestArmDisciplineRejectsUninvocableCompensateTool(t *testing.T) {
	def := deployerAgent()
	def.ToolRecovery = map[string]agent.ToolRecoveryDecl{
		"bash": {CompensateTool: "tool-the-agent-cannot-call"},
	}
	c := commitmentCoordinator(t, def)
	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{RequireRollback: true}, StopPolicy{}, ReplanPolicy{})

	err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1"})
	if err == nil {
		t.Fatal("arming accepted a compensate tool the agent cannot invoke")
	}
	if !strings.Contains(err.Error(), ReasonCommitGateMissingRecovery) {
		t.Fatalf("error = %v, want %s", err, ReasonCommitGateMissingRecovery)
	}
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a rejected contract still armed a discipline")
	}
}

func TestValidateToolRecoveryRejectsUnusableDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		decls map[string]agent.ToolRecoveryDecl
		want  string
	}{
		{
			name:  "empty tool name",
			decls: map[string]agent.ToolRecoveryDecl{"  ": {CompensateTool: "x"}},
			want:  "empty tool name",
		},
		{
			name:  "blank compensate tool",
			decls: map[string]agent.ToolRecoveryDecl{"bash": {CompensateTool: "   "}},
			want:  "compensate-tool is blank",
		},
		{
			name:  "self-compensating tool",
			decls: map[string]agent.ToolRecoveryDecl{"bash": {CompensateTool: "BASH"}},
			want:  "cannot be the tool it compensates",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := agent.ValidateToolRecovery(tc.decls)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if err := agent.ValidateToolRecovery(map[string]agent.ToolRecoveryDecl{
		"bash": {CompensateTool: "delete-bridge", ReconcileTool: "probe"},
	}); err != nil {
		t.Fatalf("valid declaration rejected: %v", err)
	}
}

func TestAgentCanInvokeReadsTheAgentToolSet(t *testing.T) {
	def := &agent.AgentDef{Tools: "bash,view,-write", MCPTools: map[string]agent.MCPToolConfig{"mcp_probe": {}}}
	for _, tc := range []struct {
		tool string
		want bool
	}{
		{"bash", true},
		{"BASH", true},
		{"mcp_probe", true},
		{"write", false},
		{"unknown", false},
		{"", false},
	} {
		if got := agentCanInvoke(def, tc.tool); got != tc.want {
			t.Fatalf("agentCanInvoke(%q) = %t, want %t", tc.tool, got, tc.want)
		}
	}
	// Without a resolved agent the tool set is unknown; the gate's other
	// prerequisites still apply, so this must not block every mutation.
	if !agentCanInvoke(nil, "bash") {
		t.Fatal("a nil agent blocked an otherwise valid compensate tool")
	}
}
