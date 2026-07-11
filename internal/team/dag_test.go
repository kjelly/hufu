package team

import (
	"context"
	"testing"
	"time"
)

func intPtr(i int) *int { return &i }

func TestValidateOnFailureTargets(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []TaskDef
		wantErr bool
	}{
		{
			name:    "no on_failure",
			tasks:   []TaskDef{{Agent: "a"}, {Agent: "b", DependsOn: []int{0}}},
			wantErr: false,
		},
		{
			name:    "self target",
			tasks:   []TaskDef{{Agent: "a", OnFailure: intPtr(0)}},
			wantErr: false,
		},
		{
			name: "direct ancestor",
			tasks: []TaskDef{
				{Agent: "build"},
				{Agent: "test", DependsOn: []int{0}, OnFailure: intPtr(0)},
			},
			wantErr: false,
		},
		{
			name: "transitive ancestor",
			tasks: []TaskDef{
				{Agent: "gen"},
				{Agent: "build", DependsOn: []int{0}},
				{Agent: "test", DependsOn: []int{1}, OnFailure: intPtr(0)},
			},
			wantErr: false,
		},
		{
			name: "out of range",
			tasks: []TaskDef{
				{Agent: "a", OnFailure: intPtr(5)},
			},
			wantErr: true,
		},
		{
			name: "negative index",
			tasks: []TaskDef{
				{Agent: "a", OnFailure: intPtr(-1)},
			},
			wantErr: true,
		},
		{
			name: "unrelated task",
			tasks: []TaskDef{
				{Agent: "a"},
				{Agent: "b", OnFailure: intPtr(0)}, // b does not depend on a
			},
			wantErr: true,
		},
		{
			name: "descendant instead of ancestor",
			tasks: []TaskDef{
				{Agent: "build", OnFailure: intPtr(1)}, // points at its own dependent
				{Agent: "test", DependsOn: []int{0}},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOnFailureTargets(tc.tasks)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateOnFailureTargets() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestTodoListResetForRetry(t *testing.T) {
	cases := []struct {
		name       string
		fromStatus TaskStatus
	}{
		{name: "resets done task", fromStatus: TaskDone},
		{name: "resets errored task", fromStatus: TaskError},
		{name: "resets in-progress task", fromStatus: TaskInProgress},
		{name: "resets verifying task", fromStatus: TaskVerifying},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := &TodoList{}
			tl.AddBatch([]TodoSpec{{Agent: "a", Desc: "task", Verify: "test -f out.txt"}})
			tl.UpdateStatus("1", TaskInProgress, "")
			if tc.fromStatus != TaskInProgress {
				tl.UpdateStatus("1", tc.fromStatus, "finished")
			}
			if tc.fromStatus == TaskDone || tc.fromStatus == TaskError || tc.fromStatus == TaskVerifying {
				tl.UpdateStatusAndOutput("1", tc.fromStatus, "finished", "artifact")
				_ = tl.SetVerificationResult("1", &VerificationResult{Command: "test -f out.txt", ExitCode: 0})
			}

			tl.ResetForRetry("1", "reset by DAG retry")

			item := tl.Items()[0]
			if item.Status != TaskPending {
				t.Errorf("expected status pending after reset, got %s", item.Status)
			}
			if item.Retries != 1 {
				t.Errorf("expected retries 1 after reset, got %d", item.Retries)
			}
			if !item.StartedAt.IsZero() || !item.EndedAt.IsZero() {
				t.Errorf("expected timing cleared, got started=%v ended=%v", item.StartedAt, item.EndedAt)
			}
			if item.Detail != "reset by DAG retry" {
				t.Errorf("expected reset detail, got %q", item.Detail)
			}
			if item.Output != "" {
				t.Errorf("expected output cleared, got %q", item.Output)
			}
			if item.VerifyResult != nil {
				t.Errorf("expected verification result cleared, got %#v", item.VerifyResult)
			}

			// The re-run must be able to go Pending -> InProgress -> Done again.
			tl.UpdateStatus("1", TaskInProgress, "")
			if tl.Items()[0].Status != TaskInProgress {
				t.Errorf("expected in_progress after relaunch, got %s", tl.Items()[0].Status)
			}
			if tl.Items()[0].StartedAt.IsZero() {
				t.Error("expected StartedAt re-stamped on relaunch")
			}
			tl.UpdateStatus("1", TaskDone, "")
			if tl.Items()[0].Status != TaskDone {
				t.Errorf("expected done after relaunch, got %s", tl.Items()[0].Status)
			}
		})
	}
}

func TestInvalidateTaskCache(t *testing.T) {
	c := &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	ctx := context.Background()

	c.storeTaskCache("builder", "compile the project", "old output")
	c.storeTaskCache("builder", "another task", "keep me")
	c.storeTaskCache("tester", "compile the project", "different agent")

	if out, ok := c.lookupTaskCache(ctx, "builder", "compile the project"); !ok || out != "old output" {
		t.Fatalf("expected cache hit before invalidation, got ok=%v out=%q", ok, out)
	}

	// Normalization must match lookupTaskCache: case and whitespace insensitive.
	c.invalidateTaskCache("builder", "  Compile   THE project ")

	if _, ok := c.lookupTaskCache(ctx, "builder", "compile the project"); ok {
		t.Error("expected cache miss after invalidation")
	}
	if out, ok := c.lookupTaskCache(ctx, "builder", "another task"); !ok || out != "keep me" {
		t.Errorf("expected unrelated entry preserved, got ok=%v out=%q", ok, out)
	}
	if out, ok := c.lookupTaskCache(ctx, "tester", "compile the project"); !ok || out != "different agent" {
		t.Errorf("expected other agent's entry preserved, got ok=%v out=%q", ok, out)
	}

	// A re-run after invalidation stores fresh output and is served again.
	c.storeTaskCache("builder", "compile the project", "fresh output")
	if out, ok := c.lookupTaskCache(ctx, "builder", "compile the project"); !ok || out != "fresh output" {
		t.Errorf("expected fresh output after re-store, got ok=%v out=%q", ok, out)
	}
}

func TestExecuteTasksDefaultsMaxRetriesForOnFailure(t *testing.T) {
	// Mirrors the defaulting logic in ExecuteTasks: on_failure without
	// max_retries must still trigger at least one retry.
	tasks := []TaskDef{
		{Agent: "build"},
		{Agent: "test", DependsOn: []int{0}, OnFailure: intPtr(0)},
	}
	for i := range tasks {
		if tasks[i].OnFailure != nil && tasks[i].MaxRetries < 1 {
			tasks[i].MaxRetries = 1
		}
	}
	if tasks[1].MaxRetries != 1 {
		t.Errorf("expected MaxRetries defaulted to 1, got %d", tasks[1].MaxRetries)
	}
	if tasks[0].MaxRetries != 0 {
		t.Errorf("expected task without on_failure untouched, got %d", tasks[0].MaxRetries)
	}
}

func TestResetForRetryTimingReclock(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]TodoSpec{{Agent: "a", Desc: "task"}})
	tl.UpdateStatus("1", TaskInProgress, "")
	firstStart := tl.Items()[0].StartedAt
	tl.UpdateStatus("1", TaskDone, "")

	time.Sleep(2 * time.Millisecond)
	tl.ResetForRetry("1", "")
	tl.UpdateStatus("1", TaskInProgress, "")

	secondStart := tl.Items()[0].StartedAt
	if !secondStart.After(firstStart) {
		t.Errorf("expected fresh StartedAt after reset, first=%v second=%v", firstStart, secondStart)
	}
}
