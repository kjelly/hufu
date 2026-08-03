package team

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
)

func TestPlanRevisionValidatorRejectsUnsafePlans(t *testing.T) {
	fp := "acceptance-fingerprint"
	tests := []struct {
		name   string
		mutate func(*PlanRevision)
		want   string
	}{
		{name: "cycle", mutate: func(r *PlanRevision) { r.TaskDAG[0].DependsOn = []int{1}; r.TaskDAG[1].DependsOn = []int{0} }, want: "cycle"},
		{name: "missing dependency", mutate: func(r *PlanRevision) { r.TaskDAG[0].DependsOn = []int{9} }, want: "invalid dependency"},
		{name: "acceptance weakening", mutate: func(r *PlanRevision) { r.AcceptanceFingerprint = "changed" }, want: "acceptance fingerprint"},
		{name: "high risk without verify", mutate: func(r *PlanRevision) { r.TaskDAG[0].SideEffect = SideEffectExternalWrite }, want: "requires verification"},
		{name: "unauthorized tool", mutate: func(r *PlanRevision) { r.TaskDAG[0].Requires = []string{"sudo"} }, want: "unauthorized tool"},
		{name: "unauthorized path", mutate: func(r *PlanRevision) { r.TaskDAG[0].ContextFiles = []string{"/etc/shadow"} }, want: "unauthorized path"},
		{name: "on_failure out of range", mutate: func(r *PlanRevision) { r.TaskDAG[0].OnFailure = intPtrForPlanTest(9) }, want: "on_failure"},
		{name: "on_failure non-ancestor", mutate: func(r *PlanRevision) { r.TaskDAG[0].OnFailure = intPtrForPlanTest(1) }, want: "on_failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			revision := PlanRevision{ID: "plan-1", Goal: "repair", AcceptanceFingerprint: fp, TaskDAG: []TaskDef{{Agent: "worker", Goal: "make change"}, {Agent: "worker", Goal: "verify"}}}
			tt.mutate(&revision)
			err := ValidatePlanRevision(revision, nil, PlanValidationContext{
				AcceptanceFingerprint: fp,
				AuthorizedTools:       map[string]map[string]bool{"worker": {"bash": true}},
				AllowedPaths:          map[string][]string{"worker": {"/workspace"}},
			})
			if err == nil || !containsText(err.Error(), tt.want) {
				t.Fatalf("validation error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPlanDiffRemapsAndRejectsOmittedOnFailureTargets(t *testing.T) {
	parent := []TaskDef{
		{ID: "unchanged", Agent: "worker", Goal: "unchanged"},
		{ID: "target", Agent: "worker", Goal: "target"},
		{ID: "loop", Agent: "worker", Goal: "old", DependsOn: []int{1}, OnFailure: intPtrForPlanTest(1)},
	}
	next := cloneTaskDefsForTest(parent)
	next[1].Goal = "target changed"
	next[2].Goal = "new loop"
	diff := buildPlanDiff(parent, next)
	selected, err := planTasksForExecution(PlanRevision{TaskDAG: next, DAGDiff: diff}, nil)
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected remapped tasks = %#v, err %v", selected, err)
	}
	if selected[1].OnFailure == nil || *selected[1].OnFailure != 0 {
		t.Fatalf("remapped on_failure = %#v", selected[1].OnFailure)
	}

	omitted := cloneTaskDefsForTest(parent)
	omitted[2].Goal = "only loop changed"
	omittedDiff := buildPlanDiff(parent, omitted)
	if _, err := planTasksForExecution(PlanRevision{TaskDAG: omitted, DAGDiff: omittedDiff}, []string{"target"}); err == nil || !containsText(err.Error(), "omitted") {
		t.Fatalf("omitted on_failure target error = %v", err)
	}
}

func intPtrForPlanTest(value int) *int { return &value }

func TestPersistPlanRevisionRejectsInvalidOnFailureBeforeReview(t *testing.T) {
	c := newPlanGateCoordinator()
	revision := PlanRevision{
		ID: "invalid-on-failure", Goal: "repair", AcceptanceFingerprint: AcceptanceFingerprint(nil, ""),
		TaskDAG: []TaskDef{{ID: "task", Agent: "worker", Goal: "task", OnFailure: intPtrForPlanTest(4)}},
		DAGDiff: PlanDiff{Added: []PlanTaskChange{{ID: "task"}}},
	}
	if err := c.PersistPlanRevision(revision); err == nil || !containsText(err.Error(), "on_failure") {
		t.Fatalf("invalid on_failure persisted before review: %v", err)
	}
}

func TestPlanRevisionValidatorRejectsParallelResourceConflict(t *testing.T) {
	err := ValidatePlanRevision(PlanRevision{ID: "plan", Goal: "repair", AcceptanceFingerprint: "fp", TaskDAG: []TaskDef{
		{Agent: "a", Goal: "one", ResourceClaims: []string{"db"}},
		{Agent: "b", Goal: "two", ResourceClaims: []string{"db"}},
	}}, nil, PlanValidationContext{AcceptanceFingerprint: "fp"})
	if err == nil || !containsText(err.Error(), "resource claim") {
		t.Fatalf("resource conflict error = %v", err)
	}
}

func TestPlanRevisionBuildsDiffAndReplays(t *testing.T) {
	packet := DiagnosticPacket{ID: "diag-1", TaskID: "task-1", Hypotheses: []RepairHypothesis{{ID: "hyp-1"}}}
	fp := AcceptanceFingerprint(nil, "")
	first, err := NewPlanRevision(nil, packet, "repair goal", "keep acceptance", []TaskDef{{Agent: "worker", Goal: "first"}}, fp)
	if err != nil {
		t.Fatal(err)
	}
	if first.TaskDAG[0].ID == "" {
		t.Fatal("NewPlanRevision did not assign a stable task ID")
	}
	second, err := NewPlanRevision(&first, packet, "repair goal", "keep acceptance", []TaskDef{{Agent: "worker", Goal: "replaced"}, {Agent: "worker", Goal: "new", DependsOn: []int{0}}}, fp)
	if err != nil {
		t.Fatal(err)
	}
	if second.ParentID != first.ID || len(second.DAGDiff.Modified) != 1 || len(second.DAGDiff.Added) != 1 || len(second.RepairHypothesisIDs) != 1 {
		t.Fatalf("revision diff = %#v", second)
	}
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	c := &Coordinator{session: &TeamSession{Workspace: dir, Config: agent.TeamConfig{Name: "plan-team"}, Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Tools: "bash"}}}, sessionData: NewSession(), eventStore: es}
	if err := c.PersistPlanRevision(first); err != nil {
		t.Fatal(err)
	}
	if err := c.PersistPlanRevision(first); err == nil {
		t.Fatal("expected equivalent plan revision to be rejected")
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	projected := ReduceToSessionData(events)
	t.Logf("projected: %#v", projected.PlanReviews)
	if len(projected.PlanRevisions) != 1 || projected.PlanRevisions[0].ID != first.ID {
		t.Fatalf("replayed revisions = %#v", projected.PlanRevisions)
	}
	selected, err := PlanTasksForExecution(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || len(selected[1].DependsOn) != 1 || selected[1].DependsOn[0] != 0 {
		t.Fatalf("execution diff = %#v", selected)
	}
	unchanged := second
	unchanged.DAGDiff.Added = nil
	unchanged.DAGDiff.Modified = nil
	if _, err := PlanTasksForExecution(unchanged); err == nil {
		t.Fatal("expected empty DAG diff to be rejected")
	}
}

func TestNewPlanRevisionIDsAuthorizeOmittedDependencyAfterTodoReplay(t *testing.T) {
	packet := DiagnosticPacket{ID: "diag-default-id"}
	fp := AcceptanceFingerprint(nil, "")
	first, err := NewPlanRevision(nil, packet, "repair", "", []TaskDef{{Agent: "worker", Goal: "base", Verify: "test -f output"}}, fp)
	if err != nil {
		t.Fatal(err)
	}
	c := newPlanGateCoordinator()
	c.taskTracker = NewTaskTracker()
	if err := c.PersistPlanRevision(first); err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		PlanTaskID: first.TaskDAG[0].ID, Agent: "worker", Desc: "base", Verify: "test -f output",
	}})[0]
	item.Status = TaskDone
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	if item.PlanTaskID == "" || item.PlanTaskID != first.TaskDAG[0].ID {
		t.Fatalf("default plan ID was not persisted to TodoItem: %#v", item)
	}

	second, err := NewPlanRevision(&first, packet, "repair", "", []TaskDef{
		first.TaskDAG[0],
		{Agent: "worker", Goal: "follow-up", DependsOn: []int{0}},
	}, fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PersistPlanRevision(second); err != nil {
		t.Fatal(err)
	}
	review, err := c.ReviewPlanRevision(second.ID)
	if err != nil || review.Status != "approved" {
		t.Fatalf("follow-up review = %#v, err %v", review, err)
	}
	selected, err := planTasksForExecution(second, coordinatorCompletedTaskIDs(c))
	if err != nil || len(selected) != 1 || selected[0].Goal != "follow-up" {
		t.Fatalf("follow-up selection = %#v, err %v", selected, err)
	}
}

func TestNewPlanRevisionRejectsDuplicateTaskIDs(t *testing.T) {
	_, err := NewPlanRevision(nil, DiagnosticPacket{ID: "diag-duplicate"}, "repair", "", []TaskDef{
		{ID: "same", Agent: "worker", Goal: "one"},
		{ID: "same", Agent: "worker", Goal: "two"},
	}, AcceptanceFingerprint(nil, ""))
	if err == nil || !containsText(err.Error(), "duplicate plan task ID") {
		t.Fatalf("duplicate ID error = %v", err)
	}
}

func TestBuildPlanDiffDetectsSemanticChangesWithStableTaskID(t *testing.T) {
	base := []TaskDef{
		{ID: "setup", Agent: "worker", Goal: "prepare"},
		{ID: "build", Agent: "worker", Goal: "build", Verify: "test -f output", Requires: []string{"bash"}, DependsOn: []int{0}},
		{ID: "alternate", Agent: "worker", Goal: "alternate"},
	}
	tests := []struct {
		name   string
		mutate func([]TaskDef)
	}{
		{name: "goal", mutate: func(tasks []TaskDef) { tasks[1].Goal = "build improved" }},
		{name: "verify", mutate: func(tasks []TaskDef) { tasks[1].Verify = "test -f new-output" }},
		{name: "tools", mutate: func(tasks []TaskDef) { tasks[1].Requires = []string{"bash", "grep"} }},
		{name: "dependency", mutate: func(tasks []TaskDef) { tasks[1].DependsOn = []int{2} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := cloneTaskDefsForTest(base)
			tt.mutate(next)
			diff := buildPlanDiff(base, next)
			if len(diff.Modified) != 1 || diff.Modified[0].ID != "build" {
				t.Fatalf("diff = %#v", diff)
			}
			completed := []string{taskRevisionID(next[next[1].DependsOn[0]])}
			selected, err := planTasksForExecution(PlanRevision{TaskDAG: next, DAGDiff: diff}, completed)
			if err != nil || len(selected) != 1 || selected[0].ID != "build" {
				t.Fatalf("selected = %#v, err %v", selected, err)
			}
		})
	}

	identical := buildPlanDiff(base, cloneTaskDefsForTest(base))
	if len(identical.Unchanged) != len(base) || len(identical.Modified) != 0 {
		t.Fatalf("identical diff = %#v", identical)
	}
}

func cloneTaskDefsForTest(tasks []TaskDef) []TaskDef {
	cloned := make([]TaskDef, len(tasks))
	for i := range tasks {
		cloned[i] = cloneTaskDef(tasks[i])
	}
	return cloned
}

func newPlanGateCoordinator() *Coordinator {
	return &Coordinator{
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "plan-gate", AllowedPaths: []string{"/workspace"}},
			Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Tools: "bash", AllowedPaths: []string{"/workspace"}}},
		},
		sessionData: NewSession(),
	}
}

func TestPersistPlanRevisionUsesProductionAcceptanceAuthorizationAndBudget(t *testing.T) {
	fp := AcceptanceFingerprint(nil, "")
	newRevision := func(task TaskDef, acceptance string) PlanRevision {
		return PlanRevision{ID: task.Goal, Goal: "repair", AcceptanceFingerprint: acceptance, TaskDAG: []TaskDef{task}}
	}
	tests := []struct {
		name       string
		task       TaskDef
		acceptance string
		setup      func(*Coordinator)
		want       string
	}{
		{name: "acceptance", task: TaskDef{Agent: "worker", Goal: "acceptance"}, acceptance: "caller-fingerprint", want: "acceptance fingerprint"},
		{name: "tool authorization", task: TaskDef{Agent: "worker", Goal: "tool", Requires: []string{"sudo"}}, acceptance: fp, want: "unauthorized tool"},
		{name: "path authorization", task: TaskDef{Agent: "worker", Goal: "path", ContextFiles: []string{"/etc/shadow"}}, acceptance: fp, want: "unauthorized path"},
		{name: "task budget", task: TaskDef{Agent: "worker", Goal: "budget"}, acceptance: fp, setup: func(c *Coordinator) { c.SetPlanBudget(0, 0); c.session.Config.MaxConcurrent = 0; c.planMaxTasks = 0 }, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPlanGateCoordinator()
			if tt.setup != nil {
				tt.setup(c)
			}
			if tt.name == "task budget" {
				c.SetPlanBudget(1, 1)
				task := tt.task
				revision := PlanRevision{ID: "budget", Goal: "repair", AcceptanceFingerprint: fp, TaskDAG: []TaskDef{task, {Agent: "worker", Goal: "budget-2"}}}
				if err := c.PersistPlanRevision(revision); err == nil || !containsText(err.Error(), "exceeding budget") {
					t.Fatalf("budget error = %v", err)
				}
				return
			}
			err := c.PersistPlanRevision(newRevision(tt.task, tt.acceptance))
			if err == nil || !containsText(err.Error(), tt.want) {
				t.Fatalf("production gate error = %v, want %q", err, tt.want)
			}
		})
	}
	c := newPlanGateCoordinator()
	c.tokenBudget = 500
	if err := c.PersistPlanRevision(newRevision(TaskDef{Agent: "worker", Goal: "token-budget"}, fp)); err == nil || !containsText(err.Error(), "token budget") {
		t.Fatalf("token budget error = %v", err)
	}
}

func TestPlanDiffRejectsUnfinishedOmittedDependency(t *testing.T) {
	parent := []TaskDef{{Agent: "worker", Goal: "unchanged"}, {Agent: "worker", Goal: "old", DependsOn: []int{0}}}
	revision := PlanRevision{ID: "plan-dependency", Goal: "repair", AcceptanceFingerprint: "fp", TaskDAG: []TaskDef{
		parent[0], {Agent: "worker", Goal: "modified", DependsOn: []int{0}},
	}, DAGDiff: buildPlanDiff(parent, []TaskDef{
		parent[0], {Agent: "worker", Goal: "modified", DependsOn: []int{0}},
	})}
	if _, err := PlanTasksForExecution(revision); err == nil || !containsText(err.Error(), "unfinished dependency") {
		t.Fatalf("unfinished dependency error = %v", err)
	}
	revision.CompletedTaskIDs = []string{taskRevisionID(parent[0])}
	if _, err := PlanTasksForExecution(revision); err == nil || !containsText(err.Error(), "unfinished dependency") {
		t.Fatalf("caller-supplied completion was accepted: %v", err)
	}
}

func TestPersistPlanRevisionDeepCopiesCallerOwnedData(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	c := newPlanGateCoordinator()
	c.eventStore = es
	fp := AcceptanceFingerprint(nil, "")
	revision := PlanRevision{ID: "immutable", Goal: "repair", AcceptanceFingerprint: fp,
		DiagnosticPacketIDs: []string{"diag-original"},
		TaskDAG:             []TaskDef{{Agent: "worker", Goal: "original", ContextFiles: []string{"/workspace/a"}, VerifySpec: &VerificationSpec{Path: "/workspace/a"}}},
		DAGDiff:             PlanDiff{Added: []PlanTaskChange{{ID: "task-original", DependsOn: []string{"dep-original"}}}},
	}
	if err := c.PersistPlanRevision(revision); err != nil {
		t.Fatal(err)
	}
	revision.DiagnosticPacketIDs[0] = "diag-mutated"
	revision.TaskDAG[0].Goal = "mutated"
	revision.TaskDAG[0].ContextFiles[0] = "/etc/shadow"
	revision.TaskDAG[0].VerifySpec.Path = "/etc/shadow"
	revision.DAGDiff.Added[0].DependsOn[0] = "dep-mutated"
	stored := c.PlanRevisions()
	if len(stored) != 1 || stored[0].DiagnosticPacketIDs[0] != "diag-original" || stored[0].TaskDAG[0].Goal != "original" || stored[0].TaskDAG[0].VerifySpec.Path != "/workspace/a" || stored[0].DAGDiff.Added[0].DependsOn[0] != "dep-original" {
		t.Fatalf("stored revision was mutated: %#v", stored)
	}
	if c.sessionData.PlanRevisions[0].DiagnosticPacketIDs[0] != "diag-original" {
		t.Fatalf("session revision was mutated: %#v", c.sessionData.PlanRevisions)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	projected := ReduceToSessionData(events)
	if len(projected.PlanRevisions) != 1 || projected.PlanRevisions[0].TaskDAG[0].Goal != "original" {
		t.Fatalf("event revision was mutated: %#v", projected.PlanRevisions)
	}
}

func TestExecutePlanRevisionCannotBypassProductionGates(t *testing.T) {
	c := newPlanGateCoordinator()
	task := TaskDef{Agent: "worker", Goal: "execute"}
	revision := PlanRevision{ID: "execute-gate", Goal: "repair", AcceptanceFingerprint: "caller-fingerprint", Review: PlanReviewResult{Status: "approved"}, TaskDAG: []TaskDef{task}, DAGDiff: PlanDiff{Added: []PlanTaskChange{{ID: taskRevisionID(task)}}}}
	if _, err := c.ExecutePlanRevision(context.Background(), revision); err == nil || !containsText(err.Error(), "acceptance fingerprint") {
		t.Fatalf("execute gate error = %v", err)
	}
	if len(c.PlanRevisions()) != 0 {
		t.Fatalf("rejected revision was persisted: %#v", c.PlanRevisions())
	}
}

func TestPlanRevisionCompletionEvidenceIsCoordinatorOwned(t *testing.T) {
	c := newPlanGateCoordinator()
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", PlanTaskID: "dep-1", Agent: "worker", Desc: "pending", Status: TaskInProgress},
		{ID: "2", PlanTaskID: "dep-2", Agent: "worker", Desc: "verified", Status: TaskDone, Verify: "test -f output", VerifyResult: &VerificationResult{ExitCode: 0}},
		{ID: "3", PlanTaskID: "dep-3", Agent: "worker", Desc: "unverified", Status: TaskDone, Verify: "test -f missing"},
	})
	completed := coordinatorCompletedTaskIDs(c)
	if len(completed) != 1 || completed[0] != "dep-2" {
		t.Fatalf("coordinator completion evidence = %#v", completed)
	}
}

func TestPlanTaskIDFlowsThroughTodoExecutionAndReplay(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	item := tl.AddBatch([]TodoSpec{{PlanTaskID: "plan-build", Agent: "worker", Desc: "build"}})[0]
	if item.ID == item.PlanTaskID || item.PlanTaskID != "plan-build" {
		t.Fatalf("todo identity = runtime %q, plan %q", item.ID, item.PlanTaskID)
	}
	if got := taskDefFromTodoItem(item).ID; got != "plan-build" {
		t.Fatalf("task definition ID = %q", got)
	}
	restored := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: item.ID, Payload: mustJSON(t, item)}})
	if len(restored) != 1 || restored[0].PlanTaskID != "plan-build" {
		t.Fatalf("replayed plan task ID = %#v", restored)
	}
}

func TestPlanRevisionReviewUsesCompletedOmittedDependencyEvidence(t *testing.T) {
	c := newPlanGateCoordinator()
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().Restore([]*TodoItem{{
		ID: "runtime-1", PlanTaskID: "base", Agent: "worker", Desc: "completed base",
		Status: TaskDone, Verify: "test -f output", VerifyResult: &VerificationResult{ExitCode: 0},
	}})
	fp := AcceptanceFingerprint(nil, "")
	revision := PlanRevision{
		ID: "follow-up-review", Goal: "continue repair", AcceptanceFingerprint: fp,
		TaskDAG: []TaskDef{
			{ID: "base", Agent: "worker", Goal: "completed base"},
			{ID: "follow-up", Agent: "worker", Goal: "continue repair", DependsOn: []int{0}},
		},
		DAGDiff: PlanDiff{Added: []PlanTaskChange{{ID: "follow-up", DependsOn: []string{"base"}}}},
	}
	if err := c.PersistPlanRevision(revision); err != nil {
		t.Fatal(err)
	}
	review, err := c.ReviewPlanRevision(revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != "approved" {
		t.Fatalf("review status = %q, reason %q", review.Status, review.Reason)
	}
	selected, err := planTasksForExecution(revision, coordinatorCompletedTaskIDs(c))
	if err != nil || len(selected) != 1 || len(selected[0].DependsOn) != 0 {
		t.Fatalf("selected follow-up tasks = %#v, err %v", selected, err)
	}
}

func TestPlanRevisionApprovalCannotBeForgedOrStale(t *testing.T) {
	c := newPlanGateCoordinator()
	fp := AcceptanceFingerprint(nil, "")
	task := TaskDef{ID: "task-1", Agent: "worker", Goal: "approved task"}
	revision := PlanRevision{ID: "reviewed", Goal: "repair", AcceptanceFingerprint: fp, TaskDAG: []TaskDef{task}, DAGDiff: PlanDiff{Added: []PlanTaskChange{{ID: task.ID}}}, Review: PlanReviewResult{Status: "approved", Reviewer: "caller"}}
	if err := c.PersistPlanRevision(revision); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecutePlanRevision(context.Background(), revision); err == nil || !containsText(err.Error(), "trusted plan reviewer") {
		t.Fatalf("forged approval error = %v", err)
	}
	digest := canonicalPlanRevisionDigest(c.PlanRevisions()[0])
	bad := PlanReviewResult{Status: "approved", Reviewer: "plan-reviewer", ReviewedAt: time.Now(), RevisionID: revision.ID, RevisionDigest: "stale"}
	if err := c.recordTrustedPlanReview(revision.ID, bad); err == nil {
		t.Fatal("stale reviewer approval was accepted")
	}
	good := bad
	good.RevisionDigest = digest
	if err := c.recordTrustedPlanReview(revision.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.trustedPlanReview(revision.ID, digest); !ok {
		t.Fatal("trusted reviewer approval was not recorded")
	}
}

func TestPlanRevisionReviewRejectsAndReplaysAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-review", "session-review")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	c := newPlanGateCoordinator()
	c.eventStore = es
	c.sessionData = NewSession()
	fp := AcceptanceFingerprint(nil, "")
	revision := PlanRevision{ID: "review-replay", Goal: "repair", AcceptanceFingerprint: fp, TaskDAG: []TaskDef{{ID: "build", Agent: "worker", Goal: "build"}}, DAGDiff: PlanDiff{Added: []PlanTaskChange{{ID: "build"}}}}
	if err := c.PersistPlanRevision(revision); err != nil {
		t.Fatal(err)
	}
	approved, err := c.ReviewPlanRevision(revision.ID)
	if err != nil || approved.Status != "approved" {
		t.Fatalf("review = %#v, err %v", approved, err)
	}
	digest := approved.RevisionDigest
	rejectedReview := approved
	rejectedReview.Status = "rejected"
	rejectedReview.Reviewer = "reviewer-second"
	rejectedReview.Reason = "temporary rejection"
	rejectedReview.ReviewedAt = approved.ReviewedAt.Add(time.Second)
	if err := c.recordTrustedPlanReview(revision.ID, rejectedReview); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.trustedPlanReview(revision.ID, digest); ok {
		t.Fatal("live rejected review remained trusted")
	}
	approvedAgain := rejectedReview
	approvedAgain.Status = "approved"
	approvedAgain.Reviewer = "reviewer-third"
	approvedAgain.Reason = "re-reviewed and approved"
	approvedAgain.ReviewedAt = rejectedReview.ReviewedAt.Add(time.Second)
	if err := c.recordTrustedPlanReview(revision.ID, approvedAgain); err != nil {
		t.Fatal(err)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	projected := ReduceToSessionData(events)
	if len(projected.PlanReviews) != 1 {
		t.Fatalf("projected reviews = %#v", projected.PlanReviews)
	}
	restarted := newPlanGateCoordinator()
	restarted.taskTracker = NewTaskTracker()
	restarted.SetSessionData(projected)
	if got, ok := restarted.trustedPlanReview(revision.ID, digest); !ok || got.Reviewer != approvedAgain.Reviewer || got.Reason != approvedAgain.Reason {
		t.Fatal("trusted approval was not restored after replay")
	}

	rejected := revision
	rejected.ID = "review-rejected"
	rejected.ParentID = revision.ID
	rejected.DAGDiff = PlanDiff{}
	if err := c.PersistPlanRevision(rejected); err != nil {
		t.Fatal(err)
	}
	decision, err := c.ReviewPlanRevision(rejected.ID)
	if err != nil || decision.Status != "rejected" {
		t.Fatalf("rejection = %#v, err %v", decision, err)
	}
	if _, ok := c.trustedPlanReview(rejected.ID, decision.RevisionDigest); ok {
		t.Fatal("rejected review became executable approval")
	}
}

func TestPlanRevisionReviewerToolsDoNotExecuteTasks(t *testing.T) {
	c := &Coordinator{
		pendingPlans:    map[string]*PlanEntry{"revision": {TodoID: "revision", PlanRevisionID: "revision", Status: "submitted"}},
		approvedOutputs: make(map[string]string),
		approvedErrors:  make(map[string]error),
	}
	if _, err := (&reviewerApprovePlanTool{coordinator: c, todoID: "revision"}).Run(context.Background(), fantasy.ToolCall{}); err != nil {
		t.Fatal(err)
	}
	if c.pendingPlans["revision"].Status != "approved" {
		t.Fatalf("revision approval status = %q", c.pendingPlans["revision"].Status)
	}

	c.pendingPlans["revision"].Status = "submitted"
	if _, err := (&reviewerRejectPlanTool{coordinator: c, todoID: "revision"}).Run(context.Background(), fantasy.ToolCall{Input: `{"reason":"unsafe"}`}); err != nil {
		t.Fatal(err)
	}
	entry := c.pendingPlans["revision"]
	if entry.Status != "rejected" || entry.ReviewReason != "unsafe" {
		t.Fatalf("revision rejection = status %q reason %q", entry.Status, entry.ReviewReason)
	}
}

func containsText(value, want string) bool {
	for i := 0; i+len(want) <= len(value); i++ {
		if value[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
