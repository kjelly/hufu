package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// Commitment is bound to the tool that actually mutates
// (docs/architecture/decision-runtime.md §30, plan Stage 6).
//
// These tests exercise the real tool boundary rather than the gate function,
// because the guarantee is about tool processes that never start.

func boundaryCoordinator(t *testing.T, def *agent.AgentDef) *Coordinator {
	t.Helper()
	c := &Coordinator{
		sessionTime:  time.Now(),
		eventJournal: &memoryJournal{},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "commitment-boundary"},
			Agents:    map[string]*agent.AgentDef{def.Name: def},
		},
	}
	return c
}

func armedBoundaryTask(t *testing.T, c *Coordinator, policy CommitGatePolicy) TaskDef {
	t.Helper()
	task := mutatingTask()
	task.Agent = "deployer"
	if err := c.armDiscipline(context.Background(), "todo-1", task,
		disciplinePolicy(policy, StopPolicy{}, ReplanPolicy{}), &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	return task
}

func boundaryToolContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, todoIDKey{}, "todo-1")
	return tools.SetToolsAllowed(ctx, []string{"bash", "view", "write", "delete-bridge"})
}

// Acceptance: a blocked mutation starts zero tool processes.
func TestBlockedMutationStartsZeroToolProcesses(t *testing.T) {
	def := deployerAgent()
	def.ToolRecovery = nil
	c := boundaryCoordinator(t, def)
	armedBoundaryTask(t, c, CommitGatePolicy{RequireRollback: true})

	inner := &recordingTool{name: "bash"}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	resp, err := gated.Run(boundaryToolContext(context.Background()),
		fantasy.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"ovs-vsctl add-br br0"}`})
	if err != nil {
		t.Fatalf("a commit gate denial must not abort the stream: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "policy_blocked") {
		t.Fatalf("response = %+v, want a policy_blocked tool error", resp)
	}
	if inner.ran || inner.calls != 0 {
		t.Fatalf("blocked mutation ran the tool: ran=%t calls=%d", inner.ran, inner.calls)
	}
}

// A read-only observation reaches its tool: commitment is evaluated at the
// mutation boundary, so observing state is never mistaken for committing it.
func TestReadOnlyObservationReachesToolUnderBlockedCommitGate(t *testing.T) {
	def := deployerAgent()
	def.ToolRecovery = nil
	c := boundaryCoordinator(t, def)
	armedBoundaryTask(t, c, CommitGatePolicy{RequireRollback: true})

	inner := &recordingTool{name: "bash", resp: fantasy.NewTextResponse("hosts")}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	resp, err := gated.Run(boundaryToolContext(context.Background()),
		fantasy.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"cat /etc/hosts"}`})
	if err != nil {
		t.Fatalf("read-only observation failed: %v", err)
	}
	if resp.IsError {
		t.Fatalf("read-only observation denied: %+v", resp)
	}
	if !inner.ran {
		t.Fatal("read-only observation never reached its tool")
	}
}

// A satisfied contract lets the mutation through the same boundary.
func TestSatisfiedCommitContractAdmitsMutationAtToolBoundary(t *testing.T) {
	c := boundaryCoordinator(t, deployerAgent())
	armedBoundaryTask(t, c, CommitGatePolicy{RequireRollback: true})

	inner := &recordingTool{name: "bash", resp: fantasy.NewTextResponse("br0 created")}
	gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]

	resp, err := gated.Run(boundaryToolContext(context.Background()),
		fantasy.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"ovs-vsctl add-br br0"}`})
	if err != nil {
		t.Fatalf("admitted mutation failed: %v", err)
	}
	if resp.IsError {
		t.Fatalf("a satisfied commit contract was denied: %+v", resp)
	}
	if !inner.ran {
		t.Fatal("admitted mutation never reached its tool")
	}
}

// Acceptance: unknown side-effect state never auto-replays. The checkpoint
// stops the run, and the repair controller refuses to retry.
func TestUnknownSideEffectStateNeverAutoReplays(t *testing.T) {
	decision := EvaluateCheckpoint(StopPolicy{}, ReplanPolicy{}, CheckpointState{
		Attempt: 1, ToolCalls: 1, SideEffectState: RecoveryStateUnknown,
	})
	if decision.Action != CheckpointStop || decision.Reason != ReasonReconcileUnknownState {
		t.Fatalf("checkpoint = %#v, want a stop on unaccountable side-effect state", decision)
	}

	controller := NewRepairController()
	for _, tc := range []struct {
		name       string
		sideEffect SideEffectClass
		reconcile  string
		wantAction RepairAction
	}{
		{name: "external write reconciles", sideEffect: SideEffectExternalWrite, reconcile: "probe", wantAction: RepairReconcile},
		{name: "external write without a probe blocks", sideEffect: SideEffectExternalWrite, wantAction: RepairBlock},
		// side_effect: unknown is non-replayable by the same definition the
		// commit gate and crash recovery use; it must not fall through to a
		// plain retry.
		{name: "unknown class reconciles", sideEffect: SideEffectUnknown, reconcile: "probe", wantAction: RepairReconcile},
		{name: "unknown class without a probe blocks", sideEffect: SideEffectUnknown, wantAction: RepairBlock},
		{name: "infra mutation reconciles", sideEffect: SideEffectInfraMutation, reconcile: "probe", wantAction: RepairReconcile},
		{name: "credential mutation reconciles", sideEffect: SideEffectCredential, reconcile: "probe", wantAction: RepairReconcile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := controller.Decide(RepairRequest{
				Task: TaskDef{
					Agent: "deployer", SideEffect: tc.sideEffect,
					Recovery: RecoveryRetry, ReconcileTool: tc.reconcile,
				},
				RecoveryState: RecoveryStateUnknown,
				Attempt:       1, MaxAttempts: 3,
			})
			if got.Action != tc.wantAction {
				t.Fatalf("decision = %#v, want %s", got, tc.wantAction)
			}
			if got.Action == RepairRetry || got.Action == RepairEscalate {
				t.Fatalf("unknown side-effect state produced an auto-replay: %#v", got)
			}
		})
	}
}

// The checkpoint reads the reconcile classification that is current when it
// fires, not the one captured when the task was armed. A reconciliation that
// lands mid-execution must be what the checkpoint judges.
func TestCheckpointReadsLiveReconcileClassification(t *testing.T) {
	c := boundaryCoordinator(t, deployerAgent())
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "deployer", Desc: "mutate", Goal: "mutate",
	}})[0]

	task := mutatingTask()
	task.Agent = "deployer"
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
	if err := c.armDiscipline(context.Background(), item.ID, task, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatalf("arm: %v", err)
	}
	// Armed with no interrupted mutation: the first checkpoint continues.
	if decision := c.recordToolCall(context.Background(), item.ID, false); decision.Action != CheckpointContinue {
		t.Fatalf("checkpoint = %#v, want continue before any reconcile classification", decision)
	}

	c.taskTracker.TodoList().SetRecoveryState(item.ID, RecoveryStateUnknown)
	decision := c.recordToolCall(context.Background(), item.ID, false)
	if decision.Action != CheckpointStop || decision.Reason != ReasonReconcileUnknownState {
		t.Fatalf("checkpoint = %#v, want a stop once the classification became unknown", decision)
	}
	if decision.State.SideEffectState != RecoveryStateUnknown {
		t.Fatalf("checkpoint state side effect = %q, want the live classification", decision.State.SideEffectState)
	}
}

// Acceptance: special execution paths use the same gate. A structured step
// that declares a mutation is admitted through the commit gate, and a blocked
// step never reaches its runner.
func TestStructuredMutateStepUsesTheCommitGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		compensate string
		wantRan    bool
	}{
		{name: "blocked without a compensating operation", wantRan: false},
		{name: "admitted with one", compensate: "delete-bridge", wantRan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := deployerAgent()
			def.ToolRecovery = nil
			if tc.compensate != "" {
				def.ToolRecovery = map[string]agent.ToolRecoveryDecl{
					"provision": {CompensateTool: tc.compensate},
				}
			}
			c := boundaryCoordinator(t, def)
			task := armedBoundaryTask(t, c, CommitGatePolicy{RequireRollback: true})

			ran := false
			admit := c.structuredStepCommitGate("todo-1", task)
			err := admit(ExecutionStep{ID: "s1", Tool: "provision", Effect: ExecutionEffectMutate})
			if err == nil {
				ran = true
			}
			if ran != tc.wantRan {
				t.Fatalf("step admitted = %t, want %t (err=%v)", ran, tc.wantRan, err)
			}
			if !tc.wantRan && !strings.Contains(err.Error(), ReasonCommitGateMissingRecovery) {
				t.Fatalf("denial = %v, want %s", err, ReasonCommitGateMissingRecovery)
			}
		})
	}
}

// A produce or validate step observes rather than commits, so it is outside
// the gate for the same reason a read-only tool call is.
func TestStructuredNonMutatingStepsAreNotGated(t *testing.T) {
	def := deployerAgent()
	def.ToolRecovery = nil
	c := boundaryCoordinator(t, def)
	task := armedBoundaryTask(t, c, CommitGatePolicy{RequireRollback: true})
	admit := c.structuredStepCommitGate("todo-1", task)

	for _, effect := range []ExecutionEffect{ExecutionEffectProduce, ExecutionEffectValidate} {
		if err := admit(ExecutionStep{ID: "s1", Tool: "provision", Effect: effect}); err != nil {
			t.Fatalf("%s step denied: %v", effect, err)
		}
	}
}

// A static runtime action is gated under its capability, and a declaration
// for the capability covers every operation it exposes.
func TestRuntimeActionGateNameAndCapabilityInheritance(t *testing.T) {
	if got := runtimeActionGateName(&Action{Capability: "ovs", Type: "create-bridge"}); got != "ovs:create-bridge" {
		t.Fatalf("gate name = %q, want ovs:create-bridge", got)
	}
	if got := runtimeActionGateName(&Action{Capability: "ovs"}); got != "ovs" {
		t.Fatalf("gate name = %q, want ovs", got)
	}
	if got := runtimeActionGateName(nil); got != "" {
		t.Fatalf("gate name = %q, want empty", got)
	}

	def := deployerAgent()
	def.ToolRecovery = map[string]agent.ToolRecoveryDecl{
		"ovs":                   {CompensateTool: "delete-bridge"},
		"ovs:delete-everything": {CompensateTool: ""},
	}
	task := TaskDef{Agent: "deployer"}
	if spec := resolveToolRecovery(def, task, "ovs:create-bridge"); spec.CompensateTool != "delete-bridge" {
		t.Fatalf("capability declaration did not cover its operation: %#v", spec)
	}
	// An exact operation entry may narrow the capability default, but an empty
	// field in it is not an override.
	if spec := resolveToolRecovery(def, task, "ovs:delete-everything"); spec.CompensateTool != "delete-bridge" {
		t.Fatalf("empty field wrongly overrode the capability default: %#v", spec)
	}
}
