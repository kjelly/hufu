package team

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func newRCA20ExecutionCoordinator(t *testing.T) (*Coordinator, *EventStore, *int) {
	t.Helper()
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:                 "rca20-execution",
			AllowFreeTextResults: true,
			MaxRetries:           0,
		},
		Agents: map[string]*agent.AgentDef{
			"worker": {
				Name:       "worker",
				Role:       "worker",
				SideEffect: string(SideEffectNone),
				Generation: agent.GenerationParams{Model: "test-model"},
			},
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(session.Workspace, "run-rca20", "session-rca20")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.executionRunID = "run-rca20"
	c.SetSessionData(NewSession())
	c.phaseWorkflow = nil
	calls := 0
	c.workerAgentOverride = &submittingWorkerAgent{calls: &calls}
	t.Cleanup(func() { _ = store.Close() })
	return c, store, &calls
}

func rca20MutationBase() TaskDef {
	return TaskDef{
		ID:            "occurrence",
		Agent:         "worker",
		Goal:          "execute immutable occurrence",
		Model:         "test-model",
		SideEffect:    SideEffectNone,
		Recovery:      RecoveryRetry,
		ReconcileTool: "probe-state",
		VerifySpec:    &VerificationSpec{Type: VerifyCommandExit, Command: "true"},
		MaxRetries:    1,
	}
}

func TestRCA20PostCreationMutationFailsClosedBeforeWorkerOrAction(t *testing.T) {
	step := ExecutionStep{ID: "read", Tool: "view", Effect: ExecutionEffectRead}
	action := &Action{Capability: "structured-actions", Type: "apply", Payload: "{}"}
	cases := []struct {
		name   string
		base   func() TaskDef
		mutate func(*TaskDef)
	}{
		{
			name:   "action added",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.Action = cloneActionPtr(action) },
		},
		{
			name: "action cleared",
			base: func() TaskDef {
				task := rca20MutationBase()
				task.Action = cloneActionPtr(action)
				return task
			},
			mutate: func(task *TaskDef) { task.Action = nil },
		},
		{
			name: "execution steps changed",
			base: func() TaskDef {
				task := rca20MutationBase()
				task.Execution.Steps = []ExecutionStep{step}
				return task
			},
			mutate: func(task *TaskDef) { task.Execution.Steps[0].Tool = "bash" },
		},
		{
			name: "execution steps cleared",
			base: func() TaskDef {
				task := rca20MutationBase()
				task.Execution.Steps = []ExecutionStep{step}
				return task
			},
			mutate: func(task *TaskDef) { task.Execution.Steps = nil },
		},
		{
			name:   "plan first changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.PlanFirst = true },
		},
		{
			name:   "verify spec changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.VerifySpec = &VerificationSpec{Type: VerifyFileExists, Path: "forged"} },
		},
		{
			name:   "max retries changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.MaxRetries = 9 },
		},
		{
			name:   "side effect changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.SideEffect = SideEffectExternalWrite },
		},
		{
			name:   "recovery changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.Recovery = RecoveryManual },
		},
		{
			name:   "reconcile tool changed",
			base:   rca20MutationBase,
			mutate: func(task *TaskDef) { task.ReconcileTool = "forged-probe" },
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			c, _, calls := newRCA20ExecutionCoordinator(t)
			base := testCase.base()
			c.SetStepConfirmFn(func(_ context.Context, tasks []TaskDef) (bool, error) {
				if len(tasks) != 1 {
					t.Fatalf("step confirmation tasks = %d, want 1", len(tasks))
				}
				testCase.mutate(&tasks[0])
				return true, nil
			})
			_, err := c.ExecuteTasks(context.Background(), []TaskDef{base})
			if err == nil || !strings.Contains(err.Error(), "scheduler contract differs") {
				t.Fatalf("ExecuteTasks error = %v, want fail-closed scheduler contract error", err)
			}
			if *calls != 0 {
				t.Fatalf("worker/provider calls = %d, want 0", *calls)
			}
			items := c.taskTracker.TodoList().Items()
			if len(items) != 1 || items[0].Status != TaskPending {
				t.Fatalf("durable occurrence after rejected mutation = %#v, want one pending occurrence", items)
			}
		})
	}
}

func TestRCA20PlanIDMutationCannotChangeAdmittedExecution(t *testing.T) {
	c, _, calls := newRCA20ExecutionCoordinator(t)
	base := rca20MutationBase()
	c.SetStepConfirmFn(func(_ context.Context, tasks []TaskDef) (bool, error) {
		if len(tasks) != 1 {
			t.Fatalf("step confirmation tasks = %d, want 1", len(tasks))
		}
		tasks[0].PlanID = "forged-plan"
		return true, nil
	})
	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{base}); err != nil {
		t.Fatalf("ExecuteTasks with lifecycle-only PlanID mutation: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("worker/provider calls = %d, want exactly 1", *calls)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].PlanID != "" || items[0].Status != TaskDone {
		t.Fatalf("durable occurrence after PlanID mutation = %#v, want completed occurrence with no forged PlanID", items)
	}
}

func rca20DigestProjection() TaskOccurrenceProjection {
	return TaskOccurrenceProjection{
		ID: "todo-1", PlanTaskID: "plan-1", PlanFirst: true, PlanID: "lifecycle-plan", Phase: PhaseExecute,
		Action: &Action{Capability: "capability", Type: "apply", Payload: "{}"}, ContractID: "contract", ContractHash: "hash", ContractRevision: 3,
		Agent: "worker", Desc: "immutable description", Goal: "immutable goal", Constraints: "constraint", Model: "model", ModelTopology: []string{"model", "extra"},
		Sidecar: true, Summarize: true, OutputMode: "verbatim", ContextFiles: []string{"context.md"}, Requires: []string{"capability"}, Source: TaskSourceCoordinator, ParentID: "parent",
		DependsOn: []string{"todo-0"}, OnFailure: "todo-repair", Verify: "test -f output", VerifyMode: "success",
		VerifySpec:     &VerificationSpec{Type: VerifyJSONAssert, Path: "output.json", Assertions: []JSONAssertion{{Path: "ok", Equals: true}}},
		WorksetBinding: &WorksetBinding{WorksetID: "workset", ParentTaskID: "parent", ItemKey: "item", Bindings: map[string]string{"name": "value"}},
		WorksetReceipt: &WorksetExpansionReceipt{WorksetID: "workset", RunID: "run", ParentTaskID: "parent", ItemCount: 1, Children: map[string]string{"item": "todo-1"}},
		MaxRetries:     2, SideEffect: SideEffectWorkspaceWrite, Escalate: true, AdversarialVerify: 1, Recovery: RecoveryReconcile, ReconcileTool: "probe",
		Kind: TaskKindRepair, Advances: []string{"artifact"}, ExpectedStateChange: "artifact changes", RecoveryHypothesis: &RecoveryHypothesis{CriterionID: "artifact", ObservedFailure: "missing", HypothesizedCause: "omitted", ProposedChange: "write", ExpectedChange: "exists"},
		Execution: ExecutionContract{Kind: ExecutionKindProcess, RequiresResult: true, RequiresVerification: true, Steps: []ExecutionStep{{ID: "step", Tool: "view", Effect: ExecutionEffectRead}}},
		Optional:  true, ResourceClaims: []string{"workspace"}, Resources: []ResourceClaim{{Resource: "workspace", Mode: ResourceWrite}},
		DecisionProfile: "standard", DecisionOptions: []DecisionOption{{ID: "go", Kind: OptionExecute}}, DecisionAssumptions: []DecisionAssumption{{ID: "safe", Statement: "safe", Critical: true}},
		DecisionFacts: map[string]any{"count": 1}, DecisionArtifacts: []ArtifactRef{{ID: "artifact", Path: "output.json"}}, DecisionBaseRates: []BaseRateEvidence{{ReferenceClass: "class", Metric: "success", SampleSize: 1}},
		DecisionProvenance: []EvidenceProvenance{{SourceID: "source", SourceType: EvidenceSourceDeclared, RetrievedAt: time.Unix(1, 0).UTC()}},
	}
}

func TestRCA20DigestBindsEveryImmutableProjectionFieldExceptPlanID(t *testing.T) {
	base := rca20DigestProjection()
	baseDigest, err := decisionTaskInputDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*TaskOccurrenceProjection)
	}{
		{"ID", func(p *TaskOccurrenceProjection) { p.ID = "todo-2" }},
		{"PlanTaskID", func(p *TaskOccurrenceProjection) { p.PlanTaskID = "plan-2" }},
		{"PlanFirst", func(p *TaskOccurrenceProjection) { p.PlanFirst = false }},
		{"Phase", func(p *TaskOccurrenceProjection) { p.Phase = PhaseVerify }},
		{"Action", func(p *TaskOccurrenceProjection) { p.Action = &Action{Capability: "other", Type: "inspect"} }},
		{"ContractID", func(p *TaskOccurrenceProjection) { p.ContractID = "other-contract" }},
		{"ContractHash", func(p *TaskOccurrenceProjection) { p.ContractHash = "other-hash" }},
		{"ContractRevision", func(p *TaskOccurrenceProjection) { p.ContractRevision++ }},
		{"Agent", func(p *TaskOccurrenceProjection) { p.Agent = "other-agent" }},
		{"Desc", func(p *TaskOccurrenceProjection) { p.Desc = "changed description" }},
		{"Goal", func(p *TaskOccurrenceProjection) { p.Goal = "changed goal" }},
		{"Constraints", func(p *TaskOccurrenceProjection) { p.Constraints = "changed constraint" }},
		{"Model", func(p *TaskOccurrenceProjection) { p.Model = "other-model" }},
		{"ModelTopology", func(p *TaskOccurrenceProjection) { p.ModelTopology = []string{"other-model"} }},
		{"Sidecar", func(p *TaskOccurrenceProjection) { p.Sidecar = false }},
		{"Summarize", func(p *TaskOccurrenceProjection) { p.Summarize = false }},
		{"OutputMode", func(p *TaskOccurrenceProjection) { p.OutputMode = "summary" }},
		{"ContextFiles", func(p *TaskOccurrenceProjection) { p.ContextFiles = []string{"other.md"} }},
		{"Requires", func(p *TaskOccurrenceProjection) { p.Requires = []string{"other-capability"} }},
		{"Source", func(p *TaskOccurrenceProjection) { p.Source = TaskSourceAgent }},
		{"ParentID", func(p *TaskOccurrenceProjection) { p.ParentID = "other-parent" }},
		{"DependsOn", func(p *TaskOccurrenceProjection) { p.DependsOn = []string{"other"} }},
		{"OnFailure", func(p *TaskOccurrenceProjection) { p.OnFailure = "other-failure" }},
		{"Verify", func(p *TaskOccurrenceProjection) { p.Verify = "false" }},
		{"VerifyMode", func(p *TaskOccurrenceProjection) { p.VerifyMode = "observation" }},
		{"VerifySpec", func(p *TaskOccurrenceProjection) {
			p.VerifySpec = &VerificationSpec{Type: VerifyFileAbsent, Path: "other"}
		}},
		{"WorksetBinding", func(p *TaskOccurrenceProjection) { p.WorksetBinding = &WorksetBinding{WorksetID: "other"} }},
		{"WorksetReceipt", func(p *TaskOccurrenceProjection) { p.WorksetReceipt = &WorksetExpansionReceipt{WorksetID: "other"} }},
		{"MaxRetries", func(p *TaskOccurrenceProjection) { p.MaxRetries++ }},
		{"SideEffect", func(p *TaskOccurrenceProjection) { p.SideEffect = SideEffectExternalWrite }},
		{"Escalate", func(p *TaskOccurrenceProjection) { p.Escalate = false }},
		{"AdversarialVerify", func(p *TaskOccurrenceProjection) { p.AdversarialVerify++ }},
		{"Recovery", func(p *TaskOccurrenceProjection) { p.Recovery = RecoveryManual }},
		{"ReconcileTool", func(p *TaskOccurrenceProjection) { p.ReconcileTool = "other-probe" }},
		{"Kind", func(p *TaskOccurrenceProjection) { p.Kind = TaskKindDiagnostic }},
		{"Advances", func(p *TaskOccurrenceProjection) { p.Advances = []string{"other-artifact"} }},
		{"ExpectedStateChange", func(p *TaskOccurrenceProjection) { p.ExpectedStateChange = "other change" }},
		{"RecoveryHypothesis", func(p *TaskOccurrenceProjection) {
			p.RecoveryHypothesis = &RecoveryHypothesis{ObservedFailure: "other", HypothesizedCause: "other", ProposedChange: "other", ExpectedChange: "other"}
		}},
		{"Execution", func(p *TaskOccurrenceProjection) { p.Execution = ExecutionContract{Kind: ExecutionKindInteractive} }},
		{"Optional", func(p *TaskOccurrenceProjection) { p.Optional = false }},
		{"ResourceClaims", func(p *TaskOccurrenceProjection) { p.ResourceClaims = []string{"other-resource"} }},
		{"Resources", func(p *TaskOccurrenceProjection) {
			p.Resources = []ResourceClaim{{Resource: "other", Mode: ResourceExclusive}}
		}},
		{"DecisionProfile", func(p *TaskOccurrenceProjection) { p.DecisionProfile = "other" }},
		{"DecisionOptions", func(p *TaskOccurrenceProjection) {
			p.DecisionOptions = []DecisionOption{{ID: "other", Kind: OptionAbandon}}
		}},
		{"DecisionAssumptions", func(p *TaskOccurrenceProjection) {
			p.DecisionAssumptions = []DecisionAssumption{{ID: "other", Statement: "other"}}
		}},
		{"DecisionFacts", func(p *TaskOccurrenceProjection) { p.DecisionFacts = map[string]any{"other": true} }},
		{"DecisionArtifacts", func(p *TaskOccurrenceProjection) { p.DecisionArtifacts = []ArtifactRef{{ID: "other", Path: "other"}} }},
		{"DecisionBaseRates", func(p *TaskOccurrenceProjection) { p.DecisionBaseRates = []BaseRateEvidence{{ReferenceClass: "other"}} }},
		{"DecisionProvenance", func(p *TaskOccurrenceProjection) { p.DecisionProvenance = []EvidenceProvenance{{SourceID: "other"}} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := base
			testCase.mutate(&candidate)
			got, err := decisionTaskInputDigest(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if got == baseDigest {
				t.Fatalf("digest did not change after %s mutation: %q", testCase.name, got)
			}
		})
	}
	planIDOnly := base
	planIDOnly.PlanID = "different-lifecycle-plan"
	planIDDigest, err := decisionTaskInputDigest(planIDOnly)
	if err != nil {
		t.Fatal(err)
	}
	if planIDDigest != baseDigest {
		t.Fatalf("PlanID-only digest changed: got %q, want %q", planIDDigest, baseDigest)
	}
}

func TestRCA20RetryReplayPreservesDurableExecutionContract(t *testing.T) {
	c, store, _ := newRCA20ExecutionCoordinator(t)
	task := rca20MutationBase()
	task.Goal = "retry and replay immutable execution"
	task.Sidecar = true
	task.Summarize = true
	task.OutputMode = "verbatim"
	task.ContextFiles = []string{"context.md"}
	task.Requires = []string{"capability"}
	task.Escalate = true
	task.AdversarialVerify = 1
	task.Execution = ExecutionContract{Kind: ExecutionKindProcess, RequiresResult: true, Steps: []ExecutionStep{{ID: "inspect", Tool: "view", Effect: ExecutionEffectRead}}}
	task.Action = nil
	_, item := createAdmittedTestTask(t, c, task)
	before := taskOccurrenceProjectionForTest(t, item)
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry contract"); err != nil {
		t.Fatalf("CommitTaskResetForRetry: %v", err)
	}
	current := c.todoItemByID(item.ID)
	after := taskOccurrenceProjectionForTest(t, current)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("retry changed immutable projection:\nbefore=%#v\nafter=%#v", before, after)
	}
	if current.Retries != 1 || current.Status != TaskPending {
		t.Fatalf("retry lifecycle projection = retries %d status %s, want 1/pending", current.Retries, current.Status)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 {
		t.Fatalf("replayed tasks = %d, want 1", len(replayed))
	}
	replayedProjection := taskOccurrenceProjectionForTest(t, replayed[0])
	if !reflect.DeepEqual(after, replayedProjection) {
		t.Fatalf("replay changed immutable projection:\nlive=%#v\nreplayed=%#v", after, replayedProjection)
	}
	if replayed[0].Retries != 1 || replayed[0].Status != TaskPending {
		t.Fatalf("replayed retry lifecycle = retries %d status %s, want 1/pending", replayed[0].Retries, replayed[0].Status)
	}
	if _, found, err := c.validateTaskOccurrenceAdmission(context.Background(), taskDefFromTodoItem(current), current.ID, 2); err != nil || !found {
		t.Fatalf("retry admission validation = found %t err %v, want durable attempt 2 admission", found, err)
	}
}

func TestRCA20PlanApprovalLifecyclePreservesAdmissionDigest(t *testing.T) {
	c, store, _ := newRCA20ExecutionCoordinator(t)
	task := rca20MutationBase()
	task.PlanFirst = true
	_, item := createAdmittedTestTask(t, c, task)
	before, err := decisionTaskInputDigest(taskOccurrenceProjectionForTest(t, item))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskPlanLifecycle(context.Background(), item.ID, true, item.ID); err != nil {
		t.Fatalf("commitTaskPlanLifecycle: %v", err)
	}
	current := c.todoItemByID(item.ID)
	after, err := decisionTaskInputDigest(taskOccurrenceProjectionForTest(t, current))
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("plan approval changed admission digest: before=%q after=%q", before, after)
	}
	if current.PlanID != item.ID || !current.PlanFirst {
		t.Fatalf("plan lifecycle projection = PlanFirst %t PlanID %q, want true/%q", current.PlanFirst, current.PlanID, item.ID)
	}
	if _, found, err := c.validateTaskOccurrenceAdmission(context.Background(), taskDefFromTodoItem(current), current.ID, 1); err != nil || !found {
		t.Fatalf("approved plan admission validation = found %t err %v, want durable admission", found, err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 || replayed[0].PlanID != item.ID || !replayed[0].PlanFirst {
		t.Fatalf("replayed plan lifecycle = %#v, want approved plan marker", replayed)
	}
}

func taskOccurrenceProjectionForTest(t *testing.T, item *TodoItem) TaskOccurrenceProjection {
	t.Helper()
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(fmt.Errorf("newTaskOccurrenceProjection: %w", err))
	}
	return projection
}
