package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// A systemic scope is the (component, operation, class, digest) of a failure.
// Once MaxSystemicFailureTasks distinct tasks observe the same scope it is
// escalated exactly once; under hard enforcement dispatch into that scope
// stops for the rest of the run, and criterion progress does not lift it.

func systemicLimits(threshold int, hard bool) agent.ReliabilityConfig {
	return agent.ReliabilityConfig{MaxSystemicFailureTasks: threshold, HardEnforcement: hard}
}

// systemicFingerprint is the failure every systemic test records; detail
// changes the digest and therefore the scope.
func systemicFingerprint(detail string) FailureFingerprint {
	return NewFailureFingerprint("build", "worker", "go test", FailureVerify, detail)
}

// systemicItems builds outcome tasks advancing "build" with the given IDs.
func systemicItems(ids ...string) []*TodoItem {
	items := make([]*TodoItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, &TodoItem{ID: id, Kind: TaskKindOutcome, Advances: []string{"build"}})
	}
	return items
}

// systemicFingerprinted builds persisted tasks that already carry fp.
func systemicFingerprinted(fp FailureFingerprint, ids ...string) []*TodoItem {
	items := systemicItems(ids...)
	for _, item := range items {
		item.FailureFingerprints = []FailureFingerprint{fp}
	}
	return items
}

// systemicCoordinator is a coordinator whose PersistFailure path checkpoints
// and emits events, so escalation can be observed end to end.
func systemicCoordinator(t *testing.T, edit func(*agent.TeamConfig)) (*Coordinator, *EventStore) {
	t.Helper()
	var opts []coordinatorOption
	if edit != nil {
		opts = append(opts, withTeamConfig(edit))
	}
	c := newTestCoordinator(t, opts...)
	c.sessionData = NewSession()
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "session-test")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = store // closed by the coordinator's cleanup
	return c, store
}

// systemicPersist records the same verification failure for every item.
func systemicPersist(c *Coordinator, items []*TodoItem) {
	for _, item := range items {
		c.PersistFailure("worker", item.Desc, item.ID, "verification failed: exit code 1")
	}
}

func systemicEscalationEvents(t *testing.T, store *EventStore) int {
	t.Helper()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range events {
		if e.Type != "systemic_escalation" {
			continue
		}
		count++
		var payload map[string]any
		_ = json.Unmarshal(e.Payload, &payload)
		if disposition, _ := payload["disposition"].(string); disposition == "" {
			t.Error("systemic_escalation event missing disposition")
		}
	}
	return count
}

func TestSystemicScopeEscalation(t *testing.T) {
	fp := systemicFingerprint("exit code 1")
	tests := []struct {
		name    string
		limits  agent.ReliabilityConfig
		taskIDs []string
		// wantEscalateOn is the 1-based record call that escalates; 0 never.
		wantEscalateOn  int
		wantHardBlocked bool
		wantTaskCount   int
	}{
		{"three distinct tasks escalate under hard enforcement", systemicLimits(3, true), []string{"t1", "t2", "t3"}, 3, true, 3},
		{"the same task repeated counts once", systemicLimits(3, true), []string{"only", "only", "only", "only", "only"}, 0, false, 1},
		{"warn-only escalates once and never hard-blocks", systemicLimits(3, false), []string{"t1", "t2", "t3", "t4", "t5"}, 3, false, 5},
		{"zero threshold disables counting", systemicLimits(0, true), []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state AntiThrashingState
			for i, item := range systemicItems(tt.taskIDs...) {
				_, _, systemic := state.record(item, fp, "", tt.limits)
				if systemic != (i+1 == tt.wantEscalateOn) {
					t.Fatalf("record #%d systemic = %v, want escalation only on call %d", i+1, systemic, tt.wantEscalateOn)
				}
				if state.HardBlocked && i+1 < tt.wantEscalateOn {
					t.Fatalf("HardBlocked after record #%d, before the threshold", i+1)
				}
			}
			scope := systemicScopeKey(fp)
			wantEscalations := 0
			if tt.wantEscalateOn > 0 {
				wantEscalations = 1
			}
			if state.SystemicEscalations != wantEscalations || state.EscalatedSystemicScopes[scope] != (wantEscalations == 1) {
				t.Errorf("escalations = %d (marker %v), want %d", state.SystemicEscalations, state.EscalatedSystemicScopes[scope], wantEscalations)
			}
			if state.HardBlocked != tt.wantHardBlocked || state.BlockedSystemicScopes[scope] != tt.wantHardBlocked {
				t.Errorf("hard blocked = %v, scope blocked = %v, want %v", state.HardBlocked, state.BlockedSystemicScopes[scope], tt.wantHardBlocked)
			}
			if got := state.systemicTaskCount(scope); got != tt.wantTaskCount {
				t.Errorf("systemicTaskCount = %d, want %d", got, tt.wantTaskCount)
			}
		})
	}
}

func TestSystemicScopeBlocksOnlyTasksInEscalatedScope(t *testing.T) {
	fp := systemicFingerprint("exit code 1")
	var state AntiThrashingState
	for _, item := range systemicItems("t1", "t2", "t3") {
		state.record(item, fp, "", systemicLimits(3, true))
	}
	if !state.HardBlocked {
		t.Fatal("precondition: scope not escalated")
	}
	contributing := systemicFingerprinted(fp, "t3")[0]
	if !state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, contributing) {
		t.Error("task carrying the escalated fingerprint was not blocked")
	}
	other := systemicFingerprinted(systemicFingerprint("exit code 2"), "task-other")[0]
	other.Advances = []string{"lint"}
	if state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"lint"}}, other) {
		t.Error("task whose fingerprint is in another scope was blocked")
	}
}

func TestSystemicScopeBlocksFreshCandidateByStableOperation(t *testing.T) {
	// Executed tasks record their last tool call as LastOperation, but the
	// systemic fingerprint uses the stable Kind-derived operation so a fresh
	// candidate that never ran derives the same (component, operation).
	var state AntiThrashingState
	for _, item := range systemicItems("a", "b", "c") {
		item.LastOperation = "bash"
		if stableOperation(item) != "task:outcome" || failureOperation(item) != "bash" {
			t.Fatalf("operations = stable %q / failure %q, want task:outcome / bash", stableOperation(item), failureOperation(item))
		}
		fp := NewFailureFingerprint("build", "worker", stableOperation(item), FailureVerify, "exit code 1")
		state.record(item, fp, "", systemicLimits(3, true))
	}
	if !state.HardBlocked {
		t.Fatal("precondition: scope not escalated")
	}
	fresh := &TodoItem{ID: "fresh", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if failureOperation(fresh) != "task:outcome" {
		t.Fatalf("fresh candidate operation = %q, want task:outcome", failureOperation(fresh))
	}
	tests := []struct {
		name  string
		agent string
		kind  TaskKind
		want  bool
	}{
		{"fresh candidate in the escalated scope", "worker", TaskKindOutcome, true},
		{"different component", "reviewer", TaskKindOutcome, false},
		{"different kind", "worker", TaskKindRepair, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := &TodoItem{ID: "candidate", Agent: tt.agent, Kind: tt.kind, Advances: []string{"build"}}
			if got := state.blocksTask(TaskDef{Agent: tt.agent, Kind: tt.kind, Advances: []string{"build"}}, candidate); got != tt.want {
				t.Errorf("blocksTask = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSystemicEscalationSurvivesCriterionProgress(t *testing.T) {
	fp := systemicFingerprint("exit code 1")
	items := systemicFingerprinted(fp, "t1", "t2", "t3")
	var state AntiThrashingState
	for _, item := range items {
		state.record(item, fp, "", systemicLimits(3, true))
	}
	if !state.BlockedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("precondition: scope not escalated")
	}
	state.resetAfterCriterionProgress([]string{"build"}, items)
	if !state.BlockedSystemicScopes[systemicScopeKey(fp)] || !state.HardBlocked {
		t.Fatal("criterion progress cleared a systemic escalation; a systemic defect is a property of the run")
	}
}

func TestSystemicScopeRebuildMatchesLiveRecording(t *testing.T) {
	fp := systemicFingerprint("exit code 1")
	belowThreshold := systemicFingerprinted(fp, "t1", "t2")
	belowThreshold[1].Advances = []string{"tests"}
	tests := []struct {
		name            string
		limits          agent.ReliabilityConfig
		items           []*TodoItem
		wantCount       int
		wantEscalations int
		wantBlocked     bool
	}{
		{"three persisted tasks restore the hard block", systemicLimits(3, true), systemicFingerprinted(fp, "t1", "t2", "t3"), 3, 1, true},
		{"below threshold restores the count only", systemicLimits(3, true), belowThreshold, 2, 0, false},
		{"warn-only restores the escalation marker without blocking", systemicLimits(3, false), systemicFingerprinted(fp, "t1", "t2", "t3"), 3, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := systemicScopeKey(fp)
			var live, rebuilt AntiThrashingState
			for _, item := range tt.items {
				live.record(item, fp, "", tt.limits)
			}
			rebuilt.rebuild(tt.items, tt.limits)
			if rebuilt.SystemicEscalations != live.SystemicEscalations || rebuilt.HardBlocked != live.HardBlocked ||
				rebuilt.BlockedSystemicScopes[scope] != live.BlockedSystemicScopes[scope] || rebuilt.EscalatedSystemicScopes[scope] != live.EscalatedSystemicScopes[scope] {
				t.Fatalf("rebuilt state %+v does not match live state %+v", rebuilt, live)
			}
			if got := rebuilt.systemicTaskCount(scope); got != tt.wantCount {
				t.Errorf("rebuilt systemicTaskCount = %d, want %d", got, tt.wantCount)
			}
			if rebuilt.SystemicEscalations != tt.wantEscalations || rebuilt.EscalatedSystemicScopes[scope] != (tt.wantEscalations == 1) {
				t.Errorf("rebuilt escalations = %d, want %d", rebuilt.SystemicEscalations, tt.wantEscalations)
			}
			if rebuilt.HardBlocked != tt.wantBlocked || rebuilt.BlockedSystemicScopes[scope] != tt.wantBlocked {
				t.Errorf("rebuilt hard blocked = %v, want %v", rebuilt.HardBlocked, tt.wantBlocked)
			}
			if tt.wantBlocked && !rebuilt.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, tt.items[0]) {
				t.Error("rebuilt state did not block dispatch into the escalated scope")
			}
		})
	}
}

func TestSystemicThresholdConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    int
		wantSet bool
	}{
		{"default is three", "name: systemic\nacceptance: 'true'\n", 3, false},
		{"yaml override", "name: systemic\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 5\n", 5, true},
		{"explicit zero disables", "name: systemic\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 0\n", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Reliability.MaxSystemicFailureTasks != tt.want || cfg.Reliability.MaxSystemicFailureTasksSet != tt.wantSet {
				t.Fatalf("parsed MaxSystemicFailureTasks = %d (set %v), want %d (set %v)", cfg.Reliability.MaxSystemicFailureTasks, cfg.Reliability.MaxSystemicFailureTasksSet, tt.want, tt.wantSet)
			}
			// The coordinator must honor an explicit zero instead of
			// restoring the default.
			c := &Coordinator{session: &TeamSession{Config: cfg}}
			if got := c.reliabilityConfig().MaxSystemicFailureTasks; got != tt.want {
				t.Fatalf("reliabilityConfig().MaxSystemicFailureTasks = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSystemicThresholdExplicitZeroDisablesLiveAndReplay(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: systemic-zero\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 0\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	live, _ := systemicCoordinator(t, func(c *agent.TeamConfig) { *c = cfg })
	items := live.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}},
	})
	systemicPersist(live, items)
	// HardBlocked may still trip through the separate same-fingerprint
	// limit; the systemic feature itself must stay silent.
	if got := live.Metrics().SystemicFingerprintsEscalated; got != 0 || len(live.antiThrashing.BlockedSystemicScopes) != 0 {
		t.Fatalf("live escalations = %d, blocked scopes = %v, want none", got, live.antiThrashing.BlockedSystemicScopes)
	}

	// After crash-resume the explicit zero still disables the feature.
	replayed := &Coordinator{session: &TeamSession{Config: cfg}, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(&SessionData{Tasks: systemicFingerprinted(systemicFingerprint("exit code 1"), "t1", "t2", "t3")})
	if got := replayed.Metrics().SystemicFingerprintsEscalated; got != 0 || len(replayed.antiThrashing.BlockedSystemicScopes) != 0 {
		t.Fatalf("replay escalations = %d, blocked scopes = %v, want none", got, replayed.antiThrashing.BlockedSystemicScopes)
	}
}

func TestSystemicEscalationPersistsOnceAcrossDistinctTasks(t *testing.T) {
	c, store := systemicCoordinator(t, nil)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task B", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task C", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task D", Advances: []string{"build"}},
	})

	systemicPersist(c, items[:2])
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 0 {
		t.Fatalf("escalations after two failures = %d, want 0", got)
	}
	systemicPersist(c, items[2:3])
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 1 || !c.antiThrashingHardBlocked() {
		t.Fatalf("escalations after three failures = %d (hard blocked %v), want 1 and hard blocked", got, c.antiThrashingHardBlocked())
	}
	if got := todoByID(t, c, items[2].ID).Status; got != TaskBlocked {
		t.Errorf("third contributing task status = %s, want blocked", got)
	}
	// A fourth failure in the escalated scope neither re-counts nor
	// re-emits the escalation.
	systemicPersist(c, items[3:])
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 1 {
		t.Errorf("escalations after four failures = %d, want 1", got)
	}
	if got := systemicEscalationEvents(t, store); got != 1 {
		t.Errorf("systemic_escalation events = %d, want exactly 1", got)
	}
}

func TestSystemicEscalationExcludesCancelledFailures(t *testing.T) {
	c, _ := systemicCoordinator(t, nil)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A"},
		{Agent: "worker", Desc: "task B"},
		{Agent: "worker", Desc: "task C"},
	})
	for _, item := range items {
		c.PersistFailure("worker", item.Desc, item.ID, c.FailureDetail(context.Canceled, FailureSourceSigint))
	}
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 0 || c.antiThrashingHardBlocked() {
		t.Fatalf("cancelled failures escalated %d times (hard blocked %v), want none", got, c.antiThrashingHardBlocked())
	}
}

func TestSystemicEscalationLiveMatchesReplay(t *testing.T) {
	live, _ := systemicCoordinator(t, func(cfg *agent.TeamConfig) {
		cfg.Reliability = systemicLimits(3, true)
	})
	items := live.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task B", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task C", Advances: []string{"build"}},
	})
	systemicPersist(live, items)
	if !live.antiThrashingHardBlocked() || len(live.antiThrashing.BlockedSystemicScopes) == 0 {
		t.Fatal("live run did not hard block a systemic scope")
	}

	replayed := &Coordinator{session: live.session, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(LoadSession(live.session.Workspace))
	if got, want := replayed.Metrics().SystemicFingerprintsEscalated, live.Metrics().SystemicFingerprintsEscalated; got != want {
		t.Fatalf("replay escalations = %d, live = %d", got, want)
	}
	if replayed.antiThrashingHardBlocked() != live.antiThrashingHardBlocked() {
		t.Fatalf("replay hard blocked = %v, live = %v", replayed.antiThrashingHardBlocked(), live.antiThrashingHardBlocked())
	}
	for scope := range live.antiThrashing.BlockedSystemicScopes {
		if !replayed.antiThrashing.BlockedSystemicScopes[scope] {
			t.Errorf("replay did not block systemic scope %q that live blocked", scope)
		}
	}
}

func TestSystemicEscalationBlocksFreshCandidateViaPersistFailure(t *testing.T) {
	c, _ := systemicCoordinator(t, nil)
	failed := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}, Kind: TaskKindOutcome},
	})
	// Each task executed, so its last tool call is recorded; the systemic
	// fingerprint must still use the stable operation.
	for _, item := range failed {
		c.taskTracker.TodoList().SetLastOperation(item.ID, "bash")
	}
	systemicPersist(c, failed)
	if !c.antiThrashingHardBlocked() {
		t.Fatal("precondition: systemic escalation did not fire")
	}
	fresh := &TodoItem{ID: "fresh", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if !c.antiThrashingBlocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, fresh) {
		t.Fatal("fresh candidate in the escalated scope was not blocked at dispatch")
	}
}

func TestSystemicDispositionForClass(t *testing.T) {
	tests := []struct {
		class TaskFailureClass
		want  string
	}{
		{FailureProtocol, "needs_human"},
		{FailureEnvironment, "needs_human"},
		{FailureContract, "needs_human"},
		{FailureExecution, "replan_required"},
		{FailureVerify, "replan_required"},
		{FailureTimeout, "replan_required"},
		{FailurePolicy, "replan_required"},
	}
	for _, tt := range tests {
		if got := SystemicDispositionForClass(tt.class); got != tt.want {
			t.Errorf("SystemicDispositionForClass(%q) = %q, want %q", tt.class, got, tt.want)
		}
	}
}
