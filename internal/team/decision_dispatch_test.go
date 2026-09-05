package team

import (
	"context"
	"encoding/json"
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
		ID:    "t1",
		Agent: "worker",
		Goal:  "migrate the ingest pipeline",
		DecisionOptions: []DecisionOption{
			{ID: "migrate", Kind: OptionExecute, Title: "Migrate now"},
			{ID: "wait", Kind: OptionDefer, Title: "Wait a quarter"},
		},
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
