package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func newDelegationPolicyCoordinator(policy agent.DelegationPolicy) *Coordinator {
	return &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Delegation: policy}},
		taskTracker: NewTaskTracker(),
	}
}

func TestDelegationPolicyRejectsOneTaskBeforeConfiguredInitialBatch(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"reader", "probe"},
		RequireExactInitialBatch: true,
	})

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "reader", Goal: "read"}})
	if err == nil || !strings.Contains(err.Error(), "first delegation must contain exactly") {
		t.Fatalf("expected initial-batch rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("rejected initial batch created %d TODOs, want none", got)
	}

	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "reader", Goal: "read"}, {Agent: "probe", Goal: "inspect"}}); err == nil || !strings.Contains(err.Error(), "already attempted") {
		t.Fatalf("second initial batch error = %v, want attempt latch", err)
	}
}

func TestDelegationPolicyBindsInitialStaticExecutionContract(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"reader", "probe"},
		RequireExactInitialBatch: true,
		BindInitialTaskContracts: true,
	})
	c.session.ContractTasks = []TaskDef{{Agent: "reader", Execution: ExecutionContract{ToolSequence: []string{"view", "submit_result"}}}, {Agent: "probe", Execution: ExecutionContract{ToolSequence: []string{"bash", "submit_result"}}}}
	got, err := c.bindInitialTaskContracts([]TaskDef{{Agent: "reader", Goal: "read"}, {Agent: "probe", Goal: "probe"}})
	if err != nil {
		t.Fatalf("bind initial contracts: %v", err)
	}
	if want := []string{"view", "submit_result"}; !reflect.DeepEqual(got[0].Execution.ToolSequence, want) {
		t.Fatalf("reader contract = %v, want %v", got[0].Execution.ToolSequence, want)
	}
	if got[0].Goal != "read" {
		t.Fatalf("goal = %q, want coordinator goal preserved", got[0].Goal)
	}
}

func TestDelegationPolicyRejectsGoalInvariantBeforeTodoCreation(t *testing.T) {
	canonical := "BEGIN CANONICAL\nSPACE\nEND CANONICAL"
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "worker", WhenGoalContains: "prepare", RequiredLiterals: []string{canonical}, ForbiddenLiterals: []string{"CHECKLIST_DOWN 0"},
	}}})
	for _, goal := range []string{
		"prepare\nCHECKLIST_DOWN 0",
		"prepare\nBEGIN CANONICAL\nSPACE\nEND CANONICAL\nCHECKLIST_DOWN 0",
		"prepare\nSPACE",
	} {
		if err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: goal}}); err == nil || !strings.Contains(err.Error(), "task-goal-invariants") {
			t.Fatalf("goal %q error = %v, want invariant rejection", goal, err)
		}
		if got := len(c.taskTracker.TodoList().Items()); got != 0 {
			t.Fatalf("rejected goal %q created %d TODOs, want none", goal, got)
		}
	}
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: "prepare\n" + canonical}}); err != nil {
		t.Fatalf("canonical goal rejected: %v", err)
	}
}

func TestDelegationPolicyChecksInvariantLiteralsInConstraints(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "worker", WhenGoalContains: "batch-", RequiredLiterals: []string{"literal-range"}, ForbiddenLiterals: []string{"sizing"},
	}}})
	task := TaskDef{Agent: "worker", Goal: "Review batch-0001", Constraints: "literal-range: abc..def"}
	if err := c.validateDelegationPolicy([]TaskDef{task}); err != nil {
		t.Fatalf("constraint-carried invariant literal rejected: %v", err)
	}
	bad := task
	bad.Constraints = "literal-range: abc..def; sizing probe"
	if err := c.validateDelegationPolicy([]TaskDef{bad}); err == nil || !strings.Contains(err.Error(), "task-goal-invariants") {
		t.Fatalf("forbidden constraint literal error = %v, want invariant rejection", err)
	}
}

func TestDelegationPolicyRejectsRawRangeDiscoveryBeforeTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "worker", WhenGoalContains: "git log", RequiredLiterals: []string{"summary"}, ForbiddenLiterals: []string{"raw output"},
	}}})
	for _, goal := range []string{
		"Run git log and return raw output as a summary",
		"Run git log and return the result",
	} {
		if err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: goal}}); err == nil || !strings.Contains(err.Error(), "task-goal-invariants") {
			t.Fatalf("goal %q error = %v, want invariant rejection", goal, err)
		}
	}
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: "Run git log and return a compact summary"}}); err != nil {
		t.Fatalf("compact discovery goal rejected: %v", err)
	}
}

func TestDelegationPolicyRejectsExecutionInvariantBeforeTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent:                    "worker",
		WhenGoalContains:         "freeze",
		RequiredToolSequence:     []string{"bash", "bash", "submit_result"},
		ForbiddenExecutionFields: []string{"tool_input_field", "tool_input_value_sequence", "tool_input_sequence"},
	}}})

	invalid := TaskDef{
		Agent: "worker",
		Goal:  "candidate freeze",
		Execution: ExecutionContract{
			ToolSequence:           []string{"bash", "bash", "submit_result"},
			ToolInputField:         "command",
			ToolInputValueSequence: []string{"first"},
		},
	}
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{invalid})
	if err == nil || !strings.Contains(err.Error(), "forbidden execution field") {
		t.Fatalf("invalid execution contract error = %v, want pre-dispatch invariant rejection", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("rejected execution contract created %d TODOs, want none", got)
	}

	valid := invalid
	valid.Execution.ToolInputField = ""
	valid.Execution.ToolInputValueSequence = nil
	if err := c.validateDelegationPolicy([]TaskDef{valid}); err != nil {
		t.Fatalf("valid execution contract rejected: %v", err)
	}
}

func TestDelegationPolicyValidatesCompletedTaskReferenceBeforeTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "auditor", WhenGoalContains: "freeze audit",
		RequiredTaskReference: &agent.TaskGoalReference{
			GoalPrefix: "runner_task_id=", Agent: "runner", TaskContains: "§3.1 candidate-freeze",
		},
	}}})
	runner := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runner", Desc: "§3.1 candidate-freeze"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(runner.ID, TaskDone, "", "done"); err != nil {
		t.Fatal(err)
	}

	valid := TaskDef{Agent: "auditor", Goal: "freeze audit\nrunner_task_id=" + runner.ID}
	if err := c.validateDelegationPolicy([]TaskDef{valid}); err != nil {
		t.Fatalf("valid task reference rejected: %v", err)
	}

	for _, goal := range []string{
		"freeze audit\nrunner_task_id=sha256-deadbeef",
		"freeze audit\nrunner_task_id=",
		"freeze audit\nrunner_task_id=" + runner.ID + "\nrunner_task_id=" + runner.ID,
	} {
		if err := c.validateDelegationPolicy([]TaskDef{{Agent: "auditor", Goal: goal}}); err == nil || !strings.Contains(err.Error(), "task reference") && !strings.Contains(err.Error(), "referenced Todo") {
			t.Fatalf("goal %q error = %v, want task-reference rejection", goal, err)
		}
	}
}

// TestDelegationPolicyChecksTaskReferenceInConstraints guards against a real
// inconsistency: RequiredLiterals/ForbiddenLiterals validate goal+Constraints
// (goalPayload) so a Coordinator may keep the literal contract in Constraints
// without bloating the human-readable goal, but RequiredTaskReference and
// RequiredTaskReferences validated only task.Goal. A dependent Todo ID kept in
// Constraints -- the same recommended practice as literals -- was therefore
// rejected as a missing task reference even though it was present.
func TestDelegationPolicyChecksTaskReferenceInConstraints(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "auditor", WhenGoalContains: "freeze audit",
		RequiredTaskReference: &agent.TaskGoalReference{
			GoalPrefix: "runner_task_id=", Agent: "runner", TaskContains: "§3.1 candidate-freeze",
		},
	}}})
	runner := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runner", Desc: "§3.1 candidate-freeze"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(runner.ID, TaskDone, "", "done"); err != nil {
		t.Fatal(err)
	}

	task := TaskDef{Agent: "auditor", Goal: "freeze audit", Constraints: "runner_task_id=" + runner.ID}
	if err := c.validateDelegationPolicy([]TaskDef{task}); err != nil {
		t.Fatalf("constraint-carried task reference rejected: %v", err)
	}
}

func TestDelegationPolicyValidatesDistinctCompletedProducerSetBeforeTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{
		Agent: "consumer", WhenGoalContains: "consensus",
		RequiredTaskReferences: []agent.TaskGoalReference{
			{GoalPrefix: "code_task_id=", Agent: "producer", TaskContains: "source candidate"},
			{GoalPrefix: "live_task_id=", Agent: "observer", TaskContains: "live observation"},
		},
	}}})
	code := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", Desc: "source candidate"}})[0]
	live := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "observer", Desc: "live observation"}})[0]
	for _, item := range []*TodoItem{code, live} {
		if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "", "done"); err != nil {
			t.Fatal(err)
		}
	}
	valid := TaskDef{Agent: "consumer", Goal: "consensus\ncode_task_id=" + code.ID + "\nlive_task_id=" + live.ID}
	if err := c.validateDelegationPolicy([]TaskDef{valid}); err != nil {
		t.Fatalf("valid producer set rejected: %v", err)
	}
	for _, goal := range []string{
		"consensus\ncode_task_id=" + code.ID,
		"consensus\ncode_task_id=" + code.ID + "\nlive_task_id=" + code.ID,
		"consensus\ncode_task_id=" + code.ID + "\nlive_task_id=missing",
	} {
		if err := c.validateDelegationPolicy([]TaskDef{{Agent: "consumer", Goal: goal}}); err == nil {
			t.Fatalf("invalid producer set accepted: %q", goal)
		}
	}
}

func TestDelegationPolicyBindsEachRecoverablePartialInitialBatchOnce(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"reader", "probe"},
		BindInitialTaskContracts: true,
	})
	c.session.ContractTasks = []TaskDef{
		{Agent: "reader", Execution: ExecutionContract{ToolSequence: []string{"view", "submit_result"}}},
		{Agent: "probe", Execution: ExecutionContract{ToolSequence: []string{"bash", "submit_result"}}},
	}

	reader, err := c.bindInitialTaskContracts([]TaskDef{{Agent: "reader", Goal: "read"}})
	if err != nil || !reflect.DeepEqual(reader[0].Execution.ToolSequence, []string{"view", "submit_result"}) {
		t.Fatalf("bind reader partial batch: tasks=%#v err=%v", reader, err)
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader", Desc: "read"}})

	probe, err := c.bindInitialTaskContracts([]TaskDef{{Agent: "probe", Goal: "probe"}})
	if err != nil || !reflect.DeepEqual(probe[0].Execution.ToolSequence, []string{"bash", "submit_result"}) {
		t.Fatalf("bind probe partial batch: tasks=%#v err=%v", probe, err)
	}

	repeated, err := c.bindInitialTaskContracts([]TaskDef{{Agent: "reader", Goal: "repeat"}})
	if err != nil || len(repeated[0].Execution.ToolSequence) != 0 {
		t.Fatalf("repeated initial worker was rebound: tasks=%#v err=%v", repeated, err)
	}
}

func TestDelegationPolicyRejectsConflictingInitialStaticContract(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"reader"},
		RequireExactInitialBatch: true,
		BindInitialTaskContracts: true,
	})
	c.session.ContractTasks = []TaskDef{{Agent: "reader", Execution: ExecutionContract{ToolSequence: []string{"submit_result"}}}}
	_, err := c.bindInitialTaskContracts([]TaskDef{{Agent: "reader", Execution: ExecutionContract{ToolSequence: []string{"view", "submit_result"}}}})
	if err == nil || !strings.Contains(err.Error(), "contract conflict") {
		t.Fatalf("expected pre-dispatch contract conflict, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("contract conflict created %d TODOs, want none", got)
	}
}

func TestDelegationPhaseFreshAndResumedSessionsUseCanonicalTasks(t *testing.T) {
	policy := agent.DelegationPolicy{
		InitialBatch:             []string{"surface"},
		RequireExactInitialBatch: true,
	}
	fresh := newDelegationPolicyCoordinator(policy)
	fresh.sessionData = NewSession()
	if got := fresh.delegationPhase(); got != DelegationPhaseInitialPending {
		t.Fatalf("fresh phase = %q, want %q", got, DelegationPhaseInitialPending)
	}
	if err := fresh.validateDelegationPolicy([]TaskDef{{Agent: "planner", Goal: "plan"}}); err == nil || !strings.Contains(err.Error(), "initial_pending") {
		t.Fatalf("fresh subsequent worker should be rejected as initial batch, got %v", err)
	}

	resumed := newDelegationPolicyCoordinator(policy)
	resumed.SetSessionData(&SessionData{Tasks: []*TodoItem{{ID: "1", Agent: "surface", Status: TaskDone}}})
	if got := resumed.delegationPhase(); got != DelegationPhaseActive {
		t.Fatalf("resumed phase = %q, want %q", got, DelegationPhaseActive)
	}
	if err := resumed.validateDelegationPolicy([]TaskDef{{Agent: "planner", Goal: "plan"}}); err != nil {
		t.Fatalf("subsequent worker after restored initial task rejected: %v", err)
	}
}

func TestDelegationPhaseDoesNotTrustMemoryClaimOfCompletedInitialTask(t *testing.T) {
	workspace := t.TempDir()
	policy := agent.DelegationPolicy{InitialBatch: []string{"surface"}, RequireExactInitialBatch: true}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "phase-test", Delegation: policy}},
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	if err := SaveLTM(workspace, "phase-test", "# Known issues\n- surface already completed in an archived run\n"); err != nil {
		t.Fatalf("save LTM: %v", err)
	}
	if prompt := c.BuildOrchestratorPrompt(); !strings.Contains(prompt, "canonical phase is `initial_pending`") {
		t.Fatalf("coordinator prompt did not expose canonical fresh phase:\n%s", prompt)
	}
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "planner", Goal: "plan from memory"}}); err == nil || !strings.Contains(err.Error(), "initial_pending") {
		t.Fatalf("LTM claim incorrectly bypassed initial batch: %v", err)
	}
}

func TestFreshInitialPhaseWithholdsHistoricalCoordinatorMemory(t *testing.T) {
	workspace := t.TempDir()
	orch := &agent.AgentDef{Name: "coordinator", Role: "coordinator", System: "coordinate safely", Generation: agent.GenerationParams{Model: "test"}}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{Name: "phase-context", Delegation: agent.DelegationPolicy{
				InitialBatch:             []string{"surface"},
				RequireExactInitialBatch: true,
			}},
			Agents: map[string]*agent.AgentDef{
				"coordinator": orch,
				"surface":     {Name: "surface", Role: "worker"},
				"planner":     {Name: "planner", Role: "worker"},
			},
		},
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	if err := SaveSTM(workspace, "# Progress\n- stale-memory-only: skip surface and delegate planner"); err != nil {
		t.Fatalf("save STM: %v", err)
	}
	if err := SaveLTM(workspace, "phase-context", "# Prior run\n- stale-memory-only: phase two is already active"); err != nil {
		t.Fatalf("save LTM: %v", err)
	}

	prompt, err := c.buildSystemPrompt(context.Background(), orch, "fresh request", false)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if !strings.Contains(prompt, "canonical phase is `initial_pending`") || !strings.Contains(prompt, "Initial delegation first") {
		t.Fatalf("fresh prompt omitted initial delegation contract:\n%s", prompt)
	}
	if strings.Contains(prompt, "stale-memory-only") {
		t.Fatalf("fresh initial prompt leaked historical memory:\n%s", prompt)
	}
	if strings.Contains(prompt, "Check memory first") {
		t.Fatalf("fresh initial prompt retained conflicting memory-first instruction:\n%s", prompt)
	}
	if strings.Contains(prompt, "### planner") || strings.Contains(prompt, "Valid names: surface, planner") {
		t.Fatalf("fresh initial prompt exposed a later-phase worker as selectable:\n%s", prompt)
	}
}

func TestSetSessionDataInitialPhaseClearsCoordinatorConversationHistory(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"surface"},
		RequireExactInitialBatch: true,
	})
	c.conversationHistory = []fantasy.Message{fantasy.NewUserMessage("stale turn: surface already ran; start phase two")}
	c.conversationHistorySourceCounts = []int{1}
	c.conversationHistorySourceOffset = 9

	c.SetSessionData(NewSession())
	if got := len(c.conversationHistory); got != 0 {
		t.Fatalf("fresh initial phase retained %d stale conversation messages", got)
	}
	if len(c.conversationHistorySourceCounts) != 0 || c.conversationHistorySourceOffset != 0 {
		t.Fatalf("fresh initial history metadata = counts=%v offset=%d, want cleared", c.conversationHistorySourceCounts, c.conversationHistorySourceOffset)
	}
}

func TestFreshSessionDisablesHistoricalMemoryForCoordinatorLifetime(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{})
	if c.historicalMemoryDisabled() {
		t.Fatal("ordinary coordinator unexpectedly disables historical memory")
	}
	c.conversationHistory = []fantasy.Message{fantasy.NewUserMessage("stale completed review")}
	c.conversationHistorySourceCounts = []int{1}

	c.SetFreshSession(true)
	if !c.historicalMemoryDisabled() {
		t.Fatal("fresh session may inject archived historical memory")
	}
	if len(c.conversationHistory) != 0 {
		t.Fatal("fresh session retained coordinator conversation history")
	}

	// Event-store initialization consumes freshSession, but the fresh run must
	// continue withholding the archive for its remaining coordinator/worker
	// prompts.
	c.freshSession.Store(false)
	if !c.historicalMemoryDisabled() {
		t.Fatal("fresh session re-enabled historical memory after branch setup")
	}
}

func TestDelegationPhasePersistsWhenInitialBatchIsAccepted(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		InitialBatch:             []string{"surface"},
		RequireExactInitialBatch: true,
	})
	c.sessionData = NewSession()
	initial := []TaskDef{{Agent: "surface", Goal: "discover"}}
	if err := c.validateDelegationPolicy(initial); err != nil {
		t.Fatalf("valid fresh initial batch rejected: %v", err)
	}
	c.markInitialDelegationAccepted()
	if got := c.delegationPhase(); got != DelegationPhaseActive {
		t.Fatalf("phase after accepted initial batch = %q, want %q", got, DelegationPhaseActive)
	}
	if got := c.sessionData.DelegationPhase; got != DelegationPhaseActive {
		t.Fatalf("durable phase = %q, want %q", got, DelegationPhaseActive)
	}
}

func TestDelegationPolicyRequiresConfiguredInitialCoordinatorTool(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{InitialCoordinatorTool: "agent"})
	if got := c.initialCoordinatorToolDenial("", "view"); !strings.Contains(got, "first tool call") {
		t.Fatalf("initial coordinator tool denial = %q, want a denial", got)
	}
	if got := c.initialCoordinatorToolDenial("", "agent"); got != "" {
		t.Fatalf("configured initial tool denied: %q", got)
	}
	if got := c.initialCoordinatorToolDenial("worker", "view"); got != "" {
		t.Fatalf("worker tool incorrectly subject to coordinator gate: %q", got)
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader", Desc: "initial task"}})
	if got := c.initialCoordinatorToolDenial("", "view"); got != "" {
		t.Fatalf("coordinator tool incorrectly denied after initial delegation: %q", got)
	}
}

func TestDelegationPolicyRejectsRedispatchAfterSuccessfulTerminalResult(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		NoRedispatchAfterSuccess: []string{"reader", "probe"},
	})
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader", Desc: "read contract"}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "typed success")

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "reader", Goal: "duplicate read"},
		{Agent: "probe", Goal: "independent probe"},
	})
	if err == nil || !strings.Contains(err.Error(), "may not be redispatched") {
		t.Fatalf("expected successful-worker redispatch rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 1 {
		t.Fatalf("rejected duplicate batch changed existing TODOs: got %d, want 1", got)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("successful task status changed to %s", got)
	}
}

func TestDelegationPolicyTaskDoneRealWorkerConsumesSlotWithoutTypedResultStatus(t *testing.T) {
	cases := map[string]func(*TodoItem){
		"nil typed result": func(*TodoItem) {},
		"recovered protocol with empty status": func(item *TodoItem) {
			item.TypedResult = &TaskResult{Agent: item.Agent, Source: "recovered_protocol"}
		},
	}

	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			for _, policyRepair := range []bool{false, true} {
				t.Run(fmt.Sprintf("policy_repair=%t", policyRepair), func(t *testing.T) {
					tracker := NewTaskTracker()
					item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "completed review"}})[0]
					configure(item)
					tracker.TodoList().UpdateStatus(item.ID, TaskDone, "worker completed")

					c := &Coordinator{
						taskTracker: tracker,
						session: &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{
							NoRedispatchAfterSuccess: []string{"reviewer"},
						}}},
					}
					if policyRepair {
						c.coordinatorPolicyRepairsAttempt.Store(1)
					}

					_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "reviewer", Goal: "duplicate review"}})
					if err == nil {
						t.Fatal("duplicate dispatch was accepted")
					}
					if got := len(tracker.TodoList().Items()); got != 1 {
						t.Fatalf("duplicate dispatch created %d TODOs, want 1", got)
					}
				})
			}
		})
	}
}

func TestDelegationPolicyRuntimeOwnedSuccessDoesNotConsumeProtectedWorkerSlot(t *testing.T) {
	cases := map[string]func(*TodoItem){
		"action": func(item *TodoItem) {
			item.Action = &Action{Capability: "structured-actions", Type: "produce-workset"}
		},
		"structured coordinator": func(item *TodoItem) {
			item.Execution.Steps = []ExecutionStep{{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce}}
		},
		"runtime result": func(item *TodoItem) {
			item.TypedResult.Source = "runtime"
		},
	}

	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			for _, policyRepair := range []bool{false, true} {
				t.Run(fmt.Sprintf("policy_repair=%t", policyRepair), func(t *testing.T) {
					tracker := NewTaskTracker()
					item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "produce workset"}})[0]
					item.TypedResult = &TaskResult{Agent: "reviewer", Status: TaskResultStatusSuccess, Source: "submitted"}
					configure(item)
					tracker.TodoList().UpdateStatus(item.ID, TaskDone, "runtime completed")
					c := &Coordinator{
						taskTracker: tracker,
						session:     &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{NoRedispatchAfterSuccess: []string{"reviewer"}}}},
					}
					if policyRepair {
						c.coordinatorPolicyRepairsAttempt.Store(1)
					}
					if err := c.validateDelegationPolicy([]TaskDef{{Agent: "reviewer", Goal: "review workset"}}); err != nil {
						t.Fatalf("runtime-owned task consumed protected worker slot: %v", err)
					}
				})
			}
		})
	}
}

func TestDelegationPolicyRepairIgnoresRuntimeOwnedTaskStates(t *testing.T) {
	resolutionStatuses := map[string]TaskStatus{
		"done":       TaskDone,
		"skipped":    TaskSkipped,
		"failed":     TaskError,
		"superseded": TaskError,
		"reconciled": TaskError,
		"waived":     TaskError,
	}
	shapes := map[string]func(*TodoItem){
		"action": func(item *TodoItem) {
			item.Action = &Action{Capability: "structured-actions", Type: "produce-workset"}
		},
		"structured coordinator": func(item *TodoItem) {
			item.Execution.Steps = []ExecutionStep{{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce}}
		},
		"runtime result": func(item *TodoItem) {
			item.TypedResult = &TaskResult{Agent: item.Agent, Status: TaskResultStatusFailed, Source: "runtime"}
		},
	}

	for shape, configure := range shapes {
		for resolution, status := range resolutionStatuses {
			t.Run(fmt.Sprintf("%s/%s", shape, resolution), func(t *testing.T) {
				tracker := NewTaskTracker()
				item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "runtime task"}})[0]
				configure(item)
				item.PlanTaskID = "produce-workset"
				item.ContractID = "produce-workset"
				item.ParentID = "workflow-parent"
				item.Status = status
				if resolution != "done" && resolution != "skipped" && resolution != "failed" {
					item.Resolution = &TaskResolution{Status: resolution, ResolvedBy: "runtime"}
				}

				c := &Coordinator{
					taskTracker: tracker,
					session:     &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{NoRedispatchAfterSuccess: []string{"reviewer"}}}},
				}
				c.coordinatorPolicyRepairsAttempt.Store(1)
				if err := c.validateDelegationPolicy([]TaskDef{{Agent: "reviewer", Goal: "review workset"}}); err != nil {
					t.Fatalf("runtime-owned %s task affected policy repair: %v", shape, err)
				}
			})
		}
	}
}

func TestDelegationPolicyRepairRealWorkerStillBlocksRuntimeMixedRedispatch(t *testing.T) {
	tracker := NewTaskTracker()
	worker := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "completed review"}})[0]
	worker.TypedResult = &TaskResult{Agent: worker.Agent, Status: TaskResultStatusSuccess, Source: "submitted"}
	tracker.TodoList().UpdateStatus(worker.ID, TaskDone, "worker completed")

	runtimeItem := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "runtime follow-up"}})[0]
	runtimeItem.Execution.Steps = []ExecutionStep{{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce}}
	runtimeItem.Status = TaskError

	c := &Coordinator{
		taskTracker: tracker,
		session:     &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{NoRedispatchAfterSuccess: []string{"reviewer"}}}},
	}
	c.coordinatorPolicyRepairsAttempt.Store(1)
	if err := c.validateDelegationPolicy([]TaskDef{{Agent: "reviewer", Goal: "redispatch review"}}); err == nil || !strings.Contains(err.Error(), "completed workers may not be redispatched") {
		t.Fatalf("mixed worker/runtime redispatch error = %v, want completed-worker rejection", err)
	}
}

type delegationWorksetActionProvider struct{}

func (delegationWorksetActionProvider) Validate(Action) error { return nil }

func (delegationWorksetActionProvider) Execute(ctx context.Context, _ Action) (interface{}, error) {
	env := ActionEnvironmentFromContext(ctx)
	if err := os.WriteFile(filepath.Join(env.Workspace, "workset.json"), []byte(`{"schema_version":1,"items":[{"key":"one","bindings":{"name":"one"}},{"key":"two","bindings":{"name":"two"}},{"key":"three","bindings":{"name":"three"}}]}`), 0o644); err != nil {
		return nil, err
	}
	return ActionResult{Artifacts: []ArtifactRef{{Path: "workset.json", Kind: "workset_manifest", Description: "workset-manifest"}}}, nil
}

func TestDelegationPolicyAdmitsRuntimeProducerFanOutAndPreservesFinish(t *testing.T) {
	const worksetSize = 3
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:         "delegation-workset-policy",
			Workflow:     agent.WorkflowConfig{Phases: []string{"prepare", "audit", "execute", "verify"}},
			Capabilities: agent.CapabilityConfig{Required: []string{"structured-actions"}},
			Delegation:   agent.DelegationPolicy{NoRedispatchAfterSuccess: []string{"reviewer"}},
			Verification: agent.VerificationConfig{Required: true},
		},
		Agents: map[string]*agent.AgentDef{
			"preparer": {Name: "preparer", Role: "worker"},
			"auditor":  {Name: "auditor", Role: "worker"},
			"reviewer": {Name: "reviewer", Role: "worker"},
			"verifier": {Name: "verifier", Role: "worker"},
		},
	}
	session.ProviderRegistry = NewProviderRegistry()
	session.ProviderRegistry.Register("structured-actions", delegationWorksetActionProvider{})
	session.ContractTasks = []TaskDef{
		{ID: "prepare", Agent: "preparer", Phase: PhasePrepare},
		{ID: "audit", Agent: "auditor", Phase: PhaseAudit},
		{ID: "produce-workset", Agent: "reviewer", Phase: PhaseExecute, Optional: true, Action: &Action{Capability: "structured-actions", Type: "produce-workset"}},
		{ID: "review-workset", Agent: "reviewer", Phase: PhaseExecute, VerifySpec: &VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
		{ID: "verify", Agent: "verifier", Phase: PhaseVerify, VerifySpec: &VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
	}
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}

	tracker := NewTaskTracker()
	c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: w, executionRunID: "run-delegation-policy", sessionData: NewSession()}
	producer := TaskDef{ID: "produce-workset", Agent: "reviewer", Goal: "produce workset", Phase: PhaseExecute, ContractID: "produce-workset", Action: session.ContractTasks[2].Action}
	producerItem := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: producer.ID, Phase: producer.Phase, ContractID: producer.ContractID, Action: producer.Action, Agent: producer.Agent, Desc: producer.Goal}})[0]
	if _, err := c.executeTask(context.Background(), producer, producerItem.ID); err != nil {
		t.Fatalf("runtime producer action failed: %v", err)
	}
	if producerItem.TypedResult == nil || producerItem.TypedResult.Source != "runtime" {
		t.Fatalf("producer result = %#v, want runtime-owned result", producerItem.TypedResult)
	}
	if err := w.observe([]*TodoItem{producerItem}); err != nil {
		t.Fatal(err)
	}

	review := TaskDef{
		ID: "review-workset", Agent: "reviewer", Goal: "review workset", Phase: PhaseExecute, ContractID: "review-workset",
		FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "produce-workset", Artifact: "workset-manifest"}, GoalTemplate: "review {name}"},
	}
	expanded, err := c.expandFanOutTasks([]TaskDef{review})
	if err != nil {
		t.Fatalf("expand review workset: %v", err)
	}
	if len(expanded) != worksetSize {
		t.Fatalf("expanded children = %d, want %d", len(expanded), worksetSize)
	}
	if err := c.validateDelegationPolicy(expanded); err != nil {
		t.Fatalf("first review dispatch rejected after runtime producer: %v", err)
	}
	for _, child := range expanded {
		if child.WorksetBinding == nil || child.WorksetBinding.ParentTaskID != review.ID {
			t.Fatalf("child binding = %#v, want parent %q", child.WorksetBinding, review.ID)
		}
	}

	ids := tracker.TodoList().ReserveIDs(len(expanded))
	receipts, err := buildWorksetReceipts(expanded, ids, c.executionRunID)
	if err != nil {
		t.Fatalf("build workset receipt: %v", err)
	}
	worksetReceipt := receipts[expanded[0].WorksetBinding.WorksetID]
	childItems := make([]*TodoItem, 0, len(expanded))
	for index, child := range expanded {
		item := todoItemFromSpec(TodoSpec{
			PlanTaskID: child.ID, Phase: child.Phase, ContractID: child.ContractID, Agent: child.Agent, Desc: child.Goal,
			WorksetBinding: child.WorksetBinding, VerifySpec: child.VerifySpec,
		}, ids[index])
		childItems = append(childItems, item)
	}
	tracker.TodoList().AddReserved(childItems)
	for index, item := range childItems {
		if item.ID != ids[index] {
			t.Fatalf("child ID = %q, want reserved %q", item.ID, ids[index])
		}
		if index == 0 {
			item.WorksetReceipt = worksetReceipt
		}
		item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "review complete"}
		item.VerifyResult = &VerificationResult{ExitCode: 0}
		tracker.TodoList().UpdateStatus(item.ID, TaskDone, "review complete")
	}
	if states := c.WorksetGroupStates(); len(states) != 1 || states[0].Expected != worksetSize || states[0].Completed != worksetSize || states[0].Verified != worksetSize || states[0].State != "complete" {
		t.Fatalf("workset state = %#v, want %d/%d verified complete", states, worksetSize, worksetSize)
	}
	if err := w.observe(childItems); err != nil {
		t.Fatalf("workflow execute/verify transition: %v", err)
	}
	if got := w.State(); got != PhaseVerify {
		t.Fatalf("workflow state after fan-out = %s, want VERIFY", got)
	}
	acceptance := VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: review.ID, WorksetRequireTerminal: true, WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess}}
	c.acceptanceSpec = &AcceptanceSpec{Verifications: []VerificationSpec{acceptance}}
	verified, err := c.executeWorksetCompleteVerification(context.Background(), acceptance)
	if err != nil || verified == nil || verified.ExitCode != 0 {
		t.Fatalf("workset acceptance verification = %#v, err=%v", verified, err)
	}

	verifyItem := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "verify", Phase: PhaseVerify, ContractID: "verify", Agent: "verifier", Desc: "verify workset", VerifySpec: session.ContractTasks[4].VerifySpec}})[0]
	verifyItem.TypedResult = &TaskResult{TaskID: verifyItem.ID, Agent: verifyItem.Agent, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "verified"}
	verifyItem.VerifyResult = &VerificationResult{ExitCode: 0}
	tracker.TodoList().UpdateStatus(verifyItem.ID, TaskDone, "verified")
	if err := w.observe([]*TodoItem{verifyItem}); err != nil {
		t.Fatalf("workflow verify success: %v", err)
	}

	beforeSecondDispatch := len(tracker.TodoList().Items())
	expandedAgain, err := c.expandFanOutTasks([]TaskDef{review})
	if err != nil {
		t.Fatalf("re-expand review workset: %v", err)
	}
	if err := c.validateDelegationPolicy(expandedAgain); err == nil || !strings.Contains(err.Error(), "may not be redispatched") {
		t.Fatalf("second review dispatch error = %v, want no-redispatch rejection", err)
	}
	if got := len(tracker.TodoList().Items()); got != beforeSecondDispatch {
		t.Fatalf("rejected second review dispatch created %d new TODOs", got-beforeSecondDispatch)
	}
	response, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"workset reviewed"}`})
	if err != nil || response.IsError || !c.finishCalled.Load() {
		t.Fatalf("finish response = %#v, err=%v, finish_called=%v", response, err, c.finishCalled.Load())
	}
}

func TestDelegationPolicyRejectsWorkerOutsideAllowlist(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		AllowedWorkers: []string{"reader", "probe"},
	})
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "helper", Goal: "retrieve a result"}})
	if err == nil || !strings.Contains(err.Error(), "outside the configured allowlist") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("allowlist rejection created %d TODOs, want none", got)
	}
}

func TestDelegationPolicyRejectsForbiddenContextFiles(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{ForbidContextFiles: true})
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{
		Agent:        "reader",
		Goal:         "read the handoff",
		ContextFiles: []string{"handoff.md"},
	}})
	if err == nil || !strings.Contains(err.Error(), "context_files are forbidden") {
		t.Fatalf("expected forbidden-context-files rejection, got %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("context-file policy rejection created %d TODOs, want none", got)
	}
}

func TestDelegationPolicySerializesConcurrentStateChangingTasks(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"writer-a": {Name: "writer-a", Role: "worker", SideEffect: "workspace_write"},
				"writer-b": {Name: "writer-b", Role: "worker", SideEffect: "infra_mutation"},
			},
		},
		taskTracker: NewTaskTracker(),
	}

	tasks := c.serializeMutationTasks([]TaskDef{
		{Agent: "writer-a", Goal: "write one bounded artifact"},
		{Agent: "writer-b", Goal: "mutate one external target"},
	})
	if len(tasks) != 2 || len(tasks[1].DependsOn) != 1 || tasks[1].DependsOn[0] != 0 {
		t.Fatalf("mutation batch was not serialized: %#v", tasks)
	}
	if len(tasks[0].DependsOn) != 0 {
		t.Fatalf("first mutation unexpectedly gained dependencies: %#v", tasks[0].DependsOn)
	}
}
