package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func disciplineCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{sessionTime: time.Now(), eventJournal: &memoryJournal{}}
}

func disciplinePolicy(commit CommitGatePolicy, stop StopPolicy, replan ReplanPolicy) DecisionPolicy {
	return DecisionPolicy{
		IndependentJudgments: 1,
		Discipline:           DisciplinePolicy{Commit: commit, Stop: stop, Replan: replan},
	}
}

// A task that armed no discipline behaves exactly as before: both hooks are
// no-ops. This is the compatibility guarantee for every team that has not
// adopted decision profiles.
func TestDisciplineHooksAreNoOpsWhenUnarmed(t *testing.T) {
	c := disciplineCoordinator(t)
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash"); denial != "" {
		t.Fatalf("unarmed commit gate denied a tool: %q", denial)
	}
	if denial := c.checkpointDenial("todo-1"); denial != "" {
		t.Fatalf("unarmed checkpoint denied a tool: %q", denial)
	}
	if decision := c.recordToolCall(context.Background(), "todo-1", false); decision.Action != CheckpointContinue {
		t.Fatalf("unarmed checkpoint = %#v, want continue", decision)
	}
	// A nil coordinator is the same: the gate never panics on a path that has
	// no coordinator attached.
	var nilCoordinator *Coordinator
	if denial := nilCoordinator.checkpointDenial("todo-1"); denial != "" {
		t.Fatalf("nil coordinator denied a tool: %q", denial)
	}
}

func TestCheckpointSchedulerOutcomesProjectCanonicalTodoState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     string
		wantStatus TaskStatus
		wantRetry  int
	}{
		{name: "replan", action: CheckpointReplan, wantStatus: TaskPending, wantRetry: 1},
		{name: "request information", action: CheckpointRequestInformation, wantStatus: TaskPaused},
		{name: "needs human", action: CheckpointNeedsHuman, wantStatus: TaskPaused},
		{name: "stop", action: CheckpointStop, wantStatus: TaskBlocked},
		// No authorized stronger model exists in this fixture, so escalation
		// must fail closed rather than silently retrying the old model.
		{name: "unauthorized escalation", action: CheckpointEscalate, wantStatus: TaskBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewTaskTracker()
			tracker.TodoList().Restore([]*TodoItem{{ID: "todo-1", Status: TaskInProgress}})
			c := disciplineCoordinator(t)
			c.taskTracker = tracker
			discipline := &taskDiscipline{todoID: "todo-1", task: TaskDef{Escalate: true}}
			if err := c.projectCheckpointOutcome(discipline, CheckpointDecision{Action: tc.action, Detail: "fixture"}); err != nil {
				t.Fatalf("projectCheckpointOutcome: %v", err)
			}
			item := tracker.TodoList().Items()[0]
			if item.Status != tc.wantStatus || item.Retries != tc.wantRetry {
				t.Fatalf("item = %#v, want status=%s retries=%d", item, tc.wantStatus, tc.wantRetry)
			}
		})
	}
}

func TestCheckpointSchedulerOffProfileLeavesTodoUntouched(t *testing.T) {
	tracker := NewTaskTracker()
	tracker.TodoList().Restore([]*TodoItem{{ID: "todo-1", Status: TaskInProgress}})
	c := disciplineCoordinator(t)
	c.taskTracker = tracker
	if got := c.recordToolCall(context.Background(), "todo-1", false); got.Action != CheckpointContinue {
		t.Fatalf("recordToolCall = %#v, want continue", got)
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("off-profile status = %s, want %s", got, TaskInProgress)
	}
}

func TestSubmitResultCriticalContradictionCannotCommitSuccess(t *testing.T) {
	c := disciplineCoordinator(t)
	c.executionRunID = "checkpoint-submit"
	c.taskTracker = NewTaskTracker()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "checkpoint submit"}})[0]
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{
		OnCriticalAssumptionContradicted: CheckpointReplan,
	})
	if err := c.armDiscipline(context.Background(), item.ID, TaskDef{ID: item.ID, Agent: item.Agent}, policy, &DecisionRecord{
		ID: "checkpoint-decision", Assumptions: []DecisionAssumption{{ID: "A1", Critical: true}},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(
		occurrenceTestContext(c, item.ID, 1),
		toolCallForTest(`{"status":"success","summary":"done","assumption_checks":[{"assumption_id":"A1","status":"contradicted"}]}`),
	)
	if err != nil || !response.IsError {
		t.Fatalf("submit_result = %#v, %v; want checkpoint rejection", response, err)
	}
	if result := c.GetTaskResult(item.ID); result != nil {
		t.Fatalf("invalidated submit_result was committed: %#v", result)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskPending || got.TypedResult != nil {
		t.Fatalf("invalidated task projection = %#v, want pending without result", got)
	}
}

func TestCheckpointReplanRejectsLateWorkerResultAndError(t *testing.T) {
	c := disciplineCoordinator(t)
	c.executionRunID = "checkpoint-stale-worker"
	c.taskTracker = NewTaskTracker()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "checkpoint stale worker"}})[0]
	if err := c.CommitTaskTransition(context.Background(), item.ID, TaskPending, TaskInProgress, "started", "", nil); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("active worker lease is missing")
	}
	if err := c.projectCheckpointOutcome(&taskDiscipline{todoID: item.ID}, CheckpointDecision{Action: CheckpointReplan, Detail: "critical evidence changed"}); err != nil {
		t.Fatal(err)
	}
	if c.storeSubmittedTaskResultForOccurrence(identity, &TaskResult{Status: TaskResultStatusSuccess, Summary: "late success"}) {
		t.Fatal("late result was accepted after replan")
	}
	c.terminalizeTaskErrorIfUnresolved(item.ID, context.DeadlineExceeded, identity)
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskPending || got.TypedResult != nil {
		t.Fatalf("late worker changed replan projection: %#v", got)
	}
}

func TestCheckpointPauseIsNotCrashResumedButOrdinaryPauseIs(t *testing.T) {
	c := disciplineCoordinator(t)
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Status: TaskPaused, CheckpointPause: true},
		{ID: "2", Status: TaskPaused},
	})
	interrupted := c.getInterruptedTasks()
	if len(interrupted) != 1 || interrupted[0].ID != "2" {
		t.Fatalf("interrupted tasks = %#v, want only ordinary pause", interrupted)
	}
}

func TestCheckpointLifecycleProvenanceSurvivesEventReplay(t *testing.T) {
	created := &TodoItem{ID: "todo-1", Agent: "worker", Desc: "replay checkpoint", Status: TaskInProgress, Model: "fast", OccurrenceRevision: 1, DispatchID: "dispatch-old"}
	paused := cloneTodoItem(created)
	paused.Status = TaskPaused
	paused.CheckpointPause = true
	paused.OccurrenceRevision = 2
	paused.DispatchID = ""
	createdPayload, err := json.Marshal(taskTransitionPayloadWithCoordinator(created, nil))
	if err != nil {
		t.Fatal(err)
	}
	pausedPayload, err := json.Marshal(taskTransitionPayloadWithCoordinator(paused, nil))
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList([]RunEvent{
		{Type: string(EventTaskCreated), TaskID: created.ID, Payload: createdPayload},
		{Type: string(EventTaskPaused), TaskID: paused.ID, Payload: pausedPayload},
	})
	if len(replayed) != 1 || !replayed[0].CheckpointPause || replayed[0].OccurrenceRevision != 2 || replayed[0].DispatchID != "" {
		t.Fatalf("replayed checkpoint provenance = %#v", replayed)
	}
}

func TestCheckpointReadsMaterialEvidenceChangeFromDecisionJournal(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionEvidenceSealed, decisionEvent{
		DecisionID: "decision-evidence", EvidenceHash: "sealed-old", Packet: &DecisionEvidencePacket{ID: "old", Hash: "sealed-old"},
	}); err != nil {
		t.Fatal(err)
	}
	if c.materialEvidenceChanged(context.Background(), "decision-evidence", "sealed-old") {
		t.Fatal("unchanged sealed evidence reported as changed")
	}
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionEvidenceSealed, decisionEvent{
		DecisionID: "decision-evidence", EvidenceHash: "sealed-new", Packet: &DecisionEvidencePacket{ID: "new", Hash: "sealed-new"},
	}); err != nil {
		t.Fatal(err)
	}
	if !c.materialEvidenceChanged(context.Background(), "decision-evidence", "sealed-old") {
		t.Fatal("new sealed evidence did not invalidate armed evidence")
	}
}

func TestCheckpointPauseWithoutJournalKeepsFullOccurrenceProjection(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "no-journal checkpoint"}})[0]
	if err := c.CommitTaskTransition(context.Background(), item.ID, TaskPending, TaskInProgress, "started", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(context.Background(), item.ID, TaskPaused, "needs operator input", "", map[string]interface{}{
		"checkpoint_pause": true,
	}); err != nil {
		t.Fatal(err)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskPaused || !got.CheckpointPause || got.OccurrenceRevision < 2 || got.DispatchID != "" {
		t.Fatalf("no-journal checkpoint projection = %#v", got)
	}
}

// Arming fails closed when the profile requires kill criteria and the task
// declared none: stop conditions must exist before resources are spent.
func TestArmDisciplineRequiresKillCriteria(t *testing.T) {
	c := disciplineCoordinator(t)
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{RequireKillCriteria: true}, ReplanPolicy{})

	err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, nil)
	if err == nil || !strings.Contains(err.Error(), ReasonStopPolicyMissingKillCriteria) {
		t.Fatalf("armDiscipline = %v, want %s", err, ReasonStopPolicyMissingKillCriteria)
	}
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed despite failing validation")
	}
}

func TestArmDisciplinePreservesDecisionProfileProjection(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)
	if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionStarted, decisionEvent{
		DecisionID: "dec-1", TaskID: "todo-1", Profile: "standard",
	}); err != nil {
		t.Fatalf("append initial decision event: %v", err)
	}
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, disciplinePolicy(CommitGatePolicy{}, StopPolicy{}, ReplanPolicy{}), &DecisionRecord{
		ID: "dec-1", Profile: "standard",
	}); err != nil {
		t.Fatalf("armDiscipline: %v", err)
	}

	events, err := journal.ReadEvents(context.Background())
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("decision events = %d, want 2", len(events))
	}
	var armed decisionEvent
	if err := json.Unmarshal(events[1].Payload, &armed); err != nil {
		t.Fatalf("decode execution-armed event: %v", err)
	}
	if armed.Profile != "standard" {
		t.Fatalf("execution-armed profile = %q, want standard", armed.Profile)
	}
	state, err := projectDecision(context.Background(), journal, "dec-1")
	if err != nil {
		t.Fatalf("project decision: %v", err)
	}
	if state.Profile != "standard" {
		t.Fatalf("projected profile = %q, want standard", state.Profile)
	}
}

// A blocked commit gate denies the tool before it starts, and the denial is
// recorded (spec §30.2, test matrix L).
func TestCommitGateDeniesBeforeToolStart(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	task := TaskDef{ID: "t1", SideEffect: SideEffectInfraMutation, Recovery: RecoveryRetry}
	policy := disciplinePolicy(CommitGatePolicy{RequireReconcile: true}, StopPolicy{}, ReplanPolicy{})
	record := &DecisionRecord{ID: "dec-1", EvidenceHash: "hash-1"}

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, record); err != nil {
		t.Fatal(err)
	}
	denial := c.commitGateDenial(context.Background(), "todo-1", "bash")
	if denial == "" {
		t.Fatal("commit gate allowed a mutation with no reconcile path")
	}
	if !strings.Contains(denial, "policy_blocked") || !strings.Contains(denial, ReasonCommitGateMissingReconcile) {
		t.Fatalf("denial = %q, want policy_blocked with the reason code", denial)
	}
	if journal.count(agent.EventCommitGateBlocked) != 1 {
		t.Fatalf("events = %v, want commit_gate_blocked", journal.typesOf())
	}
}

// A satisfied gate allows the tool and is not re-evaluated per call: the
// prerequisites are properties of the task contract.
func TestCommitGateAllowsSatisfiedTask(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	task := mutatingTask()
	policy := disciplinePolicy(
		CommitGatePolicy{RequireReconcile: true, RequireVerification: true, RequireObservability: true},
		StopPolicy{}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if denial := c.commitGateDenial(context.Background(), "todo-1", "bash"); denial != "" {
			t.Fatalf("call %d denied: %q", i, denial)
		}
	}
	if journal.count(agent.EventCommitGateBlocked) != 0 {
		t.Fatal("a satisfied gate emitted a blocked event")
	}
}

// A read-only task is outside the gate's scope entirely.
func TestCommitGateIgnoresNonMutatingTasks(t *testing.T) {
	c := disciplineCoordinator(t)
	task := TaskDef{ID: "t1", SideEffect: SideEffectNone}
	policy := disciplinePolicy(CommitGatePolicy{RequireReconcile: true, RequireEvidence: true}, StopPolicy{}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, nil); err != nil {
		t.Fatal(err)
	}
	if denial := c.commitGateDenial(context.Background(), "todo-1", "view"); denial != "" {
		t.Fatalf("read-only task denied: %q", denial)
	}
}

// The checkpoint fires on the configured cadence and stops the task; once
// stopped, further tool calls are refused rather than continuing on momentum.
func TestCheckpointStopsTaskAndRefusesFurtherCalls(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{
		CheckpointEvery: 2,
		KillCriteria:    []KillCriterion{{ID: "calls", Kind: agent.KillKindToolCalls, Threshold: 2}},
	}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}

	// First call: no checkpoint is due yet.
	if decision := c.recordToolCall(context.Background(), "todo-1", false); decision.Action != CheckpointContinue {
		t.Fatalf("call 1 = %#v, want continue", decision)
	}
	if denial := c.checkpointDenial("todo-1"); denial != "" {
		t.Fatalf("task refused a tool before any checkpoint fired: %q", denial)
	}

	// Second call: the checkpoint fires and the criterion is met.
	decision := c.recordToolCall(context.Background(), "todo-1", false)
	if decision.Action != CheckpointStop || decision.Criterion != "calls" {
		t.Fatalf("call 2 = %#v, want a stop on the calls criterion", decision)
	}
	if denial := c.checkpointDenial("todo-1"); denial == "" {
		t.Fatal("a stopped task still permits tool calls")
	}
	if journal.count(agent.EventKillCriterionTriggered) != 1 {
		t.Fatalf("events = %v, want kill_criterion_triggered", journal.typesOf())
	}
}

// A checkpoint that decides to replan records a replan event rather than a
// kill, so the two outcomes stay distinguishable in the log.
func TestCheckpointReplanEmitsReplanEvent(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	policy := disciplinePolicy(CommitGatePolicy{},
		StopPolicy{CheckpointEvery: 1},
		ReplanPolicy{OnCriticalAssumptionContradicted: agent.ReplanReplan})
	record := &DecisionRecord{
		ID: "dec-1", EvidenceHash: "hash-1",
		Assumptions: []DecisionAssumption{{ID: "A1", Critical: true, Status: AssumptionContradicted}},
	}
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, record); err != nil {
		t.Fatal(err)
	}

	decision := c.recordToolCall(context.Background(), "todo-1", false)
	if decision.Action != CheckpointReplan || decision.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("decision = %#v, want a replan on a contradicted critical assumption", decision)
	}
	if journal.count(agent.EventReplanRequested) != 1 {
		t.Fatalf("events = %v, want replan_requested", journal.typesOf())
	}
	if journal.count(agent.EventKillCriterionTriggered) != 0 {
		t.Fatal("a replan was recorded as a kill")
	}
}

// Checkpoint evaluation makes zero model calls. A checkpoint that re-asked a
// model whether its assumptions still hold would turn execution into a second
// inference loop (spec §29.1).
func TestCheckpointMakesNoModelCalls(t *testing.T) {
	c := disciplineCoordinator(t)
	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{
		CheckpointEvery: 1,
		KillCriteria:    []KillCriterion{{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 1_000_000}},
	}, ReplanPolicy{})
	record := &DecisionRecord{
		ID: "dec-1",
		Assumptions: []DecisionAssumption{
			{ID: "A1", Critical: true, Status: AssumptionUnknown},
			{ID: "A2", Critical: true, Status: AssumptionSupported},
		},
	}
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, record); err != nil {
		t.Fatal(err)
	}

	// The coordinator has no provider, sidecar or model configured. Any model
	// call during checkpoint evaluation would fail or panic here.
	for i := 0; i < 5; i++ {
		if decision := c.recordToolCall(context.Background(), "todo-1", false); decision.Action != CheckpointContinue {
			t.Fatalf("checkpoint %d = %#v, want continue", i, decision)
		}
	}
}

// Disarming releases the contract so a later task is not governed by an
// earlier one's stop policy.
func TestDisarmDiscipline(t *testing.T) {
	c := disciplineCoordinator(t)
	policy := disciplinePolicy(CommitGatePolicy{RequireReconcile: true}, StopPolicy{}, ReplanPolicy{})
	task := TaskDef{ID: "t1", SideEffect: SideEffectInfraMutation}

	if err := c.armDiscipline(context.Background(), "todo-1", task, policy, nil); err != nil {
		t.Fatal(err)
	}
	if c.commitGateDenial(context.Background(), "todo-1", "bash") == "" {
		t.Fatal("armed gate did not deny")
	}
	c.disarmDiscipline("todo-1")
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash"); denial != "" {
		t.Fatalf("disarmed gate still denies: %q", denial)
	}
}

// A checkpoint that decides to replan because the decision's own assumptions
// failed must also supersede the decision, once (spec §31, §35).
func TestCheckpointReplanMarksTheDecisionStale(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	policy := disciplinePolicy(CommitGatePolicy{},
		StopPolicy{CheckpointEvery: 1},
		ReplanPolicy{OnCriticalAssumptionContradicted: agent.ReplanReplan})
	record := &DecisionRecord{
		ID: "dec-1", EvidenceHash: "hash-1",
		Assumptions: []DecisionAssumption{{ID: "A1", Critical: true, Status: AssumptionContradicted}},
	}
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, record); err != nil {
		t.Fatal(err)
	}

	if decision := c.recordToolCall(context.Background(), "todo-1", false); decision.Action != CheckpointReplan {
		t.Fatalf("decision = %#v, want replan", decision)
	}
	if journal.count(agent.EventReplanRequested) != 1 {
		t.Fatalf("replan events = %d, want 1", journal.count(agent.EventReplanRequested))
	}
	if journal.count(agent.EventDecisionInvalidated) != 1 {
		t.Fatalf("invalidation events = %d, want the decision superseded once",
			journal.count(agent.EventDecisionInvalidated))
	}

	// A second checkpoint on the same condition must not append a second
	// invalidation: the decision is already superseded.
	c.recordToolCall(context.Background(), "todo-1", false)
	if journal.count(agent.EventDecisionInvalidated) != 1 {
		t.Fatalf("invalidation events = %d, want marking to be idempotent",
			journal.count(agent.EventDecisionInvalidated))
	}
}

// A kill criterion is not an invalidated decision: the plan was fine, the
// budget ran out. It must not mark the decision stale.
func TestKillCriterionDoesNotMarkTheDecisionStale(t *testing.T) {
	c := disciplineCoordinator(t)
	journal := c.eventJournal.(*memoryJournal)

	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{
		CheckpointEvery: 1,
		KillCriteria:    []KillCriterion{{ID: "calls", Kind: agent.KillKindToolCalls, Threshold: 1}},
	}, ReplanPolicy{})
	if err := c.armDiscipline(context.Background(), "todo-1", TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}

	if decision := c.recordToolCall(context.Background(), "todo-1", false); decision.Action != CheckpointStop {
		t.Fatalf("decision = %#v, want stop", decision)
	}
	if journal.count(agent.EventKillCriterionTriggered) != 1 {
		t.Fatalf("kill events = %d, want 1", journal.count(agent.EventKillCriterionTriggered))
	}
	if journal.count(agent.EventDecisionInvalidated) != 0 {
		t.Fatal("a budget stop marked the decision stale; the plan was not invalidated")
	}
}

// A task resuming after an interrupted mutation whose outcome could not be
// classified must stop rather than run on over state nobody can account for
// (spec §38.3, DoD "side-effect crash reconciles before retry").
func TestCheckpointStopsOnUnknownSideEffectState(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "deployer", Desc: "create the bridge"}})
	todoID := items[0].ID
	c.taskTracker.TodoList().SetRecoveryState(todoID, RecoveryStateUnknown)

	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
	if err := c.armDiscipline(context.Background(), todoID, TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}
	if got := c.disciplineFor(todoID).sideEffectState; got != RecoveryStateUnknown {
		t.Fatalf("armed sideEffectState = %q, want %q", got, RecoveryStateUnknown)
	}

	decision := c.recordToolCall(context.Background(), todoID, false)
	if decision.Action != CheckpointStop || decision.Reason != ReasonReconcileUnknownState {
		t.Fatalf("decision = %#v, want a stop on %s", decision, ReasonReconcileUnknownState)
	}
	if denial := c.checkpointDenial(todoID); denial == "" {
		t.Fatal("a task stopped on unknown side-effect state still permits tool calls")
	}
}

// A classified outcome is not a reason to stop: complete, partial and
// not-started are all accountable states.
func TestCheckpointContinuesOnClassifiedSideEffectState(t *testing.T) {
	for _, state := range []string{"", RecoveryStateComplete, RecoveryStatePartial, RecoveryStateNotStarted} {
		c := dispatchCoordinator(t, dispatchConfig())
		items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "deployer", Desc: "create the bridge"}})
		todoID := items[0].ID
		if state != "" {
			c.taskTracker.TodoList().SetRecoveryState(todoID, state)
		}

		policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{CheckpointEvery: 1}, ReplanPolicy{})
		if err := c.armDiscipline(context.Background(), todoID, TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
			t.Fatal(err)
		}
		if decision := c.recordToolCall(context.Background(), todoID, false); decision.Action != CheckpointContinue {
			t.Fatalf("state %q = %#v, want continue", state, decision)
		}
	}
}

func TestTaskRecoveryStateDefaultsToEmpty(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	if got := c.taskRecoveryState("unknown-todo"); got != "" {
		t.Fatalf("taskRecoveryState = %q, want empty", got)
	}
	var nilCoordinator *Coordinator
	if got := nilCoordinator.taskRecoveryState("x"); got != "" {
		t.Fatalf("taskRecoveryState on nil = %q, want empty", got)
	}
}
