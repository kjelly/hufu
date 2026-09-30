package team

import (
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func newBranchStateTestCoordinator(t *testing.T) (*Coordinator, string, string) {
	t.Helper()
	workspace := t.TempDir()
	st := NewSessionTree()
	branch, err := st.CreateRootBranch("run-branch")
	if err != nil {
		t.Fatalf("CreateRootBranch: %v", err)
	}
	st.ActiveBranch = branch.ID
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatalf("SaveSessionTree: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:       "team-x",
				Generation: agent.GenerationParams{Model: "m-1"},
			},
		},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	return c, workspace, branch.ID
}

func setBranchActiveModel(t *testing.T, workspace, branchID, model string) {
	t.Helper()
	st, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatalf("LoadSessionTree: %v", err)
	}
	st.Branches[branchID].State.ActiveModel = model
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatalf("SaveSessionTree: %v", err)
	}
}

func branchActiveModel(t *testing.T, workspace, branchID string) string {
	t.Helper()
	st, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatalf("LoadSessionTree: %v", err)
	}
	return st.Branches[branchID].State.ActiveModel
}

func TestUpdateBranchStateSkipsRewriteWhenRecordedStateIsUnchanged(t *testing.T) {
	c, workspace, branchID := newBranchStateTestCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "dev", Desc: "t1"}})
	c.saveCheckpoint()
	if got := branchActiveModel(t, workspace, branchID); got != "m-1" {
		t.Fatalf("first checkpoint ActiveModel = %q, want m-1", got)
	}

	// A marker written behind the coordinator's back survives a checkpoint
	// only if that checkpoint did not rewrite the tree.
	setBranchActiveModel(t, workspace, branchID, "external-marker")
	c.saveCheckpoint()
	if got := branchActiveModel(t, workspace, branchID); got != "external-marker" {
		t.Fatalf("unchanged checkpoint rewrote the tree: ActiveModel = %q", got)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "", ""); err != nil {
		t.Fatalf("TryUpdateStatusAndOutput: %v", err)
	}
	c.saveCheckpoint()
	if got := branchActiveModel(t, workspace, branchID); got != "m-1" {
		t.Fatalf("status change did not rewrite the tree: ActiveModel = %q", got)
	}
	st, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatalf("LoadSessionTree: %v", err)
	}
	plan := st.Branches[branchID].State.TaskPlan
	if len(plan) != 1 || plan[0].Status != TaskInProgress {
		t.Fatalf("TaskPlan = %+v, want one in-progress task", plan)
	}
}

func TestUpdateBranchStateConcurrentCheckpoints(t *testing.T) {
	c, workspace, branchID := newBranchStateTestCoordinator(t)
	specs := make([]TodoSpec, 8)
	for i := range specs {
		specs[i] = TodoSpec{Agent: "dev", Desc: "task"}
	}
	c.taskTracker.TodoList().AddBatch(specs)
	var wg sync.WaitGroup
	for _, item := range c.taskTracker.TodoList().Items() {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(id, TaskInProgress, "", ""); err != nil {
				t.Errorf("TryUpdateStatusAndOutput(%s): %v", id, err)
			}
			c.saveCheckpoint()
		}(item.ID)
	}
	wg.Wait()
	c.saveCheckpoint()
	st, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatalf("LoadSessionTree: %v", err)
	}
	plan := st.Branches[branchID].State.TaskPlan
	if len(plan) != len(specs) {
		t.Fatalf("TaskPlan has %d tasks, want %d", len(plan), len(specs))
	}
	for _, item := range plan {
		if item.Status != TaskInProgress {
			t.Fatalf("task %s status = %q, want in_progress", item.ID, item.Status)
		}
	}
}

func TestBranchStateFingerprint(t *testing.T) {
	base := func() []*TodoItem {
		return []*TodoItem{{ID: "1", Desc: "review", Status: TaskInProgress}}
	}
	baseline := branchStateFingerprint(base(), "m-1", "team-x", nil)
	cases := []struct {
		name       string
		plan       []*TodoItem
		model      string
		team       string
		compaction *StructuredSummary
		wantSame   bool
	}{
		{name: "identical state", plan: base(), model: "m-1", team: "team-x", wantSame: true},
		{name: "unrecorded field changes", plan: func() []*TodoItem {
			p := base()
			p[0].LastOperation = "grep"
			return p
		}(), model: "m-1", team: "team-x", wantSame: true},
		{name: "status", plan: func() []*TodoItem {
			p := base()
			p[0].Status = TaskDone
			return p
		}(), model: "m-1", team: "team-x"},
		{name: "retries", plan: func() []*TodoItem {
			p := base()
			p[0].Retries = 1
			return p
		}(), model: "m-1", team: "team-x"},
		{name: "output", plan: func() []*TodoItem {
			p := base()
			p[0].Output = "done"
			return p
		}(), model: "m-1", team: "team-x"},
		{name: "typed result", plan: func() []*TodoItem {
			p := base()
			p[0].TypedResult = &TaskResult{}
			return p
		}(), model: "m-1", team: "team-x"},
		{name: "resolution", plan: func() []*TodoItem {
			p := base()
			p[0].Resolution = &TaskResolution{Status: "reconciled"}
			return p
		}(), model: "m-1", team: "team-x"},
		{name: "added task", plan: append(base(), &TodoItem{ID: "2"}), model: "m-1", team: "team-x"},
		{name: "model", plan: base(), model: "m-2", team: "team-x"},
		{name: "team", plan: base(), model: "m-1", team: "team-y"},
		{name: "compaction", plan: base(), model: "m-1", team: "team-x", compaction: &StructuredSummary{Goal: "g"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := branchStateFingerprint(tc.plan, tc.model, tc.team, tc.compaction)
			if (got == baseline) != tc.wantSame {
				t.Fatalf("fingerprint same = %v, want %v", got == baseline, tc.wantSame)
			}
		})
	}
}
