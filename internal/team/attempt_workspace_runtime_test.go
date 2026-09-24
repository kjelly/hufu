package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// isolatedEditAgent is a fake worker whose "tool effects" are file edits in
// the attempt's execution root. It submits through the real submit_result
// tool, so completion runs the production pipeline end to end.
type isolatedEditAgent struct {
	c *Coordinator
	// edit runs inside the attempt. It receives the execution root the
	// runtime installed for this attempt.
	edit func(root, goal string, attempt int) error
	// omitResult skips submit_result, leaving a protocol-incomplete handoff.
	omitResult bool

	mu    sync.Mutex
	roots map[string][]string
}

func (a *isolatedEditAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx, call.Prompt)
}

func (a *isolatedEditAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx, call.Prompt)
}

func (a *isolatedEditAgent) run(ctx context.Context, prompt string) (*fantasy.AgentResult, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	root, ok := ctx.Value(tools.AgentExecutionRootKey).(tools.AgentExecutionRoot)
	if !ok || root.Root == "" {
		return nil, errors.New("the attempt has no isolated execution root")
	}
	a.mu.Lock()
	if a.roots == nil {
		a.roots = make(map[string][]string)
	}
	a.roots[todoID] = append(a.roots[todoID], root.Root)
	a.mu.Unlock()
	goal := ""
	if item := a.c.todoItemByID(todoID); item != nil {
		goal = item.Goal
	}
	if a.edit != nil {
		if err := a.edit(root.Root, goal, attempt); err != nil {
			return nil, err
		}
	}
	if !a.omitResult {
		response, err := (&submitResultTool{coordinator: a.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{
			Name: submitResultToolName, Input: `{"status":"success","summary":"edited the isolated project"}`,
		})
		if err != nil {
			return nil, err
		}
		if response.IsError {
			return nil, errors.New(response.Content)
		}
	}
	_ = prompt
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "edited the isolated project"},
	}}}, nil
}

func (a *isolatedEditAgent) rootsFor(todoID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.roots[todoID]...)
}

type isolatedTeamFixture struct {
	c       *Coordinator
	subject string
	control string
	store   *EventStore
}

func newIsolatedTeamFixture(t *testing.T, worker func(*Coordinator) fantasy.Agent) *isolatedTeamFixture {
	t.Helper()
	subject := t.TempDir()
	control := t.TempDir()
	workspace := filepath.Join(control, "session")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// executeTask's fire-and-forget memory writers target the workspace.
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	session := &TeamSession{
		Workspace: workspace,
		Scope:     WorkspaceScope{Managed: true, ControlRoot: control, SubjectRoot: subject},
		Config:    agent.TeamConfig{Name: "isolated-runtime", Timeout: 30, MaxRetries: 2, WorkerWorkspace: isolatedSpec()},
		Agents: map[string]*agent.AgentDef{
			"worker": {
				Name: "worker", Role: "worker", Tools: "view,write,edit,bash,grep,glob,ls",
				SideEffect: string(SideEffectWorkspaceWrite), MaxRetries: 2,
				Generation: agent.GenerationParams{Model: "primary"},
			},
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run-isolated", "session-isolated")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.executionRunID = "run-isolated"
	c.contextRepo = nil
	if worker != nil {
		c.workerAgentOverride = worker(c)
	}
	return &isolatedTeamFixture{c: c, subject: subject, control: control, store: store}
}

func (f *isolatedTeamFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	path := filepath.Join(f.subject, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *isolatedTeamFixture) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.subject, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func (f *isolatedTeamFixture) eventTypes(t *testing.T) []string {
	t.Helper()
	events, err := f.c.EventJournal().ReadEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, event := range events {
		if strings.HasPrefix(event.Type, "attempt_workspace_") {
			types = append(types, event.Type)
		}
	}
	return types
}

func (f *isolatedTeamFixture) worldDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(attemptWorldsDir(f.control))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func (f *isolatedTeamFixture) item(t *testing.T, goal string) *TodoItem {
	t.Helper()
	for _, item := range f.c.taskTracker.TodoList().Items() {
		if item.Goal == goal {
			return item
		}
	}
	t.Fatalf("no task with goal %q", goal)
	return nil
}

func writeWorldFile(root, rel, content string) error {
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func TestIsolatedAttemptAppliesVerifiedChangesBeforeTaskDone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the dirty Git working tree case")
	}
	var f *isolatedTeamFixture
	var canonicalDuringAttempt []string
	f = newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, _ string, _ int) error {
			if root == f.subject {
				return errors.New("the attempt ran in the canonical project")
			}
			// The uncommitted edit and the untracked file were copied.
			for rel, want := range map[string]string{"README.md": "dirty\n", "notes.txt": "untracked\n"} {
				got, err := os.ReadFile(filepath.Join(root, rel))
				if err != nil || string(got) != want {
					return fmt.Errorf("world %s = %q, %v; want %q", rel, got, err, want)
				}
			}
			if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
				return errors.New("the world contains the project's .git")
			}
			if err := writeWorldFile(root, "out/result.txt", "built\n"); err != nil {
				return err
			}
			if err := writeWorldFile(root, "README.md", "edited\n"); err != nil {
				return err
			}
			// Ignored build output never reaches the project.
			if err := writeWorldFile(root, "build.log", "noise\n"); err != nil {
				return err
			}
			canonicalDuringAttempt = []string{f.read(t, "README.md")}
			if _, err := os.Stat(filepath.Join(f.subject, "out", "result.txt")); err == nil {
				canonicalDuringAttempt = append(canonicalDuringAttempt, "out/result.txt exists")
			}
			return nil
		}}
	})
	f.write(t, ".gitignore", "*.log\n")
	f.write(t, "README.md", "clean\n")
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = f.subject
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	f.write(t, "README.md", "dirty\n")
	f.write(t, "notes.txt", "untracked\n")

	const goal = "build the result"
	// The verify command passes only where the worker's output exists; at
	// verification time that is the attempt root, not the canonical project.
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: goal, Verify: "test -f out/result.txt"}}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	item := f.item(t, goal)
	if item.Status != TaskDone {
		t.Fatalf("task status = %s (%s), want done", item.Status, item.Detail)
	}
	if !slices.Equal(canonicalDuringAttempt, []string{"dirty\n"}) {
		t.Fatalf("canonical project changed before completion: %q", canonicalDuringAttempt)
	}
	if got := f.read(t, "out/result.txt"); got != "built\n" {
		t.Fatalf("applied out/result.txt = %q", got)
	}
	if got := f.read(t, "README.md"); got != "edited\n" {
		t.Fatalf("applied README.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.subject, "build.log")); err == nil {
		t.Fatal("ignored build output was applied to the project")
	}
	want := []string{string(EventAttemptWorkspacePrepared), string(EventAttemptWorkspaceApplyStarted), string(EventAttemptWorkspaceApplied)}
	if got := f.eventTypes(t); !slices.Equal(got, want) {
		t.Fatalf("attempt world events = %v, want %v", got, want)
	}
	if dirs := f.worldDirs(t); len(dirs) != 0 {
		t.Fatalf("attempt worlds left after completion: %v", dirs)
	}
}

func TestIsolatedAttemptApplyStartedCarriesCompletionEvidence(t *testing.T) {
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, _ string, _ int) error {
			return writeWorldFile(root, "a.txt", "a\n")
		}}
	})
	const goal = "write a"
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: goal, Verify: "test -f a.txt"}}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	events, err := f.c.EventJournal().ReadEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		WorldID           string              `json:"world_id"`
		DeltaDigest       string              `json:"delta_digest"`
		ChangeCount       int                 `json:"change_count"`
		TypedResult       *TaskResult         `json:"typed_result"`
		VerifyResult      *VerificationResult `json:"verify_result"`
		ExecutionReceipt  *ExecutionReceipt   `json:"execution_receipt"`
		CoordinatorOutput string              `json:"coordinator_output"`
	}
	found := false
	for _, event := range events {
		if event.Type == string(EventAttemptWorkspaceApplyStarted) {
			found = true
			if err := json.Unmarshal(event.Payload, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found {
		t.Fatal("no attempt_workspace_apply_started event")
	}
	if started.WorldID == "" || started.DeltaDigest == "" || started.ChangeCount != 1 {
		t.Fatalf("apply_started identity = %+v", started)
	}
	if started.TypedResult == nil || started.TypedResult.Status != TaskResultStatusSuccess {
		t.Fatalf("apply_started typed_result = %+v", started.TypedResult)
	}
	if started.VerifyResult == nil || started.VerifyResult.ExitCode != 0 || started.VerifyResult.Command != "test -f a.txt" {
		t.Fatalf("apply_started verify_result = %+v", started.VerifyResult)
	}
	if started.ExecutionReceipt == nil || started.ExecutionReceipt.HandoffState != ResultHandoffSubmitted {
		t.Fatalf("apply_started execution_receipt = %+v", started.ExecutionReceipt)
	}
	if strings.TrimSpace(started.CoordinatorOutput) == "" {
		t.Fatal("apply_started carries no coordinator output")
	}
}

// barrier releases every caller once n of them have arrived, so parallel
// attempts demonstrably overlap.
type barrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	release chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, release: make(chan struct{})} }

func (b *barrier) wait() error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("parallel attempts did not overlap")
	}
}

func TestParallelIsolatedAttempts(t *testing.T) {
	tests := []struct {
		name string
		// edit is what each task's attempt does in its world.
		edit func(root, goal string, attempt int) error
		// check inspects the final canonical project.
		check func(t *testing.T, f *isolatedTeamFixture)
		// conflicts is the number of apply conflicts expected.
		conflicts int
	}{
		{
			name: "different files both apply",
			edit: func(root, goal string, _ int) error {
				return writeWorldFile(root, strings.ReplaceAll(goal, " ", "-")+".txt", goal+"\n")
			},
			check: func(t *testing.T, f *isolatedTeamFixture) {
				for _, goal := range []string{"write left", "write right"} {
					if got := f.read(t, strings.ReplaceAll(goal, " ", "-")+".txt"); got != goal+"\n" {
						t.Fatalf("%s = %q", goal, got)
					}
				}
			},
		},
		{
			name: "same file conflicts and retries on the new base",
			edit: func(root, goal string, _ int) error {
				existing, err := os.ReadFile(filepath.Join(root, "log.txt"))
				if err != nil {
					return err
				}
				return writeWorldFile(root, "log.txt", string(existing)+goal+"\n")
			},
			check: func(t *testing.T, f *isolatedTeamFixture) {
				got := f.read(t, "log.txt")
				serialA, serialB := "base\nwrite left\nwrite right\n", "base\nwrite right\nwrite left\n"
				if got != serialA && got != serialB {
					t.Fatalf("log.txt = %q, want the result of some serial order", got)
				}
			},
			conflicts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlap := newBarrier(2)
			f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
				return &isolatedEditAgent{c: c, edit: func(root, goal string, attempt int) error {
					if attempt == 1 {
						if err := overlap.wait(); err != nil {
							return err
						}
					}
					return tt.edit(root, goal, attempt)
				}}
			})
			f.write(t, "log.txt", "base\n")
			if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{
				{Agent: "worker", Goal: "write left"},
				{Agent: "worker", Goal: "write right"},
			}); err != nil {
				t.Fatalf("ExecuteTasks: %v", err)
			}
			for _, goal := range []string{"write left", "write right"} {
				if item := f.item(t, goal); item.Status != TaskDone {
					t.Fatalf("%s status = %s (%s)", goal, item.Status, item.Detail)
				}
			}
			tt.check(t, f)
			conflicts := 0
			for _, eventType := range f.eventTypes(t) {
				if eventType == string(EventAttemptWorkspaceApplyConflicted) {
					conflicts++
				}
			}
			if conflicts != tt.conflicts {
				t.Fatalf("conflicts = %d, want %d (events %v)", conflicts, tt.conflicts, f.eventTypes(t))
			}
			if dirs := f.worldDirs(t); len(dirs) != 0 {
				t.Fatalf("attempt worlds left after completion: %v", dirs)
			}
		})
	}
}

func TestIsolatedFailedAttemptDoesNotReachProjectOrNextAttempt(t *testing.T) {
	var f *isolatedTeamFixture
	var secondAttemptSawJunk bool
	f = newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, _ string, attempt int) error {
			if attempt == 1 {
				// Fails verification: the deliverable is missing.
				return writeWorldFile(root, "junk.txt", "half-done\n")
			}
			if _, err := os.Stat(filepath.Join(root, "junk.txt")); err == nil {
				secondAttemptSawJunk = true
			}
			return writeWorldFile(root, "ok.txt", "ok\n")
		}}
	})
	const goal = "produce ok"
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: goal, Verify: "test -f ok.txt"}}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	if item := f.item(t, goal); item.Status != TaskDone {
		t.Fatalf("status = %s (%s)", item.Status, item.Detail)
	}
	if secondAttemptSawJunk {
		t.Fatal("the failed attempt's world leaked into the next attempt")
	}
	if _, err := os.Stat(filepath.Join(f.subject, "junk.txt")); err == nil {
		t.Fatal("the failed attempt's change reached the project")
	}
	want := []string{
		string(EventAttemptWorkspacePrepared), string(EventAttemptWorkspaceDiscarded),
		string(EventAttemptWorkspacePrepared), string(EventAttemptWorkspaceApplyStarted), string(EventAttemptWorkspaceApplied),
	}
	if got := f.eventTypes(t); !slices.Equal(got, want) {
		t.Fatalf("attempt world events = %v, want %v", got, want)
	}
	agent := f.c.workerAgentOverride.(*isolatedEditAgent)
	roots := agent.rootsFor(f.item(t, goal).ID)
	if len(roots) != 2 || roots[0] == roots[1] {
		t.Fatalf("attempt roots = %v, want two distinct worlds", roots)
	}
}

func TestIsolatedDownstreamTaskSeesAppliedChanges(t *testing.T) {
	var downstreamSaw string
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, goal string, _ int) error {
			if goal == "write upstream" {
				return writeWorldFile(root, "upstream.txt", "from upstream\n")
			}
			data, err := os.ReadFile(filepath.Join(root, "upstream.txt"))
			if err != nil {
				return err
			}
			downstreamSaw = string(data)
			return writeWorldFile(root, "downstream.txt", "done\n")
		}}
	})
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "worker", Goal: "write upstream"},
		{Agent: "worker", Goal: "read upstream", DependsOn: []int{0}},
	}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	if downstreamSaw != "from upstream\n" {
		t.Fatalf("downstream world saw upstream.txt = %q", downstreamSaw)
	}
	if got := f.read(t, "downstream.txt"); got != "done\n" {
		t.Fatalf("downstream.txt = %q", got)
	}
}

func TestIsolatedProtocolRepairAppliesTheRepairedAttempt(t *testing.T) {
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, omitResult: true, edit: func(root, _ string, _ int) error {
			return writeWorldFile(root, "repaired.txt", "work\n")
		}}
	})
	f.c.repairAgentOverride = &mockRepairAgent{onSubmit: func() {
		for _, item := range f.c.taskTracker.TodoList().Items() {
			f.c.storeSubmittedTaskResult(item.ID, &TaskResult{Status: TaskResultStatusSuccess, Summary: "repaired", Source: "submitted"})
		}
	}}
	const goal = "repair me"
	if _, err := f.c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{{Agent: "worker", Goal: goal}}); err != nil {
		item := f.item(t, goal)
		t.Fatalf("ExecuteTasks: %v; status=%s detail=%q events=%v", err, item.Status, item.Detail, f.eventTypes(t))
	}
	if item := f.item(t, goal); item.Status != TaskDone {
		t.Fatalf("status = %s (%s)", item.Status, item.Detail)
	}
	if got := f.read(t, "repaired.txt"); got != "work\n" {
		t.Fatalf("repaired.txt = %q", got)
	}
}

// crashedProtocolIncompleteTask leaves the durable state a crash during the
// result-only repair of an isolated task leaves behind: the task is
// protocol_incomplete and its attempt world, holding the unapplied work, is
// still on disk.
func (f *isolatedTeamFixture) crashedProtocolIncompleteTask(t *testing.T, goal string) (*TodoItem, *isolatedAttempt) {
	t.Helper()
	ctx := context.Background()
	c := f.c
	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		t.Fatalf("execution policy: %v", err)
	}
	def := c.session.Agents["worker"]
	model := c.resolveAgentModel(def, "")
	task, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "worker", Goal: goal, Execution: ExecutionContract{RequiresResult: true}}, def, model)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	task.ModelTopology = initialTaskModelTopology(def, model)
	if task.ExecutionTopology, err = c.resolveCanonicalTaskTopology(task.ModelTopology, task.ResolvedExecutionTarget, task.SubagentProvider); err != nil {
		t.Fatal(err)
	}
	if !task.WorkerWorkspace.isolated() {
		t.Fatalf("admitted worker workspace = %+v, want isolated", task.WorkerWorkspace)
	}
	spec := TodoSpec{
		Agent: "worker", Desc: goal, Goal: goal, Model: model,
		ModelTopology: cloneModelTopology(task.ModelTopology), ExecutionTarget: task.ResolvedExecutionTarget,
		ExecutionTopology: cloneExecutionTopology(task.ExecutionTopology), WorkerWorkspace: task.WorkerWorkspace.clone(),
		SideEffect: task.SideEffect, Recovery: task.Recovery, Source: TaskSourceCoordinator, Execution: cloneExecutionContract(task.Execution),
	}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(ctx, projection, ids[0], 1); err != nil {
		t.Fatalf("admit: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	item := items[0]
	if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	world, err := c.prepareIsolatedAttempt(ctx, taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID, 1)
	if err != nil || world == nil {
		t.Fatalf("prepare world = %v, %v", world, err)
	}
	if err := writeWorldFile(world.prepared.Root, "resumed.txt", "unapplied work\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskProtocolIncomplete, "protocol incomplete: missing required result", "worker wrote resumed.txt", map[string]any{"protocol_checkpoint": true}); err != nil {
		t.Fatal(err)
	}
	return c.todoItemByID(item.ID), world
}

func TestResumeIsolatedProtocolIncompleteTask(t *testing.T) {
	tests := []struct {
		name string
		// loseWorld removes the attempt world before the resume.
		loseWorld     bool
		wantFile      string
		wantRepairs   int
		wantWorkerRun int
	}{
		{name: "retained world is repaired and applied", wantFile: "resumed.txt", wantRepairs: 1},
		{name: "lost world is re-dispatched", loseWorld: true, wantFile: "redispatched.txt", wantWorkerRun: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workerRuns := 0
			f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
				return &isolatedEditAgent{c: c, edit: func(root, _ string, _ int) error {
					workerRuns++
					return writeWorldFile(root, "redispatched.txt", "fresh attempt\n")
				}}
			})
			item, world := f.crashedProtocolIncompleteTask(t, "resume isolated repair")
			if tt.loseWorld {
				if err := os.RemoveAll(world.worldDir()); err != nil {
					t.Fatal(err)
				}
			}
			repairs := 0
			f.c.repairAgentOverride = &mockRepairAgent{onSubmit: func() {
				repairs++
				f.c.storeSubmittedTaskResult(item.ID, &TaskResult{TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess, Summary: "repaired after restart", Source: "submitted"})
			}}
			if _, err := f.c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); err != nil {
				got := f.c.todoItemByID(item.ID)
				t.Fatalf("ResumeInterruptedTasks: %v; status=%s detail=%q events=%v", err, got.Status, got.Detail, f.eventTypes(t))
			}
			got := f.c.todoItemByID(item.ID)
			if got.Status != TaskDone {
				t.Fatalf("status = %s (%s), events=%v", got.Status, got.Detail, f.eventTypes(t))
			}
			if repairs != tt.wantRepairs || workerRuns != tt.wantWorkerRun {
				t.Fatalf("repairs/worker runs = %d/%d, want %d/%d", repairs, workerRuns, tt.wantRepairs, tt.wantWorkerRun)
			}
			if _, err := os.Stat(filepath.Join(f.subject, tt.wantFile)); err != nil {
				t.Fatalf("%s was not applied: %v", tt.wantFile, err)
			}
			if tt.loseWorld {
				if _, err := os.Stat(filepath.Join(f.subject, "resumed.txt")); err == nil {
					t.Fatal("work from the lost world appeared in the project")
				}
			}
			if dirs := f.worldDirs(t); len(dirs) != 0 {
				t.Fatalf("attempt worlds left after completion: %v", dirs)
			}
		})
	}
}

func TestIsolatedWorkerEntryPoints(t *testing.T) {
	t.Run("direct agent runs in an attempt world", func(t *testing.T) {
		f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
			return &isolatedEditAgent{c: c, edit: func(root, _ string, _ int) error {
				return writeWorldFile(root, "direct.txt", "direct\n")
			}}
		})
		if _, err := f.c.RunDirectAgent(context.Background(), "worker", "direct isolated work"); err != nil {
			t.Fatalf("RunDirectAgent: %v", err)
		}
		if got := f.read(t, "direct.txt"); got != "direct\n" {
			t.Fatalf("direct.txt = %q", got)
		}
		want := []string{string(EventAttemptWorkspacePrepared), string(EventAttemptWorkspaceApplyStarted), string(EventAttemptWorkspaceApplied)}
		if got := f.eventTypes(t); !slices.Equal(got, want) {
			t.Fatalf("attempt world events = %v, want %v", got, want)
		}
	})
	t.Run("request_agent refuses an isolated sub-agent", func(t *testing.T) {
		f := newIsolatedTeamFixture(t, nil)
		before := len(f.c.taskTracker.TodoList().Items())
		response, err := (&requestAgentTool{coordinator: f.c}).Run(context.Background(), fantasy.ToolCall{
			Name: "request_agent", Input: `{"goal":"delegated isolated work","agent":"worker"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !response.IsError || !strings.Contains(response.Content, workspaceIsolationUnsupportedCode) {
			t.Fatalf("request_agent response = %+v, want %s", response, workspaceIsolationUnsupportedCode)
		}
		if after := len(f.c.taskTracker.TodoList().Items()); after != before {
			t.Fatalf("request_agent created %d task(s) for a refused sub-agent", after-before)
		}
	})
}
