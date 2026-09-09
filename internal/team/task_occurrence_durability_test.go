package team

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

type taskOccurrenceModelRuntime struct {
	model string
	calls int
}

type taskOccurrenceModelRecordingAgent struct {
	mu      sync.Mutex
	models  []string
	entered chan struct{}
	release chan struct{}
}

func (a *taskOccurrenceModelRecordingAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *taskOccurrenceModelRecordingAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	model, _ := ctx.Value(modelKey{}).(string)
	a.mu.Lock()
	a.models = append(a.models, model)
	a.mu.Unlock()
	if a.entered != nil {
		a.entered <- struct{}{}
		<-a.release
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "task completed"},
	}}}, nil
}

func (r *taskOccurrenceModelRuntime) ResolveTaskModel(*agent.AgentDef, TaskDef) (string, error) {
	r.calls++
	return r.model, nil
}

func (*taskOccurrenceModelRuntime) ProviderFor(string) (*agent.OpenAICompatibleProvider, error) {
	return nil, nil
}

func TestRestoredTodoOccurrenceKeepsCanonicalModelWithoutJournal(t *testing.T) {
	runtime := &taskOccurrenceModelRuntime{model: "live-model"}
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir()},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		modelRuntime: runtime,
		reportStatus: func(StatusEvent) {},
	}
	checkpoint := &SessionData{Tasks: []*TodoItem{{
		ID:     "7",
		Agent:  "worker",
		Goal:   "resume the checkpointed work",
		Status: TaskInProgress,
		Model:  "checkpoint-model",
	}}}
	c.SetSessionData(checkpoint)
	c.SetEventJournal(nil)

	def := &agent.AgentDef{
		Name:        "worker",
		ExtraModels: []string{"live-extra-model"},
		Generation:  agent.GenerationParams{Model: "mutated-agent-model"},
	}
	if !c.isDurableTaskOccurrence("7") {
		t.Fatal("restored Todo was not durable without an attached journal")
	}
	if c.shouldExecuteWithExtraModels(def, "7") {
		t.Fatal("restored Todo selected live ExtraModels fanout")
	}

	got, err := c.resolveTaskExecutionModel(def, TaskDef{Model: "mutated-task-model"}, "7")
	if err != nil {
		t.Fatalf("resolveTaskExecutionModel: %v", err)
	}
	if got != "checkpoint-model" {
		t.Fatalf("resolved model = %q, want checkpoint-model", got)
	}
	if runtime.calls != 0 {
		t.Fatalf("live model runtime calls = %d, want 0", runtime.calls)
	}
}

func TestFreshTodoOccurrenceRetainsExtraModelsFanout(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), sessionData: NewSession()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "fresh task", Model: "fresh-model",
	}})[0]
	def := &agent.AgentDef{ExtraModels: []string{"extra-model"}}

	if c.isDurableTaskOccurrence(item.ID) {
		t.Fatal("fresh in-memory Todo was classified as durable")
	}
	if !c.shouldExecuteWithExtraModels(def, item.ID) {
		t.Fatal("fresh non-durable Todo did not retain ExtraModels fanout")
	}
}

func TestEphemeralTodoExecutionPreservesSuppliedTaskDef(t *testing.T) {
	c, store, calls := newRCA20ExecutionCoordinator(t)
	// Keep the Todo ephemeral even though the shared helper normally wires a
	// durable store for its other occurrence tests.
	c.SetEventJournal(nil)
	c.eventStore = nil
	_ = store
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "same execution goal", Goal: "same execution goal",
		Execution: ExecutionContract{RequiresResult: true},
	}})[0]
	c.workerAgentOverride = &submittingWorkerAgent{
		calls: calls,
		onSubmit: func() {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID:  item.ID,
				Agent:   item.Agent,
				Status:  TaskResultStatusSuccess,
				Source:  "submitted",
				Summary: "task completed",
			})
		},
	}
	task := TaskDef{
		Agent:      "worker",
		Goal:       "same execution goal",
		MaxRetries: 0,
		SideEffect: SideEffectNone,
		Recovery:   RecoveryRetry,
		Execution:  ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyCommandExit, Command: "true"},
	}

	if c.isDurableTaskOccurrence(item.ID) {
		t.Fatal("ephemeral Todo was classified as durable")
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("worker calls = %d, want 1", *calls)
	}
	current := c.todoItemByID(item.ID)
	if current == nil || current.VerifyResult == nil || current.VerifyResult.Command != "true" {
		t.Fatalf("ephemeral execution lost supplied verification contract: %#v", current)
	}
	if current.TypedResult == nil || current.TypedResult.Source != "submitted" || current.TypedResult.Status != TaskResultStatusSuccess {
		t.Fatalf("typed result = %#v, want submitted success", current.TypedResult)
	}
	if current.VerifyResult.ExitCode != 0 {
		t.Fatalf("verification exit code = %d, want successful verification", current.VerifyResult.ExitCode)
	}
	if current.ExecutionReceipt == nil || current.ExecutionReceipt.RepairProvenance != nil {
		t.Fatalf("execution receipt repair provenance = %#v, want none", current.ExecutionReceipt)
	}
}

func TestExecuteTasksFreezesConfiguredModelTopologyBeforeExecution(t *testing.T) {
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "topology-test"},
		Agents: map[string]*agent.AgentDef{
			"worker": {
				Name:        "worker",
				Role:        "worker",
				ExtraModels: []string{"extra-a", "extra-b"},
				Generation:  agent.GenerationParams{Model: "primary"},
			},
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(NewSession())
	c.SetStepConfirmFn(func(context.Context, []TaskDef) (bool, error) { return false, nil })

	_, err = c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "freeze fanout"}})
	if err == nil {
		t.Fatal("expected step confirmation to stop execution after task creation")
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("created tasks = %d, want 1", len(items))
	}
	if got, want := items[0].ModelTopology, []string{"primary", "extra-a", "extra-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("created model topology = %#v, want %#v", got, want)
	}
}

func TestExecuteTasksExecutesOverlappingRecordedModelTopologyOncePerConfiguredModel(t *testing.T) {
	worker := &taskOccurrenceModelRecordingAgent{entered: make(chan struct{}, 3), release: make(chan struct{})}
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "topology-execution-test", MaxRetries: 0},
		Agents: map[string]*agent.AgentDef{
			"worker": {
				Name:        "worker",
				Role:        "worker",
				ExtraModels: []string{"extra-a", "extra-b"},
				Generation:  agent.GenerationParams{Model: "primary"},
			},
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(session.Workspace, "run-topology-execution", "session-topology-execution")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.executionRunID = "run-topology-execution"
	c.contextRepo = nil
	c.workerAgentOverride = worker

	runDone := make(chan error, 1)
	go func() {
		_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "execute fanout"}})
		runDone <- err
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-worker.entered:
		case <-time.After(5 * time.Second):
			close(worker.release)
			t.Fatalf("only %d of 3 model leaves overlapped", i)
		}
	}
	close(worker.release)
	if err := <-runDone; err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}

	worker.mu.Lock()
	got := append([]string(nil), worker.models...)
	worker.mu.Unlock()
	want := []string{"primary", "extra-a", "extra-b"}
	counts := make(map[string]int, len(got))
	for _, model := range got {
		counts[model]++
	}
	if len(got) != len(want) || counts[want[0]] != 1 || counts[want[1]] != 1 || counts[want[2]] != 1 {
		item := c.taskTracker.TodoList().Items()[0]
		t.Fatalf("executed models = %#v, want exactly one leaf for each %v; todo status=%s detail=%q output=%q", got, want, item.Status, item.Detail, item.Output)
	}
}

func TestDecisionEnabledFanoutSharesParentIndexForConcurrentLeaves(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-decision-fanout", "session-decision-fanout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := SaveSessionTree(workspace, NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	parent := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Decision: DecisionConfig{
			DefaultProfile: "standard", Profiles: map[string]DecisionPolicy{"standard": {IndependentJudgments: 1}},
		}}},
		taskTracker: NewTaskTracker(), eventStore: store, eventJournal: eventStoreJournal{store: store},
		executionRunID: "run-decision-fanout",
	}
	control, err := parent.pinDecisionControlPlane()
	if err != nil {
		t.Fatalf("pinDecisionControlPlane: %v", err)
	}
	if resolution, err := ResolveDecisionProfile(parent.session.Config.Decision, "", TaskDef{}); err != nil || !resolution.Enabled() {
		t.Fatalf("decision profile resolution = %#v, err=%v; want enabled", resolution, err)
	}
	leaves := []*Coordinator{
		cloneCoordinator(parent, &TeamSession{Workspace: t.TempDir()}),
		cloneCoordinator(parent, &TeamSession{Workspace: t.TempDir()}),
		cloneCoordinator(parent, &TeamSession{Workspace: t.TempDir()}),
	}
	start := make(chan struct{})
	errs := make(chan error, len(leaves))
	var wg sync.WaitGroup
	wg.Add(len(leaves))
	for i, leaf := range leaves {
		go func(i int, leaf *Coordinator) {
			defer wg.Done()
			<-start
			index, indexErr := leaf.decisionIndex()
			if indexErr != nil {
				errs <- indexErr
				return
			}
			if index != control.index {
				errs <- fmt.Errorf("leaf %d resolved a non-parent decision index", i)
				return
			}
			errs <- index.Append(indexEntry(fmt.Sprintf("fanout-decision-%d", i), time.Unix(int64(i), 0).UTC()))
		}(i, leaf)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := control.index.listFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(leaves) {
		t.Fatalf("parent decision index entries = %d, want %d", len(entries), len(leaves))
	}
	if journal, ok := control.journal.(*branchScopedDecisionJournal); !ok || journal.raw != (eventStoreJournal{store: store}) {
		t.Fatalf("parent decision journal = %T, want branch-scoped canonical parent journal", control.journal)
	}
}

func TestTaskOccurrenceModelTopologySurvivesEventReplayAndShadow(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{
		Agent:         "worker",
		Desc:          "durable fanout",
		Goal:          "durable fanout",
		Model:         "primary",
		ModelTopology: []string{"primary", "extra-a"},
	}, "1")
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
	if err != nil {
		t.Fatalf("checked replay rejected matching target dual-write: %v", err)
	}
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].ModelTopology, item.ModelTopology) || replayed[0].ExecutionTarget != item.ExecutionTarget || !reflect.DeepEqual(replayed[0].ExecutionTopology, item.ExecutionTopology) || !reflect.DeepEqual(replayed[0].BackendBinding, item.BackendBinding) {
		t.Fatalf("replayed identity = %#v, want model/target topology parity with %#v", replayed, item)
	}
	if got := taskDefFromTodoItem(replayed[0]).ModelTopology; !reflect.DeepEqual(got, item.ModelTopology) {
		t.Fatalf("task definition topology = %#v, want %#v", got, item.ModelTopology)
	}
	if err := CompareCanonicalProjection(&SessionData{Tasks: []*TodoItem{item}}, []RunEvent{{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}}); err != nil {
		t.Fatalf("checkpoint/event shadow rejected topology parity: %v", err)
	}

	mutatedPayload := map[string]any{"id": item.ID, "status": string(TaskInProgress), "model": "primary", "model_topology": []string{"changed"}}
	mutated, err := json.Marshal(mutatedPayload)
	if err != nil {
		t.Fatal(err)
	}
	replayed = ReduceToTodoList([]RunEvent{
		{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload},
		{Type: string(EventTaskStarted), TaskID: item.ID, Payload: mutated},
	})
	if got, want := replayed[0].ModelTopology, item.ModelTopology; !reflect.DeepEqual(got, want) {
		t.Fatalf("later lifecycle event changed immutable topology to %#v, want %#v", got, want)
	}
}

func TestTaskOccurrenceRetryKeepsRecordedModelTopology(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-topology", "session-topology")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-topology",
		reportStatus:   func(StatusEvent) {},
		eventStore:     store,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "retry fanout", Model: "primary", ModelTopology: []string{"primary", "extra-a"}},
	})[0]
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry recorded topology"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.taskTracker.TodoList().Items()[0].ModelTopology, item.ModelTopology; !reflect.DeepEqual(got, want) {
		t.Fatalf("retry topology = %#v, want %#v", got, want)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatalf("checked replay rejected retry identity: %v", err)
	}
	if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].ModelTopology, item.ModelTopology) || replayed[0].ExecutionTarget != item.ExecutionTarget || !reflect.DeepEqual(replayed[0].ExecutionTopology, item.ExecutionTopology) {
		t.Fatalf("replayed retry identity = %#v, want topology parity with %#v", replayed, item)
	}
}

func TestTaskOccurrenceDigestIncludesFinalizedEdgesAndWorkset(t *testing.T) {
	receipt := &WorksetExpansionReceipt{
		WorksetID: "workset-1", ParentTaskID: "parent", SourceArtifactID: "source",
		SourceSHA256: "source-digest", ItemCount: 1, ItemKeysSHA256: "keys",
		Children: map[string]string{"item": "7"},
	}
	binding := &WorksetBinding{
		WorksetID: "workset-1", ParentTaskID: "parent", ItemKey: "item",
		SourceArtifactID: "source", SourceSHA256: "source-digest",
	}
	item := todoItemFromSpec(TodoSpec{
		PlanTaskID: "plan-child", Agent: "worker", Desc: "finalized goal", Goal: "finalized goal",
		Model: "model-a", ModelTopology: []string{"model-a", "model-b"}, ParentID: "parent",
		DependsOn: []string{"2"}, OnFailure: "3", Verify: "test -f result", VerifyMode: "success",
		WorksetBinding: binding, WorksetReceipt: receipt, Execution: ExecutionContract{RequiresResult: true},
		Recovery: RecoveryRetry, SideEffect: SideEffectWorkspaceWrite,
		DecisionProfile: "standard", DecisionOptions: []DecisionOption{{ID: "ship", Title: "Ship"}},
	}, "7")
	base, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := decisionTaskInputDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*TaskOccurrenceProjection){
		func(p *TaskOccurrenceProjection) { p.DependsOn = []string{"4"} },
		func(p *TaskOccurrenceProjection) { p.OnFailure = "5" },
		func(p *TaskOccurrenceProjection) { p.ParentID = "other-parent" },
		func(p *TaskOccurrenceProjection) { p.ModelTopology = []string{"model-a"} },
		func(p *TaskOccurrenceProjection) { p.Verify = "test -f other-result" },
		func(p *TaskOccurrenceProjection) { p.Recovery = RecoveryNever },
		func(p *TaskOccurrenceProjection) { p.WorksetBinding.ItemKey = "other-item" },
		func(p *TaskOccurrenceProjection) { p.WorksetReceipt.Children["item"] = "8" },
	}
	for n, mutate := range mutations {
		candidate := base
		candidate.DependsOn = append([]string(nil), base.DependsOn...)
		candidate.ModelTopology = append([]string(nil), base.ModelTopology...)
		candidate.WorksetBinding = cloneWorksetBinding(base.WorksetBinding)
		candidate.WorksetReceipt = cloneWorksetReceipt(base.WorksetReceipt)
		mutate(&candidate)
		changed, err := decisionTaskInputDigest(candidate)
		if err != nil {
			t.Fatalf("mutation %d digest: %v", n, err)
		}
		if changed == digest {
			t.Fatalf("mutation %d was omitted from occurrence digest", n)
		}
	}
}

func TestDurableTaskCreationRejectsImmutableOccurrenceTampering(t *testing.T) {
	hypothesis := &RecoveryHypothesis{
		ObservedFailure: "the first attempt failed", HypothesizedCause: "invalid input",
		ProposedChange: "validate input", DifferenceFromPrior: "new validation", ExpectedChange: "exit 0",
		Strategy: RecoveryStrategyToolChange,
	}
	cases := map[string]func(*TodoSpec){
		"plan first":  func(spec *TodoSpec) { spec.PlanFirst = true },
		"max retries": func(spec *TodoSpec) { spec.MaxRetries = 4 },
		"recovery hypothesis": func(spec *TodoSpec) {
			spec.RecoveryHypothesis = cloneRecoveryHypothesis(hypothesis)
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-creation-tamper", "session-creation-tamper")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()

			c := &Coordinator{
				session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{WorkspaceDir: workspace}},
				taskTracker:    NewTaskTracker(),
				eventStore:     store,
				eventJournal:   eventStoreJournal{store: store},
				executionRunID: "run-creation-tamper",
			}
			ids := c.taskTracker.TodoList().ReserveIDs(1)
			base := TaskDef{ID: "plan-task", Agent: "worker", Goal: "execute the approved task"}
			if _, err := c.admitTaskOccurrence(context.Background(), base, ids[0], 1); err != nil {
				t.Fatalf("admitTaskOccurrence: %v", err)
			}
			spec := todoSpecForTestTask(base)
			mutate(&spec)
			if _, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids); err == nil {
				t.Fatal("tampered task creation unexpectedly succeeded")
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Type != string(EventDecisionAdmitted) {
				t.Fatalf("tampered creation appended task_created or other event: %#v", events)
			}
			if got := len(c.taskTracker.TodoList().Items()); got != 0 {
				t.Fatalf("tampered creation left %d visible tasks", got)
			}
		})
	}
}

func TestTaskTransitionAuditProjectionBindsImmutableExecutionAndRecovery(t *testing.T) {
	base := &TodoItem{
		ID: "task-audit", Agent: "worker", Desc: "execute", Status: TaskPending,
		Action: &Action{Capability: "deploy", Type: "apply", Payload: "release"},
		RecoveryHypothesis: &RecoveryHypothesis{
			ObservedFailure: "the first attempt failed", HypothesizedCause: "invalid input",
			ProposedChange: "validate input", DifferenceFromPrior: "new validation", ExpectedChange: "exit 0",
			Strategy: RecoveryStrategyToolChange,
		},
	}
	want := taskTransitionEventKey(base)
	cases := map[string]func(*TodoItem){
		"action":              func(item *TodoItem) { item.Action.Payload = "different-release" },
		"recovery hypothesis": func(item *TodoItem) { item.RecoveryHypothesis.ProposedChange = "different-change" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := *base
			candidate.Action = cloneActionPtr(base.Action)
			candidate.RecoveryHypothesis = cloneRecoveryHypothesis(base.RecoveryHypothesis)
			mutate(&candidate)
			if got := taskTransitionEventKey(&candidate); got == want {
				t.Fatalf("audit projection omitted changed %s", name)
			}
		})
	}
}

func TestDurableTaskCreationRejectsMarkerlessAndMismatchedAdmission(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-admission", "session-admission")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    NewTaskTracker(),
		eventStore:     store,
		eventJournal:   eventStoreJournal{store: store},
		executionRunID: "run-admission",
	}

	ids := c.taskTracker.TodoList().ReserveIDs(1)
	_, err = c.CommitTaskCreationResolved(context.Background(), []TodoSpec{{
		Agent: "worker", Desc: "markerless", Goal: "markerless", Model: "model",
		ModelTopology: []string{"model"},
	}}, ids)
	if err == nil {
		t.Fatal("markerless durable task creation unexpectedly succeeded")
	}
	events, readErr := store.ReadEvents()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(events) != 0 || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("markerless creation left durable/projection state: events=%d tasks=%d", len(events), len(c.taskTracker.TodoList().Items()))
	}

	task := TaskDef{ID: ids[0], Agent: "worker", Goal: "original", Model: "model", ModelTopology: []string{"model"}}
	if _, err := c.admitTaskOccurrence(context.Background(), task, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	_, err = c.CommitTaskCreationResolved(context.Background(), []TodoSpec{{
		Agent: "worker", Desc: "changed", Goal: "changed", Model: "model",
		ModelTopology: []string{"model"},
	}}, ids)
	if err == nil {
		t.Fatal("mismatched durable task creation unexpectedly succeeded")
	}
	events, readErr = store.ReadEvents()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(events) != 1 || events[0].Type != string(EventDecisionAdmitted) {
		t.Fatalf("mismatched creation appended task_created or other event: %#v", events)
	}
}

func TestExtraModelLeafUsesPinnedParentDecisionControlPlane(t *testing.T) {
	parentWorkspace := t.TempDir()
	store, err := NewEventStore(parentWorkspace, "run-parent-control", "session-parent-control")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := SaveSessionTree(parentWorkspace, NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	parent := &Coordinator{
		session:        &TeamSession{Workspace: parentWorkspace},
		taskTracker:    NewTaskTracker(),
		eventStore:     store,
		eventJournal:   eventStoreJournal{store: store},
		executionRunID: "run-parent-control",
	}
	control, err := parent.pinDecisionControlPlane()
	if err != nil {
		t.Fatalf("pinDecisionControlPlane: %v", err)
	}
	leaf := cloneCoordinator(parent, &TeamSession{Workspace: t.TempDir()})
	if got := leaf.decisionArtifactStore(); got != control.artifactStore {
		t.Fatalf("leaf artifact store = %T/%p, want parent %T/%p", got, got, control.artifactStore, control.artifactStore)
	}
	index, err := leaf.decisionIndex()
	if err != nil {
		t.Fatalf("leaf decisionIndex: %v", err)
	}
	if index != control.index {
		t.Fatalf("leaf decision index = %p, want parent %p", index, control.index)
	}
	journal, err := leaf.decisionJournalFor()
	if err != nil {
		t.Fatalf("leaf decisionJournalFor: %v", err)
	}
	if journal != control.journal {
		t.Fatalf("leaf journal = %T/%p, want pinned parent %T/%p", journal, journal, control.journal, control.journal)
	}
	if _, ok := journal.(*branchScopedDecisionJournal); !ok {
		t.Fatalf("leaf journal = %T, want branch-scoped parent journal", journal)
	}
}
