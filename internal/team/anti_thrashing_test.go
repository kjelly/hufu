package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestFailureFingerprintIgnoresVolatileMetadata(t *testing.T) {
	a := NewFailureFingerprint("build", "worker", "bash", FailureVerify, "task 12 attempt 1 failed at 2026-07-30T10:00:00Z: exit code 2")
	b := NewFailureFingerprint("build", "worker", "bash", FailureVerify, "task 99 attempt 4 failed at 2026-07-31T11:12:00Z: exit code 2")
	if !SameFailureFingerprint(a, b) {
		t.Fatalf("equivalent failures differ: %#v %#v", a, b)
	}
	if a.Digest == "" || len(a.Digest) != 68 {
		t.Fatalf("unexpected digest %q", a.Digest)
	}
}

func TestRecoveryHypothesisRequiresDifferentStrategyAfterRepeat(t *testing.T) {
	h := RecoveryHypothesis{ObservedFailure: "exit 2", HypothesizedCause: "bad input", ProposedChange: "validate input", ExpectedChange: "exit 0", Strategy: RecoveryStrategyRetry}
	if err := h.Validate(false, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(true, RecoveryStrategyRetry); err == nil {
		t.Fatal("same strategy should be rejected")
	}
	h.DifferenceFromPrior = "switch to validation before build"
	h.Strategy = RecoveryStrategyToolChange
	if err := h.Validate(true, RecoveryStrategyRetry); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHypothesisBindsFailedCriterion(t *testing.T) {
	h := RecoveryHypothesis{
		CriterionID: "build", ObservedFailure: "exit 1", HypothesizedCause: "bad input",
		ProposedChange: "validate input", DifferenceFromPrior: "use validation", ExpectedChange: "exit 0",
		Strategy: RecoveryStrategyToolChange,
	}
	if err := h.ValidateForCriterion("tests", true, RecoveryStrategyRetry); err == nil {
		t.Fatal("hypothesis for build must not satisfy failed tests criterion")
	}
	if err := h.ValidateForCriterion("build", true, RecoveryStrategyRetry); err != nil {
		t.Fatalf("matching criterion rejected: %v", err)
	}
}

func TestRecoveryHypothesisRejectsUnsafeReplayWithoutReconcile(t *testing.T) {
	h := RecoveryHypothesis{CriterionID: "build", ObservedFailure: "exit 1", HypothesizedCause: "bad input", ProposedChange: "validate", ExpectedChange: "exit 0", Strategy: RecoveryStrategyRetry}
	unsafe := TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryRetry}
	if err := h.ValidateForTask("build", true, RecoveryStrategyRetry, unsafe); err == nil {
		t.Fatal("unsafe replay hypothesis without reconcile was accepted")
	}
	h.Strategy = RecoveryStrategyReconcile
	h.DifferenceFromPrior = "reconcile state before any retry"
	if err := h.ValidateForTask("build", true, RecoveryStrategyRetry, unsafe); err != nil {
		t.Fatalf("reconcile strategy rejected for unsafe task: %v", err)
	}
}

func TestRepeatedHypothesisRequiresCriterionID(t *testing.T) {
	h := RecoveryHypothesis{ObservedFailure: "exit 1", HypothesizedCause: "bad input", ProposedChange: "validate", ExpectedChange: "exit 0", Strategy: RecoveryStrategyToolChange}
	if err := h.ValidateForCriterion("", true, RecoveryStrategyRetry); err == nil {
		t.Fatal("empty failed criterion bypassed repeated binding")
	}
}

func TestReplayPolicyBlocksExplicitManualAndNever(t *testing.T) {
	for _, policy := range []RecoveryPolicy{RecoveryManual, RecoveryReconcile, RecoveryNever} {
		if CanAutomaticallyReplay(TaskDef{Recovery: policy}) {
			t.Fatalf("recovery policy %q must not auto-replay", policy)
		}
	}
	if !CanAutomaticallyReplay(TaskDef{Recovery: RecoveryRetry}) {
		t.Fatal("retry policy with replayable task should auto-replay")
	}
}

func TestFailedCriterionSelectionSkipsPassedCriteria(t *testing.T) {
	c := &Coordinator{sessionData: &SessionData{CriterionResults: []CriterionResult{
		{ID: "build", State: CriterionPassed}, {ID: "tests", State: CriterionFailed},
	}}}
	item := &TodoItem{Advances: []string{"build", "tests"}}
	if got := c.failedCriterionForTask(item); got != "tests" {
		t.Fatalf("failed criterion = %q, want tests", got)
	}
}

func TestRepeatedRepairFailureRejectsWrongCriterionHypothesis(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Name: "binding", Reliability: agent.ReliabilityConfig{HardEnforcement: true},
		}},
		sessionData: &SessionData{CriterionResults: []CriterionResult{{ID: "build", State: CriterionFailed}, {ID: "tests", State: CriterionPassed}}},
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build", "tests"},
		RecoveryHypothesis: &RecoveryHypothesis{CriterionID: "tests", ObservedFailure: "exit 1", HypothesizedCause: "bad input", ProposedChange: "validate", DifferenceFromPrior: "new strategy", ExpectedChange: "exit 0", Strategy: RecoveryStrategyToolChange},
	}})[0]
	c.PersistFailure("worker", item.Desc, item.ID, "verification failed: exit code 1")
	c.PersistFailure("worker", item.Desc, item.ID, "verification failed: exit code 1")
	if !c.antiThrashingHardBlocked() {
		t.Fatal("wrong-criterion repeated hypothesis should trigger hard block")
	}
}

func TestReplayRetainsRejectedSameRecoveryStrategy(t *testing.T) {
	hypothesis := &RecoveryHypothesis{
		CriterionID: "build", ObservedFailure: "exit code 1", HypothesizedCause: "bad input",
		ProposedChange: "validate input", DifferenceFromPrior: "retry with the same repair", ExpectedChange: "exit 0",
		Strategy: RecoveryStrategyRetry,
	}
	config := agent.TeamConfig{Reliability: agent.ReliabilityConfig{HardEnforcement: true}}
	live := &Coordinator{session: &TeamSession{Config: config}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	liveItems := live.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "repair one", Kind: TaskKindRepair, Advances: []string{"build"}, RecoveryHypothesis: hypothesis},
		{Agent: "worker", Desc: "repair two", Kind: TaskKindRepair, Advances: []string{"build"}, RecoveryHypothesis: cloneRecoveryHypothesis(hypothesis)},
	})
	live.PersistFailure("worker", liveItems[0].Desc, liveItems[0].ID, "exit code 1")
	live.PersistFailure("worker", liveItems[1].Desc, liveItems[1].ID, "exit code 1")
	fp := NewFailureFingerprint("build", "worker", "task:repair", FailureExecution, "exit code 1")
	if !live.antiThrashing.strategyWasRejected(fp.Digest, RecoveryStrategyRetry) || !live.antiThrashingHardBlocked() {
		t.Fatalf("live same-strategy rejection was not retained: rejected=%v blocked=%v", live.antiThrashing.strategyWasRejected(fp.Digest, RecoveryStrategyRetry), live.antiThrashingHardBlocked())
	}

	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "strategy-replay", "strategy-session")
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) json.RawMessage {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	for _, taskID := range []string{"repair-one", "repair-two"} {
		if err := store.Append(RunEvent{Type: "task_created", TaskID: taskID, Actor: "worker", Payload: encode(map[string]any{
			"id": taskID, "desc": taskID, "status": TaskError, "kind": TaskKindRepair,
			"advances": []string{"build"}, "recovery_hypothesis": hypothesis,
		})}); err != nil {
			t.Fatal(err)
		}
		if err := store.Append(RunEvent{Type: "failure_fingerprint", TaskID: taskID, Actor: "worker", Payload: encode(map[string]any{"fingerprint": fp})}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("event store replay wrote %d events, want 4", len(events))
	}
	for i, event := range events {
		if event.Hash == "" || (i > 0 && event.PreviousHash != events[i-1].Hash) {
			t.Fatalf("event store hash chain is not durable at %d: %#v", i, event)
		}
	}
	replayedSession := ReduceToSessionData(events)
	if len(replayedSession.Tasks) != 2 || len(replayedSession.Tasks[0].FailureFingerprints) != 1 || len(replayedSession.Tasks[1].FailureFingerprints) != 1 {
		t.Fatalf("event reducer lost strategy replay evidence: %#v", replayedSession.Tasks)
	}
	replayed := &Coordinator{session: &TeamSession{Config: config}, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(replayedSession)
	if !replayed.antiThrashing.strategyWasRejected(fp.Digest, RecoveryStrategyRetry) || !replayed.antiThrashingHardBlocked() {
		t.Fatalf("replay accepted live-rejected same strategy: rejected=%v blocked=%v", replayed.antiThrashing.strategyWasRejected(fp.Digest, RecoveryStrategyRetry), replayed.antiThrashingHardBlocked())
	}
}

func TestAntiThrashingLimitsCountWithoutTaskIDReuse(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSameFailureFingerprint: 2, HardEnforcement: true}
	first := &TodoItem{ID: "1", Kind: TaskKindRepair, Advances: []string{"build"}}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	if repeated, limited := state.record(first, fp, RecoveryStrategyReflection, limits); repeated || limited {
		t.Fatal("first occurrence must warn only")
	}
	second := &TodoItem{ID: "2", Kind: TaskKindRepair, Advances: []string{"build"}}
	if repeated, limited := state.record(second, fp, RecoveryStrategyReflection, limits); !repeated || !limited {
		t.Fatal("second equivalent occurrence should hit configured limit")
	}
	if state.Counts[fp.Digest] != 2 {
		t.Fatalf("count = %d, want 2", state.Counts[fp.Digest])
	}
	if !state.HardBlocked {
		t.Fatal("hard enforcement should block subsequent equivalent work")
	}
}

func TestReliabilityConfigParses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: reliable\nacceptance: 'true'\nreliability:\n  max-diagnostic-tasks-without-progress: 3\n  max-same-failure-fingerprint: 2\n  max-repairs-per-criterion: 5\n  hard-enforcement: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.MaxDiagnosticTasksWithoutProgress != 3 || !cfg.Reliability.HardEnforcement {
		t.Fatalf("unexpected reliability config: %#v", cfg.Reliability)
	}
}

func TestPersistFailureStoresFingerprintOnCanonicalTaskAndSession(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "persist"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	c.PersistFailure("worker", "repair", item.ID, "verification failed: exit code 1")
	current := c.taskTracker.TodoList().Items()[0]
	if len(current.FailureFingerprints) != 1 || current.FailureFingerprints[0].Digest == "" {
		t.Fatalf("canonical task lost fingerprint: %#v", current.FailureFingerprints)
	}
	loaded := LoadSession(workspace)
	if loaded == nil || len(loaded.Tasks) != 1 || len(loaded.Tasks[0].FailureFingerprints) != 1 {
		t.Fatalf("session checkpoint lost fingerprint: %#v", loaded)
	}
}

func TestRepeatedIdenticalFailureOccurrencesPersistAndRebuild(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 2, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "repairer", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	c.PersistFailure("repairer", item.Desc, item.ID, "verification failed: exit code 1")
	c.PersistFailure("repairer", item.Desc, item.ID, "verification failed: exit code 1")
	current := c.taskTracker.TodoList().Items()[0]
	if len(current.FailureFingerprints) != 1 || current.FailureFingerprints[0].Occurrences != 2 {
		t.Fatalf("identical failure occurrences = %#v, want one fingerprint with occurrences=2", current.FailureFingerprints)
	}
	if got := c.antiThrashing.RepairsByCriterion["build"]; got != 2 {
		t.Fatalf("live repair count = %d, want 2", got)
	}
	restarted := &Coordinator{
		session:     c.session,
		taskTracker: NewTaskTracker(),
	}
	restarted.SetSessionData(LoadSession(workspace))
	if got := restarted.antiThrashing.RepairsByCriterion["build"]; got != 2 {
		t.Fatalf("replayed repair count = %d, want 2", got)
	}
	if !restarted.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, restarted.taskTracker.TodoList().Items()[0]) {
		t.Fatal("replayed repeated failures did not enforce repair limit")
	}
}

func TestLiveSuccessfulRepairLimitMatchesReplayEnforcement(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 1, HardEnforcement: true}}}
	live := &Coordinator{session: session, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	item := live.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "repairer", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	live.taskTracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "running")
	live.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "completed")
	live.reEvaluateAffectedCriteria(context.Background(), live.taskTracker.TodoList().Items()[0])
	if got := live.antiThrashing.RepairsByCriterion["build"]; got != 1 {
		t.Fatalf("live repair count = %d, want 1", got)
	}
	if !live.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, live.taskTracker.TodoList().Items()[0]) {
		t.Fatal("live successful repair did not establish hard blocked scope")
	}

	saved := NewSession()
	saved.Tasks = []*TodoItem{{ID: "repair", Kind: TaskKindRepair, Advances: []string{"build"}, Status: TaskDone}}
	replayed := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(saved)
	if !replayed.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("replayed successful repair did not establish the same hard blocked scope")
	}
}

func TestFailureFingerprintEventReplayDoesNotDoubleCountSnapshots(t *testing.T) {
	fp := NewFailureFingerprint("build", "repairer", "probe", FailureVerify, "exit code 1")
	first := fp
	second := fp
	second.Occurrences = 2
	created, _ := json.Marshal(map[string]any{"id": "repair", "kind": TaskKindRepair, "advances": []string{"build"}, "status": TaskError, "failure_fingerprints": []FailureFingerprint{first}})
	failure, _ := json.Marshal(map[string]any{"fingerprint": second})
	items := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: "repair", Payload: created},
		{Type: "failure_fingerprint", TaskID: "repair", Payload: failure},
	})
	if len(items) != 1 || len(items[0].FailureFingerprints) != 1 || items[0].FailureFingerprints[0].Occurrences != 2 {
		t.Fatalf("event replay double-counted fingerprint snapshot: %#v", items[0].FailureFingerprints)
	}
}

func TestFailureFingerprintEventReplayRestoresTaskEvidence(t *testing.T) {
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	allowReplay := false
	hypothesis := &RecoveryHypothesis{CriterionID: "build", ObservedFailure: "exit 1", HypothesizedCause: "bad input", ProposedChange: "validate", ExpectedChange: "exit 0", Strategy: RecoveryStrategyToolChange}
	payload, err := json.Marshal(map[string]any{"fingerprint": fp, "count": 2})
	if err != nil {
		t.Fatal(err)
	}
	items := ReduceToTodoList([]RunEvent{{Type: "failure_fingerprint", TaskID: "42", Actor: "worker", Payload: payload}})
	if len(items) != 1 || len(items[0].FailureFingerprints) != 1 || items[0].FailureFingerprints[0].Digest != fp.Digest {
		t.Fatalf("event replay lost fingerprint: %#v", items)
	}
	session := ReduceToSessionData([]RunEvent{{Type: "failure_fingerprint", TaskID: "42", Actor: "worker", Payload: payload}})
	if len(session.Tasks) != 1 || len(session.Tasks[0].FailureFingerprints) != 1 {
		t.Fatalf("session replay lost fingerprint projection: %#v", session.Tasks)
	}
	// Task checkpoint replay must retain the replay contract and hypothesis.
	taskPayload, _ := json.Marshal(map[string]any{"id": "42", "status": "pending", "execution": ExecutionContract{AllowsReplay: &allowReplay}, "side_effect": SideEffectExternalWrite, "recovery": RecoveryReconcile, "reconcile_tool": "probe-state", "recovery_hypothesis": hypothesis})
	replayed := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "42", Payload: taskPayload}})
	if replayed[0].Execution.AllowsReplay == nil || *replayed[0].Execution.AllowsReplay || replayed[0].SideEffect != SideEffectExternalWrite || replayed[0].Recovery != RecoveryReconcile || replayed[0].ReconcileTool != "probe-state" || replayed[0].RecoveryHypothesis.CriterionID != "build" {
		t.Fatalf("replay contract/hypothesis lost: %#v", replayed[0])
	}
}

func TestResetForRetryClearsRecoveryState(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	item := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "resume"}})[0]
	tl.UpdateStatus(item.ID, TaskInProgress, "running")
	tl.SetRecoveryState(item.ID, RecoveryStateUnknown)
	tl.SetLastOperation(item.ID, "bash")
	if err := tl.SetProgress(item.ID, ProgressAdvanced, []string{"build"}); err != nil {
		t.Fatal(err)
	}
	tl.ResetForRetry(item.ID, "retry")
	got := tl.Items()[0]
	if got.Status != TaskPending || got.RecoveryState != RecoveryStateNotStarted || got.LastOperation != "" || got.Progress != ProgressUnknown || len(got.ProgressCriteria) != 0 {
		t.Fatalf("retry cleanup left stale state: %#v", got)
	}
}

func TestDiagnosticLimitCountsLinkedDiagnosticWithoutProgress(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "diagnostic", Reliability: agent.ReliabilityConfig{MaxDiagnosticTasksWithoutProgress: 1, HardEnforcement: true}}},
		projectDir:  workspace,
		sessionData: NewSession(),
		acceptanceSpec: &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
			ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "false"},
		}}},
	}
	item := &TodoItem{ID: "diagnostic-1", Kind: TaskKindDiagnostic, Status: TaskDone, Advances: []string{"build"}}
	c.reEvaluateAffectedCriteria(context.Background(), item)
	if got := c.Metrics().DiagnosticTasksSinceProgress; got != 1 {
		t.Fatalf("linked diagnostic count = %d, want 1", got)
	}
	if !c.antiThrashingHardBlocked() {
		t.Fatal("diagnostic limit should trip hard enforcement")
	}
	// Re-evaluation of the same completed task must not count it twice.
	c.reEvaluateAffectedCriteria(context.Background(), item)
	if got := c.Metrics().DiagnosticTasksSinceProgress; got != 1 {
		t.Fatalf("linked diagnostic was counted twice: %d", got)
	}
}

func TestAntiThrashingBlocksOnlyRelatedPendingScope(t *testing.T) {
	s := AntiThrashingState{
		HardBlocked:     true,
		BlockedCriteria: map[string]bool{"build": true},
	}
	if !s.blocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, nil) {
		t.Fatal("expected task advancing blocked criterion to be blocked")
	}
	if s.blocksTask(TaskDef{Kind: TaskKindOutcome, Advances: []string{"docs"}}, nil) {
		t.Fatal("unrelated pending task was blocked by another criterion's limit")
	}
}

func TestHardScopeSeparatesRepairAndOutcomeForSameCriterion(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSameFailureFingerprint: 1, HardEnforcement: true}
	fp := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "exit code 1")
	repair := &TodoItem{ID: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}
	if _, limited := state.record(repair, fp, RecoveryStrategyRetry, limits); !limited {
		t.Fatal("expected fingerprint limit to trip")
	}
	repairTask := TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}
	if !state.blocksTask(repairTask, &TodoItem{FailureFingerprints: []FailureFingerprint{fp}}) {
		t.Fatal("equivalent repair scope was not blocked")
	}
	differentRepair := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "missing artifact")
	if state.blocksTask(repairTask, &TodoItem{FailureFingerprints: []FailureFingerprint{differentRepair}}) {
		t.Fatal("different repair failure was incorrectly blocked")
	}
	if state.blocksTask(TaskDef{Kind: TaskKindOutcome, Advances: []string{"build"}}, nil) {
		t.Fatal("outcome task on the same criterion was incorrectly blocked")
	}
}

func TestHardEnforcementRebuildsFromPersistedFingerprints(t *testing.T) {
	workspace := t.TempDir()
	fp := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "exit code 1")
	saved := NewSession()
	saved.Tasks = []*TodoItem{
		{ID: "repair-1", Kind: TaskKindRepair, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "repair-2", Kind: TaskKindRepair, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxSameFailureFingerprint: 2, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(saved)

	if got := c.failureFingerprintCount(fp.Digest); got != 2 {
		t.Fatalf("persisted fingerprint count = %d, want 2", got)
	}
	if !c.antiThrashingHardBlocked() {
		t.Fatal("replayed fingerprint threshold did not restore hard enforcement")
	}
	if !c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("replayed repair scope was not restored")
	}
	if c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindOutcome, Advances: []string{"build"}}, nil) {
		t.Fatal("replayed repair scope incorrectly blocked outcome work")
	}
}

func TestHardEnforcementRebuildsSuccessfulRepairWithoutFingerprint(t *testing.T) {
	workspace := t.TempDir()
	saved := NewSession()
	saved.Tasks = []*TodoItem{{ID: "repair-success", Kind: TaskKindRepair, Advances: []string{"build"}, Status: TaskDone}}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 1, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(saved)
	if got := c.antiThrashing.RepairsByCriterion["build"]; got != 1 {
		t.Fatalf("successful repair count after replay = %d, want 1", got)
	}
	if !c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("max-repairs-per-criterion did not block after replaying successful repair")
	}
}

func TestHardEnforcementRebuildsFailedThenSuccessfulRepairAttempts(t *testing.T) {
	workspace := t.TempDir()
	fp := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "exit code 1")
	saved := NewSession()
	saved.Tasks = []*TodoItem{{
		ID:                  "repair-recovered",
		Kind:                TaskKindRepair,
		Advances:            []string{"build"},
		Status:              TaskDone,
		Retries:             1,
		FailureFingerprints: []FailureFingerprint{fp},
	}}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 2, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(saved)
	if got := c.antiThrashing.RepairsByCriterion["build"]; got != 2 {
		t.Fatalf("failed-then-success repair count after replay = %d, want 2", got)
	}
	if !c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("retry-inclusive repair limit did not block after replay")
	}
}

func TestRepairAttemptCountUsesTheLargerEvidenceBound(t *testing.T) {
	fpA := NewFailureFingerprint("build", "repairer", "probe-a", FailureVerify, "exit code 1")
	fpB := NewFailureFingerprint("build", "repairer", "probe-b", FailureVerify, "exit code 2")
	cases := []struct {
		name string
		item *TodoItem
		want int
	}{
		{name: "two distinct failures across two retries", item: &TodoItem{Kind: TaskKindRepair, Retries: 2, FailureFingerprints: []FailureFingerprint{fpA, fpB}, Status: TaskDone}, want: 3},
		{name: "repeated identical failures are not lost", item: &TodoItem{Kind: TaskKindRepair, FailureFingerprints: []FailureFingerprint{{Digest: fpA.Digest, Occurrences: 3}}, Status: TaskError}, want: 3},
		{name: "duplicate fingerprint does not inflate attempts", item: &TodoItem{Kind: TaskKindRepair, Retries: 2, FailureFingerprints: []FailureFingerprint{fpA, fpA}, Status: TaskError}, want: 3},
		{name: "early exit uses actual retry count not max cap", item: &TodoItem{Kind: TaskKindRepair, Retries: 1, MaxRetries: 9, FailureFingerprints: []FailureFingerprint{fpA}, Status: TaskError}, want: 2},
		{name: "partial success still counts actual attempts", item: &TodoItem{Kind: TaskKindRepair, Retries: 2, FailureFingerprints: []FailureFingerprint{fpA}, Status: TaskDone}, want: 3},
		{name: "success after repeated failures includes final success", item: &TodoItem{Kind: TaskKindRepair, FailureFingerprints: []FailureFingerprint{{Digest: fpA.Digest, Occurrences: 3}}, Status: TaskDone}, want: 4},
		{name: "clean success counts once", item: &TodoItem{Kind: TaskKindRepair, Status: TaskDone}, want: 1},
		{name: "new pending task has no attempt", item: &TodoItem{Kind: TaskKindRepair, Status: TaskPending}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repairAttemptCount(tc.item); got != tc.want {
				t.Fatalf("repairAttemptCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLiveRepairAttemptCountMatchesReplayWithRetryEvidence(t *testing.T) {
	item := &TodoItem{ID: "repair-live", Kind: TaskKindRepair, Advances: []string{"build"}, Status: TaskDone, Progress: ProgressUnknown, Retries: 2}
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 3, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	c.reEvaluateAffectedCriteria(context.Background(), item)
	if got := c.Metrics().RepairAttemptsByCriterion["build"]; got != 3 {
		t.Fatalf("live retry-only repair count = %d, want 3", got)
	}

	replayed := NewSession()
	replayed.Tasks = []*TodoItem{{ID: item.ID, Kind: item.Kind, Advances: []string{"build"}, Status: TaskDone, Retries: 2}}
	rebuilt := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 3, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	rebuilt.SetSessionData(replayed)
	if got := rebuilt.Metrics().RepairAttemptsByCriterion["build"]; got != 3 {
		t.Fatalf("replayed retry-only repair count = %d, want 3", got)
	}
	if !c.antiThrashingHardBlocked() || !rebuilt.antiThrashingHardBlocked() {
		t.Fatalf("live/replay hard enforcement diverged: live=%v replay=%v", c.antiThrashingHardBlocked(), rebuilt.antiThrashingHardBlocked())
	}
}

func TestRebuildDoesNotDoubleCountDistinctFailuresAndRetries(t *testing.T) {
	fpA := NewFailureFingerprint("build", "repairer", "probe-a", FailureVerify, "exit code 1")
	fpB := NewFailureFingerprint("build", "repairer", "probe-b", FailureVerify, "exit code 2")
	saved := NewSession()
	saved.Tasks = []*TodoItem{{
		ID:                  "repair-multi-failure",
		Kind:                TaskKindRepair,
		Advances:            []string{"build"},
		Status:              TaskError,
		Retries:             2,
		FailureFingerprints: []FailureFingerprint{fpA, fpB},
	}}
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 3, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(saved)
	if got := c.antiThrashing.RepairsByCriterion["build"]; got != 3 {
		t.Fatalf("replayed repair attempts = %d, want 3", got)
	}
	if !c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("max-repairs-per-criterion did not trigger at the actual attempt count")
	}
}

func TestHardEnforcementRebuildsAfterEventReplay(t *testing.T) {
	fp := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "exit code 1")
	taskPayload := func(id string) json.RawMessage {
		payload, _ := json.Marshal(map[string]any{"id": id, "kind": TaskKindRepair, "advances": []string{"build"}, "status": TaskError})
		return payload
	}
	failurePayload, _ := json.Marshal(map[string]any{"fingerprint": fp})
	events := []RunEvent{
		{Type: "task_created", TaskID: "repair-1", Payload: taskPayload("repair-1")},
		{Type: "failure_fingerprint", TaskID: "repair-1", Payload: failurePayload},
		{Type: "task_created", TaskID: "repair-2", Payload: taskPayload("repair-2")},
		{Type: "failure_fingerprint", TaskID: "repair-2", Payload: failurePayload},
	}
	replayed := ReduceToSessionData(events)
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxSameFailureFingerprint: 2, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(replayed)
	if got := c.failureFingerprintCount(fp.Digest); got != 2 || !c.antiThrashingHardBlocked() {
		t.Fatalf("event replay did not restore hard state: count=%d blocked=%v", got, c.antiThrashingHardBlocked())
	}
	if c.antiThrashingBlocksTask(TaskDef{Kind: TaskKindOutcome, Advances: []string{"build"}}, nil) {
		t.Fatal("event-replayed repair scope blocked outcome work")
	}
}

func TestRepairRetriesSurviveEventReplayForRebuild(t *testing.T) {
	fp := NewFailureFingerprint("build", "repairer", "go test", FailureVerify, "exit code 1")
	taskPayload, _ := json.Marshal(map[string]any{
		"id": "repair-recovered", "kind": TaskKindRepair, "advances": []string{"build"}, "status": TaskDone, "retries": 1,
	})
	failurePayload, _ := json.Marshal(map[string]any{"fingerprint": fp})
	replayed := ReduceToSessionData([]RunEvent{
		{Type: "task_created", TaskID: "repair-recovered", Payload: taskPayload},
		{Type: "failure_fingerprint", TaskID: "repair-recovered", Payload: failurePayload},
	})
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 2, HardEnforcement: true}}},
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(replayed)
	if len(replayed.Tasks) != 1 || replayed.Tasks[0].Retries != 1 {
		t.Fatalf("replayed retries = %#v, want 1", replayed.Tasks)
	}
	if got := c.antiThrashing.RepairsByCriterion["build"]; got != 2 {
		t.Fatalf("event-replayed failed-then-success count = %d, want 2", got)
	}
}

func TestCriterionProgressReplayResetsDiagnosticRebuildCounter(t *testing.T) {
	first, _ := json.Marshal(map[string]any{"id": "diagnostic-1", "kind": TaskKindDiagnostic, "advances": []string{"build"}, "status": TaskDone})
	second, _ := json.Marshal(map[string]any{"id": "diagnostic-2", "kind": TaskKindDiagnostic, "advances": []string{"build"}, "status": TaskDone})
	progress, _ := json.Marshal(map[string]any{"progress": ProgressAdvanced})
	firstFP, _ := json.Marshal(map[string]any{"fingerprint": NewFailureFingerprint("build", "diagnostic", "probe-1", FailureVerify, "not ready")})
	secondFP, _ := json.Marshal(map[string]any{"fingerprint": NewFailureFingerprint("build", "diagnostic", "probe-2", FailureVerify, "not ready")})
	replayed := ReduceToSessionData([]RunEvent{
		{Type: "task_created", TaskID: "diagnostic-1", Payload: first},
		{Type: "failure_fingerprint", TaskID: "diagnostic-1", Payload: firstFP},
		{Type: "criterion_re_evaluated", TaskID: "diagnostic-1", Payload: progress},
		{Type: "task_created", TaskID: "diagnostic-2", Payload: second},
		{Type: "failure_fingerprint", TaskID: "diagnostic-2", Payload: secondFP},
	})
	state := AntiThrashingState{}
	state.rebuild(replayed.Tasks, agent.ReliabilityConfig{MaxDiagnosticTasksWithoutProgress: 2, HardEnforcement: true})
	if state.DiagnosticSinceProgress != 1 {
		t.Fatalf("diagnostic counter after replayed progress = %d, want 1", state.DiagnosticSinceProgress)
	}
}

func TestCriterionProgressResetsRepairCounterForLiveAndReplay(t *testing.T) {
	first, _ := json.Marshal(map[string]any{
		"id": "repair-1", "kind": TaskKindRepair, "advances": []string{"build"},
		"status": TaskDone, "progress": ProgressNoChange,
	})
	progress, _ := json.Marshal(map[string]any{
		"id": "outcome-1", "kind": TaskKindOutcome, "advances": []string{"build"},
		"status": TaskDone, "progress": ProgressAdvanced,
	})
	second, _ := json.Marshal(map[string]any{
		"id": "repair-2", "kind": TaskKindRepair, "advances": []string{"build"},
		"status": TaskDone, "progress": ProgressNoChange,
	})
	replayed := ReduceToSessionData([]RunEvent{
		{Type: "task_created", TaskID: "repair-1", Payload: first},
		{Type: "task_created", TaskID: "outcome-1", Payload: progress},
		{Type: "task_created", TaskID: "repair-2", Payload: second},
	})
	state := AntiThrashingState{}
	state.rebuild(replayed.Tasks, agent.ReliabilityConfig{MaxRepairsPerCriterion: 3, HardEnforcement: true})
	if got := state.RepairsByCriterion["build"]; got != 1 {
		t.Fatalf("replayed repair counter after progress = %d, want 1", got)
	}

	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 3, HardEnforcement: true}}},
		sessionData: NewSession(),
		acceptanceSpec: &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
			ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"},
		}}},
	}
	c.antiThrashing.RepairsByCriterion = map[string]int{"build": 2}
	progressItem := &TodoItem{ID: "outcome-live", Kind: TaskKindOutcome, Advances: []string{"build"}, Status: TaskDone, Progress: ProgressUnknown}
	c.reEvaluateAffectedCriteria(context.Background(), progressItem)
	if got := c.Metrics().RepairAttemptsByCriterion["build"]; got != 0 {
		t.Fatalf("live repair counter did not reset after progress: %d", got)
	}
}

func TestCriterionProgressClearsLiveAndReplayRepairBlock(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 1, HardEnforcement: true}}}
	acceptance := &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
		ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"},
	}}}
	live := &Coordinator{session: session, sessionData: NewSession(), acceptanceSpec: acceptance, taskTracker: NewTaskTracker()}
	item := live.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "repairer", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	live.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "completed")
	liveItem := live.taskTracker.TodoList().Items()[0]
	live.reEvaluateAffectedCriteria(context.Background(), liveItem)
	if liveItem.Progress != ProgressAdvanced || live.antiThrashingHardBlocked() {
		t.Fatalf("live progress left repair block: progress=%s blocked=%v", liveItem.Progress, live.antiThrashingHardBlocked())
	}
	if live.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, item) {
		t.Fatal("live repair remained blocked after criterion progress")
	}

	saved := NewSession()
	saved.Tasks = []*TodoItem{{ID: item.ID, Kind: TaskKindRepair, Advances: []string{"build"}, Status: TaskDone, Progress: ProgressAdvanced}}
	replayed := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(saved)
	if replayed.antiThrashingHardBlocked() {
		t.Fatal("replay retained repair block after criterion progress")
	}
	if replayed.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, saved.Tasks[0]) {
		t.Fatal("replayed repair remained blocked after criterion progress")
	}
}

func TestCriterionProgressClearsFingerprintBlockOnLiveAndReplay(t *testing.T) {
	fp := NewFailureFingerprint("build", "repairer", "verify", FailureVerify, "exit code 1")
	session := &TeamSession{Config: agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxSameFailureFingerprint: 1, HardEnforcement: true}}}
	acceptance := &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
		ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"},
	}}}
	live := &Coordinator{session: session, sessionData: NewSession(), acceptanceSpec: acceptance, taskTracker: NewTaskTracker()}
	item := live.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "repairer", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	item.FailureFingerprints = []FailureFingerprint{fp}
	live.antiThrashing.Counts = map[string]int{fp.Digest: 1}
	live.antiThrashing.markBlockedFingerprint(fp)
	liveItem := live.taskTracker.TodoList().Items()[0]
	liveItem.Status = TaskDone
	liveItem.Progress = ProgressUnknown
	live.reEvaluateAffectedCriteria(context.Background(), liveItem)
	if live.antiThrashingHardBlocked() || live.antiThrashing.Counts[fp.Digest] != 0 {
		t.Fatalf("live fingerprint block/counter survived progress: blocked=%v counts=%v", live.antiThrashingHardBlocked(), live.antiThrashing.Counts)
	}

	saved := NewSession()
	saved.Tasks = []*TodoItem{{ID: "repair", Kind: TaskKindRepair, Advances: []string{"build"}, Status: TaskDone, Progress: ProgressAdvanced, FailureFingerprints: []FailureFingerprint{fp}}}
	replayed := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(saved)
	if replayed.antiThrashingHardBlocked() || replayed.failureFingerprintCount(fp.Digest) != 0 {
		t.Fatalf("replay fingerprint block/counter survived progress: blocked=%v count=%d", replayed.antiThrashingHardBlocked(), replayed.failureFingerprintCount(fp.Digest))
	}
}

func TestPartialMultiCriterionProgressOnlyResetsAdvancedCriterionLiveAndReplay(t *testing.T) {
	config := agent.TeamConfig{Reliability: agent.ReliabilityConfig{MaxRepairsPerCriterion: 1, HardEnforcement: true}}
	acceptance := &AcceptanceSpec{Criteria: []AcceptanceCriterion{
		{ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
		{ID: "tests", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "false"}},
	}}
	live := &Coordinator{
		session:        &TeamSession{Config: config},
		sessionData:    NewSession(),
		acceptanceSpec: acceptance,
		taskTracker:    NewTaskTracker(),
	}
	fpBuild := NewFailureFingerprint("build", "repairer", "verify", FailureVerify, "build failed")
	fpTests := NewFailureFingerprint("tests", "repairer", "verify", FailureVerify, "tests failed")
	item := live.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "repairer", Desc: "repair both", Kind: TaskKindRepair, Advances: []string{"build", "tests"},
	}})[0]
	item.FailureFingerprints = []FailureFingerprint{fpBuild, fpTests}
	live.antiThrashing.Counts = map[string]int{fpBuild.Digest: 1, fpTests.Digest: 1}
	live.antiThrashing.RepairsByCriterion = map[string]int{"build": 2, "tests": 2}
	live.antiThrashing.markBlockedFingerprint(fpBuild)
	live.antiThrashing.markBlockedFingerprint(fpTests)
	live.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "completed")
	liveItem := live.taskTracker.TodoList().Items()[0]
	live.reEvaluateAffectedCriteria(context.Background(), liveItem)
	if liveItem.Progress != ProgressAdvanced || len(liveItem.ProgressCriteria) != 1 || liveItem.ProgressCriteria[0] != "build" {
		t.Fatalf("live progress subset = %v, want [build]", liveItem.ProgressCriteria)
	}
	if live.antiThrashing.RepairsByCriterion["build"] != 0 || live.antiThrashing.RepairsByCriterion["tests"] != 3 {
		t.Fatalf("live counters reset wrong criterion: %#v", live.antiThrashing.RepairsByCriterion)
	}
	if live.failureFingerprintCount(fpBuild.Digest) != 0 || live.failureFingerprintCount(fpTests.Digest) != 1 {
		t.Fatalf("live fingerprints reset wrong criterion: build=%d tests=%d", live.failureFingerprintCount(fpBuild.Digest), live.failureFingerprintCount(fpTests.Digest))
	}
	if live.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, liveItem) {
		t.Fatal("live advanced criterion remained blocked")
	}
	if !live.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"tests"}}, liveItem) {
		t.Fatal("live non-advanced criterion lost its block")
	}

	encode := func(value any) json.RawMessage {
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	taskPayload := encode(map[string]any{
		"id": "repair-both", "kind": TaskKindRepair, "advances": []string{"build", "tests"},
		"status": TaskDone, "failure_fingerprints": []FailureFingerprint{fpBuild, fpTests},
	})
	progressPayload := encode(map[string]any{
		"progress": ProgressAdvanced, "progress_criteria": []string{"build"},
		"after": []CriterionResult{{ID: "build", State: CriterionPassed}, {ID: "tests", State: CriterionFailed}},
	})
	replayedSession := ReduceToSessionData([]RunEvent{
		{Type: "task_created", TaskID: "repair-both", Payload: taskPayload},
		{Type: "criterion_re_evaluated", TaskID: "repair-both", Payload: progressPayload},
	})
	replayed := &Coordinator{session: &TeamSession{Config: config}, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(replayedSession)
	if got := replayedSession.Tasks[0].ProgressCriteria; len(got) != 1 || got[0] != "build" {
		t.Fatalf("replayed progress subset = %v, want [build]", got)
	}
	if replayed.antiThrashing.RepairsByCriterion["build"] != 0 || replayed.antiThrashing.RepairsByCriterion["tests"] != 3 {
		t.Fatalf("replayed counters reset wrong criterion: %#v", replayed.antiThrashing.RepairsByCriterion)
	}
	if replayed.failureFingerprintCount(fpBuild.Digest) != 0 || replayed.failureFingerprintCount(fpTests.Digest) != 1 {
		t.Fatalf("replayed fingerprints reset wrong criterion: build=%d tests=%d", replayed.failureFingerprintCount(fpBuild.Digest), replayed.failureFingerprintCount(fpTests.Digest))
	}
	if replayed.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"build"}}, replayedSession.Tasks[0]) {
		t.Fatal("replayed advanced criterion remained blocked")
	}
	if !replayed.antiThrashingBlocksTask(TaskDef{Kind: TaskKindRepair, Advances: []string{"tests"}}, replayedSession.Tasks[0]) {
		t.Fatal("replayed non-advanced criterion lost its block")
	}
}

func TestFailureOperationUsesTaskLocalIdentity(t *testing.T) {
	a := &TodoItem{ID: "a", Kind: TaskKindRepair, LastOperation: "bash"}
	b := &TodoItem{ID: "b", Kind: TaskKindRepair, LastOperation: "go-test"}
	if got := failureOperation(a); got != "bash" {
		t.Fatalf("operation a = %q", got)
	}
	if got := failureOperation(b); got != "go-test" {
		t.Fatalf("operation b = %q", got)
	}
}

func TestConcurrentTaskOperationsRemainTaskLocal(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "a", Desc: "a"}, {Agent: "b", Desc: "b"}})
	var wg sync.WaitGroup
	for _, pair := range []struct {
		id, operation string
	}{{items[0].ID, "bash"}, {items[1].ID, "go-test"}} {
		pair := pair
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				tracker.TodoList().SetLastOperation(pair.id, pair.operation)
			}
		}()
	}
	wg.Wait()
	got := tracker.TodoList().Items()
	if failureOperation(got[0]) != "bash" || failureOperation(got[1]) != "go-test" {
		t.Fatalf("concurrent task operation identity crossed: %#v", got)
	}
}

func TestWP09_DefaultAntiThrashingLimitsAppliedWhenYAMLUnset(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: default-reliability\nacceptance: 'true'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defaults := agent.DefaultReliabilityConfig()
	if cfg.Reliability.MaxSameFailureFingerprint != defaults.MaxSameFailureFingerprint {
		t.Fatalf("MaxSameFailureFingerprint = %d, want default %d", cfg.Reliability.MaxSameFailureFingerprint, defaults.MaxSameFailureFingerprint)
	}
	if cfg.Reliability.MaxDiagnosticTasksWithoutProgress != defaults.MaxDiagnosticTasksWithoutProgress {
		t.Fatalf("MaxDiagnosticTasksWithoutProgress = %d, want default %d", cfg.Reliability.MaxDiagnosticTasksWithoutProgress, defaults.MaxDiagnosticTasksWithoutProgress)
	}
	if cfg.Reliability.MaxRepairsPerCriterion != defaults.MaxRepairsPerCriterion {
		t.Fatalf("MaxRepairsPerCriterion = %d, want default %d", cfg.Reliability.MaxRepairsPerCriterion, defaults.MaxRepairsPerCriterion)
	}
	if !cfg.Reliability.HardEnforcement {
		t.Fatalf("HardEnforcement = false, want default true")
	}

	// Verify second failure of same digest is limited & hard blocked under defaults
	var state AntiThrashingState
	fp := NewFailureFingerprint("build", "worker", "bash", FailureVerify, "exit 1")
	first := &TodoItem{ID: "1", Kind: TaskKindRepair, Advances: []string{"build"}}
	second := &TodoItem{ID: "2", Kind: TaskKindRepair, Advances: []string{"build"}}
	_, _ = state.record(first, fp, RecoveryStrategyRetry, cfg.Reliability)
	repeated, limited := state.record(second, fp, RecoveryStrategyRetry, cfg.Reliability)
	if !repeated || !limited {
		t.Fatalf("second failure under defaults: repeated=%v limited=%v, want both true", repeated, limited)
	}
	if !state.HardBlocked {
		t.Fatal("defaults must hard block on second failure limit")
	}
}

func TestWP09_WarnOnlyOptInDoesNotHardBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: warn-only-team\nacceptance: 'true'\nreliability:\n  warn-only: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Reliability.WarnOnly || cfg.Reliability.HardEnforcement {
		t.Fatalf("unexpected reliability config for warn-only: %#v", cfg.Reliability)
	}

	var state AntiThrashingState
	fp := NewFailureFingerprint("build", "worker", "bash", FailureVerify, "exit 1")
	first := &TodoItem{ID: "1", Kind: TaskKindRepair, Advances: []string{"build"}}
	second := &TodoItem{ID: "2", Kind: TaskKindRepair, Advances: []string{"build"}}
	_, _ = state.record(first, fp, RecoveryStrategyRetry, cfg.Reliability)
	repeated, limited := state.record(second, fp, RecoveryStrategyRetry, cfg.Reliability)
	if !repeated || !limited {
		t.Fatalf("second failure under warn-only: repeated=%v limited=%v, want both true", repeated, limited)
	}
	if state.HardBlocked {
		t.Fatal("warn-only mode must NOT hard block")
	}
	if state.Warnings == 0 {
		t.Fatal("warn-only mode should record warnings")
	}
}

func TestWP09_MDOnlyTeamReceivesDefaultReliabilityConfig(t *testing.T) {
	dir := t.TempDir()
	// Create a team.yml with exploratory mode for MD-only testing
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: md-team\ngoal-mode: exploratory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mdFile := filepath.Join(dir, "worker.md")
	mdContent := "---\nname: worker\nrole: worker\n---\nYou are a worker."
	if err := os.WriteFile(mdFile, []byte(mdContent), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam failed for MD-only team: %v", err)
	}

	defaults := agent.DefaultReliabilityConfig()
	if sess.Config.Reliability.MaxSameFailureFingerprint != defaults.MaxSameFailureFingerprint {
		t.Fatalf("MD-only team MaxSameFailureFingerprint = %d, want %d", sess.Config.Reliability.MaxSameFailureFingerprint, defaults.MaxSameFailureFingerprint)
	}
	if !sess.Config.Reliability.HardEnforcement {
		t.Fatalf("MD-only team HardEnforcement = false, want true")
	}

	c := &Coordinator{
		session:     sess,
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	c.PersistFailure("worker", item.Desc, item.ID, "exit code 1")
	c.PersistFailure("worker", item.Desc, item.ID, "exit code 1")
	if !c.antiThrashingHardBlocked() {
		t.Fatal("MD-only team did not hard block on second identical failure")
	}
}

func TestWP09_LoadDefaultTeamReceivesDefaultReliabilityConfig(t *testing.T) {
	dir := t.TempDir()
	sess, err := LoadDefaultTeam(dir, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam failed: %v", err)
	}

	defaults := agent.DefaultReliabilityConfig()
	if sess.Config.Reliability.MaxSameFailureFingerprint != defaults.MaxSameFailureFingerprint {
		t.Fatalf("LoadDefaultTeam MaxSameFailureFingerprint = %d, want %d", sess.Config.Reliability.MaxSameFailureFingerprint, defaults.MaxSameFailureFingerprint)
	}
	if !sess.Config.Reliability.HardEnforcement {
		t.Fatalf("LoadDefaultTeam HardEnforcement = false, want true")
	}

	c := &Coordinator{
		session:     sess,
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "helper", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	c.PersistFailure("helper", item.Desc, item.ID, "exit code 1")
	c.PersistFailure("helper", item.Desc, item.ID, "exit code 1")
	if !c.antiThrashingHardBlocked() {
		t.Fatal("LoadDefaultTeam did not hard block on second identical failure")
	}
}

func TestWP09_ZeroValueSessionConfigPreservesHardEnforcementDefault(t *testing.T) {
	// Construct a TeamSession where Reliability is explicitly the zero struct (agent.ReliabilityConfig{})
	sess := &TeamSession{
		Config: agent.TeamConfig{
			Name:        "zero-config-team",
			Reliability: agent.ReliabilityConfig{}, // zero value struct
		},
	}

	c := &Coordinator{
		session:     sess,
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}

	rc := c.reliabilityConfig()
	defaults := agent.DefaultReliabilityConfig()
	if rc.MaxSameFailureFingerprint != defaults.MaxSameFailureFingerprint {
		t.Fatalf("reliabilityConfig().MaxSameFailureFingerprint = %d, want %d", rc.MaxSameFailureFingerprint, defaults.MaxSameFailureFingerprint)
	}
	if !rc.HardEnforcement {
		t.Fatalf("reliabilityConfig().HardEnforcement = false, want true")
	}

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repair", Kind: TaskKindRepair, Advances: []string{"build"}}})[0]
	c.PersistFailure("worker", item.Desc, item.ID, "exit code 1")
	c.PersistFailure("worker", item.Desc, item.ID, "exit code 1")
	if !c.antiThrashingHardBlocked() {
		t.Fatal("Coordinator with zero-value session config did not hard block on second identical failure")
	}
}
