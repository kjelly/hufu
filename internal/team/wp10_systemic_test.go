package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestWP10_SystemicCount_AcrossDistinctTasks_EscalatesAtThreshold is the
// §11 acceptance scenario "同一 digest 跨 3 個不同任務失敗 → systemic 升級；
// 停止對該 scope 派工". Three distinct task IDs observe the same
// (component, operation, class, digest) failure (criterion differs). On the
// third task's failure the systemic scope escalates: HardBlocked becomes
// true, SystemicEscalations increments, and the contributing task's
// subsequent dispatch is blocked via blocksTask.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_AcrossDistinctTasks_EscalatesAtThreshold(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}

	// Same component/operation/class/digest (hence same criterion — the
	// digest includes the criterion, per NewFailureFingerprint), but three
	// DISTINCT task IDs. This is the §13-observed pattern: the same
	// protocol digest repeating across tasks.
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")

	tasks := []*TodoItem{
		{ID: "task-1", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "task-2", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "task-3", Kind: TaskKindOutcome, Advances: []string{"build"}},
	}

	// First two failures must NOT escalate.
	for i := 0; i < 2; i++ {
		_, _, systemic := state.record(tasks[i], fp, "", limits)
		if systemic {
			t.Fatalf("failure #%d must not escalate (threshold=3)", i+1)
		}
		if state.HardBlocked {
			t.Fatalf("HardBlocked must stay false before threshold (after #%d)", i+1)
		}
	}

	// Third distinct task trips the systemic escalation.
	_, _, systemic := state.record(tasks[2], fp, "", limits)
	if !systemic {
		t.Fatal("third distinct task failure must trip systemic escalation")
	}
	if !state.HardBlocked {
		t.Fatal("HardBlocked must be true after systemic escalation")
	}
	if state.SystemicEscalations != 1 {
		t.Fatalf("SystemicEscalations = %d, want 1", state.SystemicEscalations)
	}

	// The scope key must be blocked.
	scope := systemicScopeKey(fp)
	if !state.BlockedSystemicScopes[scope] {
		t.Fatal("systemic scope was not marked blocked")
	}

	// §6.2: "停止對該 scope 派工". A task whose todo item carries a
	// fingerprint in the escalated scope must be blocked by blocksTask.
	// Re-use task-3 (it now carries the fingerprint conceptually).
	task3WithFP := &TodoItem{ID: "task-3", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}}
	if !state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, task3WithFP) {
		t.Fatal("blocksTask must block dispatch to a task contributing to the escalated systemic scope")
	}

	// A task with a fingerprint in a DIFFERENT systemic scope must NOT be
	// blocked by this scope's escalation.
	otherFP := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 2")
	otherItem := &TodoItem{ID: "task-other", Kind: TaskKindOutcome, Advances: []string{"lint"}, FailureFingerprints: []FailureFingerprint{otherFP}}
	if state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"lint"}}, otherItem) {
		t.Fatal("blocksTask blocked a task in a non-escalated systemic scope")
	}
}

// TestWP10_SystemicCount_SameTaskRepeatedDoesNotEscalate verifies that
// repeated failures from the SAME task ID do not inflate the systemic
// distinct-task count. The systemic scope counts distinct tasks, not
// occurrences (§6.2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_SameTaskRepeatedDoesNotEscalate(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	sameTask := &TodoItem{ID: "task-only", Kind: TaskKindOutcome, Advances: []string{"build"}}

	for i := 0; i < 5; i++ {
		_, _, systemic := state.record(sameTask, fp, "", limits)
		if systemic {
			t.Fatalf("repeated failure from the same task ID escalated on iteration %d; systemic counts distinct tasks", i+1)
		}
	}
	if state.SystemicEscalations != 0 {
		t.Fatalf("SystemicEscalations = %d, want 0 (same task repeated)", state.SystemicEscalations)
	}
	scope := systemicScopeKey(fp)
	if got := state.systemicTaskCount(scope); got != 1 {
		t.Fatalf("systemicTaskCount = %d, want 1 (one distinct task)", got)
	}
}

// TestWP10_SystemicCount_WarnOnlyDoesNotHardBlock verifies that when
// HardEnforcement is off (warn-only), the systemic threshold is detected
// (SystemicEscalations counts it for observability) but the scope is not
// hard-blocked and dispatch is not blocked.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_WarnOnlyDoesNotHardBlock(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: false}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	tasks := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}},
	}
	for _, item := range tasks {
		state.record(item, fp, "", limits)
	}
	// Warn-only: no hard block, no BlockedSystemicScopes entry.
	if state.HardBlocked {
		t.Fatal("warn-only mode must NOT hard block on systemic threshold")
	}
	if len(state.BlockedSystemicScopes) != 0 {
		t.Fatalf("BlockedSystemicScopes = %v, want empty (warn-only)", state.BlockedSystemicScopes)
	}
}

// TestWP10_SystemicCount_DefaultThresholdIsThree verifies the default
// ReliabilityConfig applies MaxSystemicFailureTasks=3 when YAML is unset.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_DefaultThresholdIsThree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: systemic-default\nacceptance: 'true'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.MaxSystemicFailureTasks != 3 {
		t.Fatalf("default MaxSystemicFailureTasks = %d, want 3", cfg.Reliability.MaxSystemicFailureTasks)
	}
}

// TestWP10_SystemicCount_YAMLOverrideRespected verifies a YAML override of
// max-systemic-failure-tasks is parsed and applied.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_YAMLOverrideRespected(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: systemic-override\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 5\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.MaxSystemicFailureTasks != 5 {
		t.Fatalf("overridden MaxSystemicFailureTasks = %d, want 5", cfg.Reliability.MaxSystemicFailureTasks)
	}
}

// TestWP10_SystemicCount_ZeroThresholdDisablesFeature verifies that
// MaxSystemicFailureTasks=0 disables systemic counting entirely (no
// escalation even across many distinct tasks).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_ZeroThresholdDisablesFeature(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 0, HardEnforcement: true}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	for i := 0; i < 10; i++ {
		item := &TodoItem{ID: "t" + string(rune('a'+i)), Kind: TaskKindOutcome, Advances: []string{"build"}}
		_, _, systemic := state.record(item, fp, "", limits)
		if systemic {
			t.Fatalf("systemic escalated with MaxSystemicFailureTasks=0 (disabled) on task %d", i+1)
		}
	}
	if state.SystemicEscalations != 0 {
		t.Fatalf("SystemicEscalations = %d, want 0 (feature disabled)", state.SystemicEscalations)
	}
}

// TestWP10_SystemicCount_EscalationIsIrreversibleAcrossCriterionProgress
// verifies that a systemic escalation survives criterion progress. A
// systemic defect is a property of the system, not of a single criterion,
// so resetAfterCriterionProgress must not clear BlockedSystemicScopes
// (§6.2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_EscalationIsIrreversibleAcrossCriterionProgress(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	tasks := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	for _, item := range tasks {
		state.record(item, fp, "", limits)
	}
	if !state.BlockedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("precondition: scope not escalated")
	}
	// Simulate criterion progress on "build".
	state.resetAfterCriterionProgress([]string{"build"}, tasks)
	if !state.BlockedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("systemic escalation was cleared by criterion progress; systemic defects are irreversible for the run (§6.2)")
	}
	if !state.HardBlocked {
		t.Fatal("HardBlocked cleared after criterion progress despite live systemic block")
	}
}

// TestWP10_SystemicCount_RebuildRestoresEscalation verifies that
// AntiThrashingState.rebuild reconstructs the systemic counts and
// blocked scopes from persisted fingerprints across distinct task IDs,
// so a crash-resume cannot silently reset systemic enforcement.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_RebuildRestoresEscalation(t *testing.T) {
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	saved := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	state.rebuild(saved, limits)

	scope := systemicScopeKey(fp)
	if got := state.systemicTaskCount(scope); got != 3 {
		t.Fatalf("rebuilt systemicTaskCount = %d, want 3", got)
	}
	if !state.BlockedSystemicScopes[scope] {
		t.Fatal("rebuild did not restore blocked systemic scope")
	}
	if !state.HardBlocked {
		t.Fatal("rebuild did not restore HardBlocked after systemic escalation")
	}
	if state.SystemicEscalations != 1 {
		t.Fatalf("rebuilt SystemicEscalations = %d, want 1", state.SystemicEscalations)
	}

	// A task contributing to the escalated scope must be blocked after
	// rebuild (§6.2: "停止對該 scope 派工").
	if !state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, saved[0]) {
		t.Fatal("rebuilt state did not block dispatch to a task in the escalated systemic scope")
	}
}

// TestWP10_SystemicCount_RebuildBelowThresholdNoEscalation verifies
// rebuild does not escalate when the distinct-task count is below the
// threshold, but the count is still reconstructed.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_RebuildBelowThresholdNoEscalation(t *testing.T) {
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	saved := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"tests"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	state.rebuild(saved, limits)
	if state.systemicTaskCount(systemicScopeKey(fp)) != 2 {
		t.Fatalf("rebuilt count = %d, want 2", state.systemicTaskCount(systemicScopeKey(fp)))
	}
	if state.BlockedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("rebuild escalated with only 2 distinct tasks (threshold 3)")
	}
	if state.SystemicEscalations != 0 {
		t.Fatalf("SystemicEscalations = %d, want 0", state.SystemicEscalations)
	}
}

// TestWP10_SystemicCount_PersistFailureEmitsEventAndBlocks verifies the
// end-to-end PersistFailure path: three PersistFailure calls for distinct
// tasks with equivalent fingerprints trigger a systemic_escalation event,
// block the contributing task, and surface in RunMetrics.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_PersistFailureEmitsEventAndBlocks(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	es, err := NewEventStore(workspace, "run-wp10", "sess-wp10")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = es

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task B", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task C", Advances: []string{"build"}},
	})

	detail := "verification failed: exit code 1"
	// First two: no escalation.
	c.PersistFailure("worker", "task A", items[0].ID, detail)
	c.PersistFailure("worker", "task B", items[1].ID, detail)
	if c.Metrics().SystemicFingerprintsEscalated != 0 {
		t.Fatalf("after 2 failures: SystemicFingerprintsEscalated = %d, want 0", c.Metrics().SystemicFingerprintsEscalated)
	}
	// Third distinct task: escalation.
	c.PersistFailure("worker", "task C", items[2].ID, detail)
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 1 {
		t.Fatalf("after 3 failures: SystemicFingerprintsEscalated = %d, want 1", got)
	}
	if !c.antiThrashingHardBlocked() {
		t.Fatal("HardBlocked must be true after systemic escalation")
	}

	// The systemic_escalation event must be in the event store.
	events, _ := es.ReadEvents()
	foundSystemic := false
	for _, e := range events {
		if e.Type == "systemic_escalation" {
			foundSystemic = true
			var payload map[string]interface{}
			_ = json.Unmarshal(e.Payload, &payload)
			if disp, _ := payload["disposition"].(string); disp == "" {
				t.Error("systemic_escalation event missing disposition")
			}
		}
	}
	if !foundSystemic {
		t.Fatal("systemic_escalation event not emitted")
	}

	// The third task must be blocked (status TaskBlocked) because the
	// systemic escalation triggers hard enforcement.
	blocked := false
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == items[2].ID && item.Status == TaskBlocked {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("third contributing task was not marked TaskBlocked after systemic escalation")
	}
}

// TestWP10_SystemicCount_DispositionByClass is a table-driven test for
// SystemicDispositionForClass covering the §6.2 escalation matrix:
// protocol / environment / contract → needs_human; any other →
// replan_required.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_DispositionByClass(t *testing.T) {
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
	for _, tc := range tests {
		if got := SystemicDispositionForClass(tc.class); got != tc.want {
			t.Errorf("SystemicDispositionForClass(%q) = %q, want %q", tc.class, got, tc.want)
		}
	}
}

// TestWP10_SystemicCount_CancelledExcluded verifies that cancelled
// failures do not contribute to systemic counts. PersistFailure skips
// the record() call for cancelled failures (§5.3), so they cannot trip
// the systemic threshold.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, §6.2, WP-10
func TestWP10_SystemicCount_CancelledExcluded(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10-cancel"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	es, err := NewEventStore(workspace, "run-wp10-cancel", "sess-wp10-cancel")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = es

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A"},
		{Agent: "worker", Desc: "task B"},
		{Agent: "worker", Desc: "task C"},
	})
	// Three cancelled failures across distinct tasks must not escalate.
	for _, item := range items {
		c.PersistFailure("worker", item.Desc, item.ID, c.FailureDetail(context.Canceled, FailureSourceSigint))
	}
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 0 {
		t.Fatalf("cancelled failures escalated systemic: %d, want 0 (§5.3 excludes cancelled)", got)
	}
	if c.antiThrashingHardBlocked() {
		t.Fatal("cancelled failures caused HardBlocked; cancelled must be excluded from anti-thrashing (§5.3)")
	}
}

// TestWP10_SystemicCount_LiveMatchesReplay is the live-vs-replay parity
// test. Three PersistFailure calls live must produce the same
// SystemicEscalations and BlockedSystemicScopes as rebuilding from the
// persisted session.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_LiveMatchesReplay(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10-parity", Reliability: agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}}}
	live := &Coordinator{session: session, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	live.taskTracker.TodoList().onChange = func() { live.saveCheckpoint() }

	items := live.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task B", Advances: []string{"build"}},
		{Agent: "worker", Desc: "task C", Advances: []string{"build"}},
	})
	detail := "verification failed: exit code 1"
	for _, item := range items {
		live.PersistFailure("worker", item.Desc, item.ID, detail)
	}
	if !live.antiThrashingHardBlocked() {
		t.Fatal("live did not hard block after 3 systemic failures")
	}
	if len(live.antiThrashing.BlockedSystemicScopes) == 0 {
		t.Fatal("live did not block any systemic scope")
	}
	// Capture the live blocked scope key for replay comparison.
	liveScopes := make([]string, 0, len(live.antiThrashing.BlockedSystemicScopes))
	for k := range live.antiThrashing.BlockedSystemicScopes {
		liveScopes = append(liveScopes, k)
	}

	// Rebuild from the persisted session and compare.
	replayed := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	replayed.SetSessionData(LoadSession(workspace))
	if got := replayed.Metrics().SystemicFingerprintsEscalated; got != live.Metrics().SystemicFingerprintsEscalated {
		t.Fatalf("replay SystemicFingerprintsEscalated = %d, live = %d", got, live.Metrics().SystemicFingerprintsEscalated)
	}
	if replayed.antiThrashingHardBlocked() != live.antiThrashingHardBlocked() {
		t.Fatalf("replay HardBlocked = %v, live = %v", replayed.antiThrashingHardBlocked(), live.antiThrashingHardBlocked())
	}
	// The rebuilt state must block the same systemic scope(s) as live.
	for _, scope := range liveScopes {
		if !replayed.antiThrashing.BlockedSystemicScopes[scope] {
			t.Errorf("replay did not block systemic scope %q that live blocked", scope)
		}
	}
}

// TestWP10_SystemicCount_WarnOnlyFourthFailureNoReEmit is the reviewer P1
// regression: in warn-only mode, the fourth distinct-task failure in an
// already-escalated scope must NOT re-increment SystemicEscalations or
// re-emit. The escalation is one-time per scope.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_WarnOnlyFourthFailureNoReEmit(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: false}
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	tasks := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t4", Kind: TaskKindOutcome, Advances: []string{"build"}},
		{ID: "t5", Kind: TaskKindOutcome, Advances: []string{"build"}},
	}
	var escalations int
	for _, item := range tasks {
		if _, _, systemic := state.record(item, fp, "", limits); systemic {
			escalations++
		}
	}
	if escalations != 1 {
		t.Fatalf("warn-only re-escalated %d times across 5 failures, want exactly 1 (one-time per scope)", escalations)
	}
	if state.SystemicEscalations != 1 {
		t.Fatalf("SystemicEscalations = %d, want 1 (one-time)", state.SystemicEscalations)
	}
	if state.HardBlocked {
		t.Fatal("warn-only must not hard-block")
	}
	if len(state.BlockedSystemicScopes) != 0 {
		t.Fatalf("warn-only BlockedSystemicScopes = %v, want empty", state.BlockedSystemicScopes)
	}
	// EscalatedSystemicScopes must record the one-time marker.
	if !state.EscalatedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("EscalatedSystemicScopes did not record the escalated scope (one-time marker missing)")
	}
}

// TestWP10_SystemicCount_WarnOnlyReplayParity verifies that rebuild under
// warn-only mode reconstructs the same SystemicEscalations count as the
// live run (reviewer P1: rebuild was guarded by HardEnforcement and
// reported 0 in warn-only).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_WarnOnlyReplayParity(t *testing.T) {
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	saved := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	// Live warn-only run.
	var live AntiThrashingState
	liveLimits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: false}
	for _, item := range saved {
		live.record(item, fp, "", liveLimits)
	}
	// Rebuild warn-only from the same persisted evidence.
	var replayed AntiThrashingState
	replayed.rebuild(saved, liveLimits)
	if replayed.SystemicEscalations != live.SystemicEscalations {
		t.Fatalf("warn-only replay SystemicEscalations = %d, live = %d (must match)", replayed.SystemicEscalations, live.SystemicEscalations)
	}
	if !replayed.EscalatedSystemicScopes[systemicScopeKey(fp)] {
		t.Fatal("warn-only replay did not restore EscalatedSystemicScopes marker")
	}
	if replayed.HardBlocked {
		t.Fatal("warn-only replay must not hard-block")
	}
	if len(replayed.BlockedSystemicScopes) != 0 {
		t.Fatalf("warn-only replay BlockedSystemicScopes = %v, want empty", replayed.BlockedSystemicScopes)
	}
}

// TestWP10_SystemicCount_ExplicitYAMLZeroDisablesViaCoordinator is the
// reviewer P2 regression: an explicit `max-systemic-failure-tasks: 0` in
// team YAML must override the default (3) and disable systemic counting
// end-to-end through reliabilityConfig() (not just parseTeamYML).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_ExplicitYAMLZeroDisablesViaCoordinator(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: systemic-zero\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 0\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.MaxSystemicFailureTasks != 0 {
		t.Fatalf("parseTeamYML MaxSystemicFailureTasks = %d, want 0 (explicit zero)", cfg.Reliability.MaxSystemicFailureTasks)
	}
	if !cfg.Reliability.MaxSystemicFailureTasksSet {
		t.Fatal("parseTeamYML did not set MaxSystemicFailureTasksSet=true for explicit zero")
	}

	// The Coordinator's reliabilityConfig() must honor the explicit zero,
	// NOT restore the default (3).
	c := &Coordinator{
		session: &TeamSession{Config: cfg},
	}
	rc := c.reliabilityConfig()
	if rc.MaxSystemicFailureTasks != 0 {
		t.Fatalf("reliabilityConfig() MaxSystemicFailureTasks = %d, want 0 (explicit zero must override default via Coordinator)", rc.MaxSystemicFailureTasks)
	}

	// End-to-end: three systemic failures must NOT escalate when the
	// feature is disabled by explicit zero.
	c2 := &Coordinator{
		session:     &TeamSession{Config: cfg},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	items := c2.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}},
	})
	detail := "verification failed: exit code 1"
	for _, item := range items {
		c2.PersistFailure("worker", item.Desc, item.ID, detail)
	}
	if got := c2.Metrics().SystemicFingerprintsEscalated; got != 0 {
		t.Fatalf("SystemicFingerprintsEscalated = %d, want 0 (explicit YAML zero disabled the feature)", got)
	}
	// Note: HardBlocked may still be true via the independent
	// MaxSameFailureFingerprint limit (default 2), which is a separate
	// anti-thrashing layer. The systemic-specific check is the absence of
	// any BlockedSystemicScopes entry and zero systemic escalations.
	if len(c2.antiThrashing.BlockedSystemicScopes) != 0 {
		t.Fatalf("BlockedSystemicScopes = %v, want empty (systemic feature disabled by explicit zero)", c2.antiThrashing.BlockedSystemicScopes)
	}
}

// TestWP10_SystemicCount_ExplicitYAMLZeroReplayStable verifies that after
// crash-resume, an explicit YAML zero still disables the feature (the
// MaxSystemicFailureTasksSet marker survives because it is part of the
// parsed TeamConfig used by the replayed Coordinator).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_ExplicitYAMLZeroReplayStable(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: systemic-zero-replay\nacceptance: 'true'\nreliability:\n  max-systemic-failure-tasks: 0\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate persisted tasks that would have escalated under the default.
	fp := NewFailureFingerprint("build", "worker", "go test", FailureVerify, "exit code 1")
	saved := []*TodoItem{
		{ID: "t1", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t2", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
		{ID: "t3", Kind: TaskKindOutcome, Advances: []string{"build"}, FailureFingerprints: []FailureFingerprint{fp}},
	}
	c := &Coordinator{session: &TeamSession{Config: cfg}, taskTracker: NewTaskTracker()}
	c.SetSessionData(&SessionData{Tasks: saved})
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 0 {
		t.Fatalf("replay with explicit YAML zero escalated %d, want 0 (feature disabled)", got)
	}
	if len(c.antiThrashing.BlockedSystemicScopes) != 0 {
		t.Fatalf("replay BlockedSystemicScopes = %v, want empty (systemic feature disabled by explicit zero)", c.antiThrashing.BlockedSystemicScopes)
	}
}

// TestWP10_SystemicCount_PersistFailureFourthFailureNoReEmit verifies the
// end-to-end PersistFailure path does not re-emit the systemic_escalation
// event for a fourth distinct-task failure in an already-escalated scope
// (reviewer P1: live re-emission).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_PersistFailureFourthFailureNoReEmit(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10-no-reeemit"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	es, err := NewEventStore(workspace, "run-wp10-ne", "sess-wp10-ne")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = es

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}},
		{Agent: "worker", Desc: "d", Advances: []string{"build"}},
	})
	detail := "verification failed: exit code 1"
	for _, item := range items {
		c.PersistFailure("worker", item.Desc, item.ID, detail)
	}
	// Exactly one systemic_escalation event must have been emitted.
	events, _ := es.ReadEvents()
	count := 0
	for _, e := range events {
		if e.Type == "systemic_escalation" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("systemic_escalation events = %d, want exactly 1 (one-time per scope)", count)
	}
	if got := c.Metrics().SystemicFingerprintsEscalated; got != 1 {
		t.Fatalf("SystemicFingerprintsEscalated after 4 failures = %d, want 1 (one-time)", got)
	}
}

// TestWP10_SystemicCount_BlocksFutureUnFingerprintedTaskViaPersistFailure
// is the scheduler-level regression requested by the reviewer: after a
// systemic escalation via PersistFailure, a post-escalation candidate
// task (not yet failed) with matching (component, operation) must be
// blocked by antiThrashingBlocksTask.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_BlocksFutureUnFingerprintedTaskViaPersistFailure(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10-sched"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	// Three tasks fail identically (same agent, same Kind-derived
	// operation "task:outcome"). The TodoSpec sets Kind explicitly to
	// match the coordinator's TodoSpec construction path.
	failed := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}, Kind: TaskKindOutcome},
	})
	detail := "verification failed: exit code 1"
	for _, item := range failed {
		c.PersistFailure("worker", item.Desc, item.ID, detail)
	}
	if !c.antiThrashingHardBlocked() {
		t.Fatal("precondition: systemic escalation did not fire")
	}

	// A future candidate task: same agent, same Kind, no failure
	// fingerprints (never run). Its derived operation is "task:outcome"
	// (matches the escalated scope's operation). It must be blocked.
	future := &TodoItem{ID: "future", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if !c.antiThrashingBlocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, future) {
		t.Fatal("post-escalation un-fingerprinted future task with matching (component, operation) was NOT blocked by antiThrashingBlocksTask (§6.2 scheduler enforcement)")
	}
}
