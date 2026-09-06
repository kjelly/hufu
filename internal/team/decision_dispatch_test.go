package team

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func dispatchCoordinator(t *testing.T, cfg DecisionConfig) *Coordinator {
	t.Helper()
	workspace := t.TempDir()
	return &Coordinator{
		sessionTime:  time.Now(),
		eventJournal: &memoryJournal{},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Decision: cfg, WorkspaceDir: workspace},
		},
	}
}

func dispatchConfig() DecisionConfig {
	return DecisionConfig{
		DefaultProfile: agent.DecisionProfileOff,
		RequestContract: agent.RequestContractConfig{
			Enabled: true, Objective: "complete the requested migration",
			SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "success", Statement: "the migration is complete"}},
		},
		Profiles: map[string]DecisionPolicy{
			"standard": {
				IndependentJudgments: 2,
				ContextIsolation:     agent.DecisionIsolationStrict,
				ScoreScale:           agent.DecisionScoreScale,
				Criteria:             []DecisionCriterion{{ID: "cost", Weight: 1}},
			},
		},
	}
}

func decisionTask() TaskDef {
	return TaskDef{
		ID:          "t1",
		Agent:       "worker",
		Goal:        "migrate the ingest pipeline",
		Constraints: "preserve the public API",
		DecisionOptions: []DecisionOption{
			{ID: "migrate", Kind: OptionExecute, Title: "Migrate now"},
			{ID: "wait", Kind: OptionDefer, Title: "Wait a quarter"},
		},
	}
}

func decisionAdmissionTask() TaskDef {
	task := decisionTask()
	task.DecisionProfile = "standard"
	task.Recovery = RecoveryRetry
	task.DecisionAssumptions = []DecisionAssumption{{
		ID: "assumption-1", Statement: "the target can accept the migration", Critical: true,
		EvidenceRefs: []ArtifactRef{{ID: "evidence-1", Path: "evidence/target.json"}},
	}}
	task.DecisionFacts = map[string]any{"target": map[string]any{"region": "us-east-1"}}
	task.DecisionArtifacts = []ArtifactRef{{ID: "evidence-1", Path: "evidence/target.json"}}
	task.DecisionBaseRates = []BaseRateEvidence{{
		ReferenceClass: "similar migrations", Metric: "success", SampleSize: 10,
		Source: ArtifactRef{ID: "base-rate-1", Path: "evidence/base-rate.json", SHA256: strings.Repeat("a", 64)}, Limitations: []string{"small sample"},
	}}
	task.DecisionProvenance = []EvidenceProvenance{{
		SourceID: "evidence-1", SourceType: EvidenceSourceArtifact, ParentSourceIDs: []string{"root-1"},
		DeclaredParentSourceIDs: []string{"declared-root-1"}, IndependenceGroup: "independent-a",
	}}
	return task
}

func decisionAdmissionTodoSpec(task TaskDef) TodoSpec {
	return todoSpecForTestTask(task)
}

func assertDecisionAdmissionIntent(t *testing.T, want, got TaskDef) {
	t.Helper()
	if want.Goal != got.Goal || want.Constraints != got.Constraints ||
		want.DecisionProfile != got.DecisionProfile ||
		!reflect.DeepEqual(want.DecisionOptions, got.DecisionOptions) ||
		!reflect.DeepEqual(want.DecisionAssumptions, got.DecisionAssumptions) ||
		!reflect.DeepEqual(want.DecisionFacts, got.DecisionFacts) ||
		!reflect.DeepEqual(want.DecisionArtifacts, got.DecisionArtifacts) ||
		!reflect.DeepEqual(want.DecisionBaseRates, got.DecisionBaseRates) ||
		!reflect.DeepEqual(want.DecisionProvenance, got.DecisionProvenance) {
		t.Fatalf("decision admission intent changed: got %#v, want %#v", got, want)
	}
}

func TestDecisionAdmissionIntentSurvivesTodoCheckpointAndEventReplay(t *testing.T) {
	task := decisionAdmissionTask()
	item := todoItemFromSpec(decisionAdmissionTodoSpec(task), "todo-1")
	assertDecisionAdmissionIntent(t, task, taskDefFromTodoItem(item))

	workspace := t.TempDir()
	if err := SaveSession(workspace, &SessionData{Tasks: []*TodoItem{item}}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.Tasks) != 1 {
		t.Fatalf("checkpoint tasks = %#v, want one task", checkpoint)
	}
	assertDecisionAdmissionIntent(t, task, taskDefFromTodoItem(checkpoint.Tasks[0]))

	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatalf("marshal task_created payload: %v", err)
	}
	replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
	if len(replayed) != 1 {
		t.Fatalf("replayed tasks = %#v, want one task", replayed)
	}
	assertDecisionAdmissionIntent(t, task, taskDefFromTodoItem(replayed[0]))
}

func TestExecuteTasksPersistsDecisionAdmissionIntent(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.delegatedTasks = make(map[string]int)
	c.SetSessionData(NewSession())
	c.SetAgentPool(&mockAgentPool{resolveDef: &agent.AgentDef{Name: "worker"}})
	task := decisionAdmissionTask()

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{task})
	if err == nil {
		t.Fatal("ExecuteTasks unexpectedly succeeded")
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("todo items = %#v, want one item", items)
	}
	if !strings.Contains(items[0].Detail, ReasonDecisionBudgetInsufficient) {
		t.Fatalf("task failure = %q, want decision admission failure", items[0].Detail)
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		t.Fatal(err)
	}
	admission, found, err := loadDecisionAdmission(context.Background(), journal, items[0].ID, 1)
	if err != nil || !found {
		t.Fatalf("load normal task admission found=%v err=%v", found, err)
	}
	digest, err := decisionTaskInputDigest(items[0])
	if err != nil || admission.TaskInputDigest != digest {
		t.Fatalf("normal task admission digest=%q reconstructed=%q err=%v", admission.TaskInputDigest, digest, err)
	}
	assertDecisionAdmissionIntent(t, task, taskDefFromTodoItem(items[0]))
}

func TestTaskDefFromTodoItemPreservesMarkerlessLogicalID(t *testing.T) {
	item := todoItemFromSpec(TodoSpec{Agent: "worker", Desc: "markerless occurrence"}, "todo-runtime-1")
	got := taskDefFromTodoItem(item)
	if got.ID != "" {
		t.Fatalf("markerless TaskDef.ID = %q, want empty logical PlanTaskID", got.ID)
	}
}

func TestResumeInterruptedDecisionTaskRetainsAdmissionBeforeFirstDecisionEvent(t *testing.T) {
	workspace := t.TempDir()
	journal := &memoryJournal{}
	const runID = "run-resume-decision-admission"
	first := &Coordinator{
		sessionTime:  time.Now(),
		eventJournal: journal,
		taskTracker:  NewTaskTracker(),
		executionRunID: runID,
		reportStatus: func(StatusEvent) {},
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
			WorkspaceDir: workspace,
			Decision:     dispatchConfig(),
		}},
	}
	first.SetSessionData(NewSession())
	task := decisionAdmissionTask()
	spec := decisionAdmissionTodoSpec(task)
	ids := first.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := first.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := first.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	loadedAdmission, found, err := loadDecisionAdmission(context.Background(), journal, items[0].ID, 1)
	if err != nil || !found {
		t.Fatalf("load created task admission found=%v err=%v", found, err)
	}
	createdDigest, err := decisionTaskInputDigest(items[0])
	if err != nil {
		t.Fatalf("digest created task: %v", err)
	}
	if loadedAdmission.TaskInputDigest != createdDigest {
		t.Fatalf("created task admission digest = %q, want Todo digest %q", loadedAdmission.TaskInputDigest, createdDigest)
	}
	if err := first.CommitTaskTransition(context.Background(), items[0].ID, TaskPending, TaskInProgress, "admitted before decision", "", nil); err != nil {
		t.Fatalf("CommitTaskTransition: %v", err)
	}
	if got := journal.count(agent.EventDecisionStarted); got != 0 {
		t.Fatalf("decision events before simulated crash = %d, want 0", got)
	}

	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.Tasks) != 1 || checkpoint.Tasks[0].Status != TaskInProgress {
		t.Fatalf("checkpoint = %#v, want one in-progress task", checkpoint)
	}
	assertDecisionAdmissionIntent(t, task, taskDefFromTodoItem(checkpoint.Tasks[0]))
	checkpointDigest, err := decisionTaskInputDigest(checkpoint.Tasks[0])
	if err != nil {
		t.Fatalf("digest checkpoint task: %v", err)
	}
	if loadedAdmission.TaskInputDigest != checkpointDigest {
		t.Fatalf("checkpoint task admission digest = %q, want Todo digest %q", loadedAdmission.TaskInputDigest, checkpointDigest)
	}

	workerCalls := 0
	resumed := &Coordinator{
		sessionTime:         time.Now(),
		eventJournal:        journal,
		taskTracker:         NewTaskTracker(),
		executionRunID:      runID,
		workerAgentOverride: &countingTextAgent{calls: &workerCalls, text: "unsafe worker dispatch"},
		reportStatus:        func(StatusEvent) {},
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
			WorkspaceDir: workspace,
			Decision:     dispatchConfig(),
		}},
	}
	resumed.SetAgentPool(&mockAgentPool{resolveDef: &agent.AgentDef{Name: "worker"}})
	resumed.SetSessionData(checkpoint)

	count, err := resumed.ResumeInterruptedTasks(context.Background())
	if count != 1 {
		t.Fatalf("ResumeInterruptedTasks count = %d, want 1", count)
	}
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionBudgetInsufficient) {
		t.Fatalf("ResumeInterruptedTasks = %v, want reconstructed decision admission failure", err)
	}
	if workerCalls != 0 {
		t.Fatalf("worker calls = %d, want 0 when decision admission cannot run", workerCalls)
	}
}

// A task with no decision profile must take the fast exit: no engine, no
// arming, and the tool hooks stay no-ops. This is the compatibility guarantee
// for every team that has not adopted decision profiles (Phase 3.5 invariant 1).
func TestPrepareTaskDecisionIsInertWithoutAProfile(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	journal := c.eventJournal.(*memoryJournal)

	disarm, err := c.prepareTaskDecision(context.Background(), decisionTask(), "todo-1")
	if err != nil {
		t.Fatalf("prepareTaskDecision = %v", err)
	}
	defer disarm()

	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed for a task with no decision profile")
	}
	if len(journal.typesOf()) != 0 {
		t.Fatalf("events = %v, want none", journal.typesOf())
	}
	if denial := c.commitGateDenial(context.Background(), "todo-1", "bash"); denial != "" {
		t.Fatalf("commit gate denied an unarmed task: %q", denial)
	}
}

func TestPrepareTaskDecisionRejectsDurableTaskWithoutAdmission(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	task := decisionAdmissionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{decisionAdmissionTodoSpec(task)})[0]
	_, err := c.prepareTaskDecision(context.Background(), taskDefFromTodoItem(item), item.ID)
	if err == nil || !strings.Contains(err.Error(), "has no decision admission") {
		t.Fatalf("prepareTaskDecision = %v, want missing durable admission", err)
	}
}

func TestPrepareTaskDecisionWithoutProfileAndJournalIsInert(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.eventJournal = eventStoreJournal{}

	disarm, err := c.prepareTaskDecision(context.Background(), decisionTask(), "todo-1")
	if err != nil {
		t.Fatalf("prepareTaskDecision = %v", err)
	}
	defer disarm()
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed without a decision profile")
	}
}

func TestPrepareTaskDecisionEnabledProfileWithoutJournalFailsClosed(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.eventJournal = eventStoreJournal{}
	task := decisionTask()
	task.DecisionProfile = "standard"

	_, err := c.prepareTaskDecision(context.Background(), task, "todo-1")
	if err == nil || !strings.Contains(err.Error(), "event journal is unavailable") {
		t.Fatalf("prepareTaskDecision = %v, want unavailable journal error", err)
	}
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed without a durable journal")
	}
}

// The reserved off profile is equally inert, even when set explicitly.
func TestPrepareTaskDecisionOffProfileIsInert(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.SetDecisionProfile(agent.DecisionProfileOff)

	task := decisionTask()
	task.DecisionProfile = "standard"
	disarm, err := c.prepareTaskDecision(context.Background(), task, "todo-1")
	if err != nil {
		t.Fatalf("prepareTaskDecision = %v", err)
	}
	defer disarm()
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("the off override still armed a discipline")
	}
}

func TestPrepareTaskDecisionUnreadableJournalFailsClosedWithProfileOff(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.eventJournal = failingJournal{err: errors.New("journal read failed")}

	_, err := c.prepareTaskDecision(context.Background(), decisionTask(), "todo-1")
	if err == nil || !strings.Contains(err.Error(), "journal read failed") {
		t.Fatalf("prepareTaskDecision = %v, want unreadable journal error", err)
	}
}

func TestPrepareTaskDecisionReadableEmptyJournalWithProfileOffIsInert(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())

	disarm, err := c.prepareTaskDecision(context.Background(), decisionTask(), "todo-1")
	if err != nil {
		t.Fatalf("prepareTaskDecision = %v", err)
	}
	defer disarm()
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed with an empty journal and profile off")
	}
	if got := len(c.eventJournal.(*memoryJournal).typesOf()); got != 0 {
		t.Fatalf("decision activity count = %d, want 0", got)
	}
}

// An unknown profile fails before any work starts, whichever layer set it.
func TestPrepareTaskDecisionRejectsUnknownProfile(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	task := decisionTask()
	task.DecisionProfile = "paranoid"

	_, err := c.prepareTaskDecision(context.Background(), task, "todo-1")
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionProfileUnknown) {
		t.Fatalf("prepareTaskDecision = %v, want %s", err, ReasonDecisionProfileUnknown)
	}
}

// A task that selects a profile but declares no alternatives is a
// configuration error, not an empty decision.
func TestFormTaskDecisionRequiresDeclaredOptions(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	task := decisionTask()
	task.DecisionProfile = "standard"
	task.DecisionOptions = nil

	_, err := c.prepareTaskDecision(context.Background(), task, "todo-1")
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionMissingAlternative) {
		t.Fatalf("prepareTaskDecision = %v, want %s", err, ReasonDecisionMissingAlternative)
	}
}

// Running with no judge model would be a silent reduction to zero judges,
// which §34 forbids. It must fail closed instead.
func TestFormTaskDecisionFailsClosedWithoutAJudgeModel(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	task := decisionTask()
	task.DecisionProfile = "standard"

	_, err := c.prepareTaskDecision(context.Background(), task, "todo-1")
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionBudgetInsufficient) {
		t.Fatalf("prepareTaskDecision = %v, want a fail-closed on the missing judge model", err)
	}
	if c.disciplineFor("todo-1") != nil {
		t.Fatal("a discipline was armed despite the decision failing")
	}
}

func TestValidateDecisionProfiles(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())

	if err := c.ValidateDecisionProfiles([]TaskDef{{ID: "t1"}, {ID: "t2", DecisionProfile: "standard"}}); err != nil {
		t.Fatalf("ValidateDecisionProfiles = %v", err)
	}

	err := c.ValidateDecisionProfiles([]TaskDef{{ID: "t2", DecisionProfile: "paranoid"}})
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionProfileUnknown) {
		t.Fatalf("unknown task profile = %v, want %s", err, ReasonDecisionProfileUnknown)
	}

	c.SetDecisionProfile("paranoid")
	err = c.ValidateDecisionProfiles(nil)
	if err == nil || !strings.Contains(err.Error(), "--decision-profile") {
		t.Fatalf("unknown override = %v, want it named in the error", err)
	}

	c.SetDecisionProfile("standard")
	if err := c.ValidateDecisionProfiles(nil); err != nil {
		t.Fatalf("known override rejected: %v", err)
	}
	c.SetDecisionProfile(agent.DecisionProfileOff)
	if err := c.ValidateDecisionProfiles(nil); err != nil {
		t.Fatalf("the reserved off override was rejected: %v", err)
	}
}

// The precedence chain's top layer has to actually reach the resolver.
func TestDecisionProfileOverrideReachesResolution(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.SetDecisionProfile("standard")

	resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), TaskDef{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Profile != "standard" || resolution.Source != DecisionProfileSourceRequest {
		t.Fatalf("resolution = %#v, want standard from the request layer", resolution)
	}
	c.SetDecisionProfile("  ")
	if got := c.DecisionProfileOverride(); got != "" {
		t.Fatalf("DecisionProfileOverride = %q, want whitespace trimmed away", got)
	}
}

// The attempt counter was initialized to `MaxRetries * 0` and was therefore
// always zero, so the attempts kill criterion could never fire. It must now
// track the task's real attempt.
func TestArmedDisciplineTracksRealAttempt(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "migrate"}})
	todoID := items[0].ID

	policy := disciplinePolicy(CommitGatePolicy{}, StopPolicy{
		CheckpointEvery: 1,
		KillCriteria:    []KillCriterion{{ID: "attempts", Kind: agent.KillKindAttempts, Threshold: 2}},
	}, ReplanPolicy{})

	if err := c.armDiscipline(context.Background(), todoID, TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}
	if got := c.disciplineFor(todoID).attempt; got != 1 {
		t.Fatalf("attempt = %d, want the first attempt to be 1", got)
	}
	if decision := c.recordToolCall(context.Background(), todoID, false); decision.Action != CheckpointContinue {
		t.Fatalf("first attempt = %#v, want continue below the threshold", decision)
	}

	// A retried task re-arms at a higher attempt, and the criterion fires.
	c.disarmDiscipline(todoID)
	c.taskTracker.TodoList().ResetForRetry(todoID, "simulated retry")
	if err := c.armDiscipline(context.Background(), todoID, TaskDef{ID: "t1"}, policy, &DecisionRecord{ID: "dec-1"}); err != nil {
		t.Fatal(err)
	}
	if got := c.disciplineFor(todoID).attempt; got != 2 {
		t.Fatalf("attempt = %d, want Retries+1", got)
	}
	decision := c.recordToolCall(context.Background(), todoID, false)
	if decision.Action != CheckpointStop || decision.Criterion != "attempts" {
		t.Fatalf("decision = %#v, want the attempts criterion to fire", decision)
	}
}

func TestTaskAttemptDefaultsToOne(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	if got := c.taskAttempt("unknown-todo"); got != 1 {
		t.Fatalf("taskAttempt = %d, want 1", got)
	}
	var nilCoordinator *Coordinator
	if got := nilCoordinator.taskAttempt("x"); got != 1 {
		t.Fatalf("taskAttempt on nil = %d, want 1", got)
	}
}

// decision-options and decision-assumptions are configuration-only for the
// same reason decision-profile is (spec §19).
func TestDecisionOptionsCannotComeFromTaskPayload(t *testing.T) {
	payload := `{"agent":"worker","goal":"go","decision_options":[{"id":"x","kind":"execute"}],` +
		`"decision_assumptions":[{"id":"A1","statement":"s"}]}`
	var task TaskDef
	if err := json.Unmarshal([]byte(payload), &task); err != nil {
		t.Fatal(err)
	}
	if len(task.DecisionOptions) != 0 || len(task.DecisionAssumptions) != 0 {
		t.Fatalf("a task payload set decision options/assumptions: %#v", task)
	}
}
