package team

import (
	"context"
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
