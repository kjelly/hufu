package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func reviewerTimeoutFingerprint() FailureFingerprint {
	return NewFailureFingerprint("", "reviewer", "verify:task_result_assert", FailureTimeout, "source=task_timeout | error=context deadline exceeded")
}

func reviewerItem(id string, status TaskStatus, failed bool) *TodoItem {
	item := &TodoItem{ID: id, Agent: "reviewer", Status: status, VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}}
	if failed {
		item.FailureFingerprints = []FailureFingerprint{reviewerTimeoutFingerprint()}
	}
	return item
}

func defaultReliabilityLimits() ReliabilityConfig {
	return agent.DefaultReliabilityConfig()
}

func TestRebuildCountsOnlyTasksThatAreStillFailing(t *testing.T) {
	limits := defaultReliabilityLimits()
	digest := reviewerTimeoutFingerprint().Digest
	candidate := reviewerItem("9", TaskPending, false)
	candidateTask := TaskDef{Agent: "reviewer", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}}
	tests := []struct {
		name          string
		items         []*TodoItem
		wantCount     int
		wantEscalated bool
	}{
		{name: "recovered tasks do not count", items: []*TodoItem{
			reviewerItem("1", TaskDone, true), reviewerItem("2", TaskDone, true), reviewerItem("3", TaskDone, true),
		}},
		{name: "one failing task among recovered ones", items: []*TodoItem{
			reviewerItem("1", TaskDone, true), reviewerItem("2", TaskDone, true), reviewerItem("3", TaskError, true),
		}, wantCount: 1},
		{name: "three failing tasks still escalate", items: []*TodoItem{
			reviewerItem("1", TaskError, true), reviewerItem("2", TaskBlocked, true), reviewerItem("3", TaskError, true),
		}, wantCount: 3, wantEscalated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state AntiThrashingState
			state.rebuild(append(tt.items, candidate), limits)
			if got := state.Counts[digest]; got != tt.wantCount {
				t.Fatalf("fingerprint count = %d, want %d", got, tt.wantCount)
			}
			if escalated := state.SystemicEscalations > 0; escalated != tt.wantEscalated {
				t.Fatalf("systemic escalations = %d, want escalated %v", state.SystemicEscalations, tt.wantEscalated)
			}
			if blocked := state.blocksTask(candidateTask, candidate); blocked != tt.wantEscalated {
				t.Fatalf("sibling blocked = %v, want %v", blocked, tt.wantEscalated)
			}
		})
	}
}

// TestRecoveredFailuresReleaseTheirScope replays the shape of a real run:
// three reviewer workset items each time out once and succeed on retry. The
// escalation their first attempts caused must not keep blocking a sibling.
func TestRecoveredFailuresReleaseTheirScope(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Reliability: defaultReliabilityLimits()}}, taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "reviewer", Desc: "unit-0001", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}},
		{Agent: "reviewer", Desc: "unit-0005", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}},
		{Agent: "reviewer", Desc: "unit-0009", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}},
		{Agent: "reviewer", Desc: "unit-0012", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}},
	})
	for _, item := range items[:3] {
		item.Status = TaskError
		item.FailureFingerprints = []FailureFingerprint{reviewerTimeoutFingerprint()}
	}
	c.rebuildAntiThrashingState()
	sibling := TaskDef{Agent: "reviewer", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}}
	// The sibling carries no fingerprint of its own, so only the escalated
	// timeout scope blocks it, and a timeout scope asks for a replan.
	if blocked, disposition := c.antiThrashingDispatchBlock(sibling, items[3]); !blocked || disposition != ReplanRequired {
		t.Fatalf("while three tasks fail: blocked=%v disposition=%q, want a replan_required block", blocked, disposition)
	}
	for _, item := range items[:3] {
		item.Status = TaskDone
		c.forgetRecoveredFailures(item.ID)
	}
	if blocked, _ := c.antiThrashingDispatchBlock(sibling, items[3]); blocked {
		t.Fatal("the sibling is still blocked after every failed task recovered")
	}
	if metrics := c.Metrics(); metrics.SystemicFingerprintsEscalated != 0 {
		t.Fatalf("systemic escalations after recovery = %d, want 0", metrics.SystemicFingerprintsEscalated)
	}
}

func TestSystemicDispatchBlockUsesTheScopeDisposition(t *testing.T) {
	limits := defaultReliabilityLimits()
	limits.MaxSameFailureFingerprint = 0 // isolate the systemic scope
	candidate := reviewerItem("9", TaskPending, false)
	candidateTask := TaskDef{Agent: "reviewer", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}}
	for _, tt := range []struct {
		class TaskFailureClass
		want  RetryDisposition
	}{
		{class: FailureTimeout, want: ReplanRequired},
		{class: FailureExecution, want: ReplanRequired},
		{class: FailureProtocol, want: NeedsHuman},
		{class: FailureEnvironment, want: NeedsHuman},
	} {
		t.Run(string(tt.class), func(t *testing.T) {
			fp := NewFailureFingerprint("", "reviewer", "verify:task_result_assert", tt.class, "same failure")
			var items []*TodoItem
			for _, id := range []string{"1", "2", "3"} {
				items = append(items, &TodoItem{ID: id, Agent: "reviewer", Status: TaskError, VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}, FailureFingerprints: []FailureFingerprint{fp}})
			}
			var state AntiThrashingState
			state.rebuild(append(items, candidate), limits)
			blocked, disposition := state.dispatchBlock(candidateTask, candidate)
			if !blocked || disposition != tt.want {
				t.Fatalf("dispatch block = %v/%q, want %q", blocked, disposition, tt.want)
			}
			other := TaskDef{Agent: "critic"}
			if blocked, _ := state.dispatchBlock(other, &TodoItem{ID: "10", Agent: "critic"}); blocked {
				t.Fatal("a different agent's task was blocked by the reviewer scope")
			}
		})
	}
}

func TestSchedulerReplanBlockDoesNotEndTheRun(t *testing.T) {
	for _, tt := range []struct {
		name        string
		disposition RetryDisposition
		wantStatus  TaskStatus
		wantWrapUp  bool
		wantReason  string
	}{
		{name: "replan required", disposition: ReplanRequired, wantStatus: TaskError, wantReason: "replan_required"},
		{name: "needs human", disposition: NeedsHuman, wantStatus: TaskBlocked, wantWrapUp: true, wantReason: "human review required"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			coord := &Coordinator{
				session: &TeamSession{Config: agent.TeamConfig{Reliability: defaultReliabilityLimits()}}, taskTracker: NewTaskTracker(),
				reportStatus: func(StatusEvent) {}, sessionData: NewSession(), taskCache: newDefaultTaskCache(taskCacheDependencies{}), maxConcurrent: 1,
			}
			task := TaskDef{Agent: "reviewer", Goal: "review unit-0012", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert}}
			items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: task.Agent, Desc: task.Goal, VerifySpec: task.VerifySpec}})
			s := mustNewDAGScheduler(t, coord, []TaskDef{task}, items, nil)
			s.blockDispatch(0, tt.disposition)
			item := coord.taskTracker.TodoList().Items()[0]
			if item.Status != tt.wantStatus || s.states[0] != tt.wantStatus {
				t.Fatalf("status = %s (scheduler %s), want %s", item.Status, s.states[0], tt.wantStatus)
			}
			if item.FailureEvent == nil || item.FailureEvent.RetryDisposition != tt.disposition {
				t.Fatalf("failure event = %#v, want disposition %q", item.FailureEvent, tt.disposition)
			}
			if coord.IsWrapUp() != tt.wantWrapUp {
				t.Fatalf("wrap-up = %v, want %v", coord.IsWrapUp(), tt.wantWrapUp)
			}
			if s.results[0].err == nil || !strings.Contains(s.results[0].err.Error(), tt.wantReason) || s.results[0].todoID != item.ID {
				t.Fatalf("coordinator result = %#v, want the block reason", s.results[0])
			}
		})
	}
}

func TestCompletedTasksKeepCriterionBoundFailures(t *testing.T) {
	criterionFailure := NewFailureFingerprint("tests", "repairer", "verify", FailureVerify, "tests failed")
	taskFailure := reviewerTimeoutFingerprint()
	for _, tt := range []struct {
		name string
		item *TodoItem
		want []string
	}{
		{name: "criterion-free task recovered", item: &TodoItem{ID: "1", Status: TaskDone, FailureFingerprints: []FailureFingerprint{taskFailure}}},
		{name: "still failing", item: &TodoItem{ID: "2", Status: TaskError, FailureFingerprints: []FailureFingerprint{taskFailure}}, want: []string{taskFailure.Digest}},
		{name: "done task bound to a criterion", item: &TodoItem{ID: "3", Status: TaskDone, Advances: []string{"tests"}, FailureFingerprints: []FailureFingerprint{criterionFailure, taskFailure}}, want: []string{criterionFailure.Digest, taskFailure.Digest}},
		{name: "done task with criterion evidence only", item: &TodoItem{ID: "4", Status: TaskDone, FailureFingerprints: []FailureFingerprint{criterionFailure, taskFailure}}, want: []string{criterionFailure.Digest}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, fp := range countedFailureFingerprints(tt.item) {
				got = append(got, fp.Digest)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("counted fingerprints = %v, want %v", got, tt.want)
			}
		})
	}
}
