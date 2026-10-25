package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// errGenericWorkerFailure is the default scripted failure: a plain execution
// error that is replayable and carries no structured classification.
var errGenericWorkerFailure = errors.New("worker failed: generic execution error")

// This file is the shared fixture layer for Coordinator tests. New tests that
// need a Coordinator able to run executeTask without a provider should start
// from newTestCoordinator and a scriptedWorker instead of building a
// Coordinator literal and a one-off fantasy.Agent.

// coordinatorFixture collects the knobs newTestCoordinator exposes.
type coordinatorFixture struct {
	config agent.TeamConfig
	agents map[string]*agent.AgentDef
	worker fantasy.Agent
	status StatusReporter
	runID  string
}

type coordinatorOption func(*coordinatorFixture) error

var errInvalidFixtureWorker = errors.New("withWorker requires a non-nil worker")

// withWorker routes every worker dispatch to the given agent.
// Explicit nil and typed-nil workers are configuration errors; omit the option
// to leave the coordinator's worker override unset.
func withWorker(worker fantasy.Agent) coordinatorOption {
	return func(f *coordinatorFixture) error {
		if worker == nil {
			return errInvalidFixtureWorker
		}
		value := reflect.ValueOf(worker)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return fmt.Errorf("%w: got nil %T", errInvalidFixtureWorker, worker)
			}
		}
		f.worker = worker
		return nil
	}
}

// withMaxRetries sets the team-level retry budget.
func withMaxRetries(n int) coordinatorOption {
	return func(f *coordinatorFixture) error {
		f.config.MaxRetries = n
		return nil
	}
}

// withTeamConfig edits the team configuration before the session is built.
func withTeamConfig(edit func(*agent.TeamConfig)) coordinatorOption {
	return func(f *coordinatorFixture) error {
		edit(&f.config)
		return nil
	}
}

// testWorkerDef is the default "worker" agent: a worker role whose own retry
// budget defers to the team configuration.
func testWorkerDef() *agent.AgentDef {
	return &agent.AgentDef{
		Name:       "worker",
		Role:       "worker",
		Generation: agent.GenerationParams{Model: "test"},
		MaxRetries: -1,
	}
}

// newTestCoordinator builds a minimal Coordinator around a temporary
// workspace. The default team has one "worker" agent, a 30 second timeout and
// three retries. Invalid options fail the test before allocating the workspace
// or coordinator. The coordinator is closed during test cleanup.
func newTestCoordinator(t *testing.T, opts ...coordinatorOption) *Coordinator {
	t.Helper()
	f := coordinatorFixture{
		config: agent.TeamConfig{Name: "test", Timeout: 30, MaxRetries: 3},
		agents: map[string]*agent.AgentDef{"worker": testWorkerDef()},
		status: func(StatusEvent) {},
		runID:  "run-test",
	}
	for i, opt := range opts {
		if err := opt(&f); err != nil {
			t.Fatalf("newTestCoordinator: option %d: %v", i+1, err)
		}
	}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    f.config,
			Agents:    f.agents,
		},
		sessionTime:    time.Now(),
		taskTracker:    NewTaskTracker(),
		reportStatus:   f.status,
		taskCache:      newDefaultTaskCache(taskCacheDependencies{}),
		executionRunID: f.runID,
	}
	cleanupTestCoordinator(t, c)
	c.workerAgentOverride = f.worker
	return c
}

// addTodo registers one pending task for the default worker and returns its
// todo ID.
func addTodo(t *testing.T, c *Coordinator, desc string) string {
	t.Helper()
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: desc}})
	if len(items) != 1 {
		t.Fatalf("AddBatch returned %d items, want 1", len(items))
	}
	return items[0].ID
}

// todoByID returns a snapshot of the tracked item or fails the test.
func todoByID(t *testing.T, c *Coordinator, id string) *TodoItem {
	t.Helper()
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("todo %q not tracked", id)
	return nil
}

// scriptedTurn is one worker response. text becomes the agent's output; err is
// returned alongside it, so a turn can carry partial output and still fail.
type scriptedTurn struct {
	text string
	err  error
}

// textTurn is a successful worker response.
func textTurn(text string) scriptedTurn { return scriptedTurn{text: text} }

// failTurn fails with partial output so the retry evidence gate is satisfied.
func failTurn(err error) scriptedTurn {
	return scriptedTurn{text: "partial work before failure", err: err}
}

// scriptedWorker is a fantasy.Agent whose responses follow a fixed script.
// The last turn repeats once the script is exhausted. Every prompt is recorded
// so tests can assert on the retry context the next attempt receives.
type scriptedWorker struct {
	mu      sync.Mutex
	turns   []scriptedTurn
	calls   int
	prompts []string
}

// scriptWorker builds a scriptedWorker. With no turns every call fails with a
// generic execution error.
func scriptWorker(turns ...scriptedTurn) *scriptedWorker {
	if len(turns) == 0 {
		turns = []scriptedTurn{failTurn(errGenericWorkerFailure)}
	}
	return &scriptedWorker{turns: turns}
}

// scriptFailThenSucceed fails the first attempt with partial output and
// answers "succeeded on retry" afterwards.
func scriptFailThenSucceed() *scriptedWorker {
	return scriptWorker(
		scriptedTurn{text: "first attempt produced partial output", err: errors.New("worker failed: first attempt error")},
		textTurn("succeeded on retry"),
	)
}

func (w *scriptedWorker) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return w.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (w *scriptedWorker) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	w.mu.Lock()
	w.calls++
	w.prompts = append(w.prompts, call.Prompt)
	turn := w.turns[min(w.calls, len(w.turns))-1]
	w.mu.Unlock()
	if turn.text == "" && turn.err != nil {
		return nil, turn.err
	}
	return textAgentResult(turn.text), turn.err
}

// Calls reports how many times the worker was dispatched.
func (w *scriptedWorker) Calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// Prompt returns the prompt of the given attempt (1-based) or fails the test.
func (w *scriptedWorker) Prompt(t *testing.T, attempt int) string {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if attempt < 1 || attempt > len(w.prompts) {
		t.Fatalf("attempt %d prompt not captured (%d calls)", attempt, len(w.prompts))
	}
	return w.prompts[attempt-1]
}

// textAgentResult wraps plain text as an agent response.
func textAgentResult(text string) *fantasy.AgentResult {
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: text},
	}}}
}

func TestWithWorkerValidation(t *testing.T) {
	worker := scriptWorker(textTurn("done"))
	for _, tt := range []struct {
		name    string
		worker  fantasy.Agent
		wantErr bool
	}{
		{name: "nil", wantErr: true},
		{name: "typed nil", worker: (*scriptedWorker)(nil), wantErr: true},
		{name: "typed nil alternate worker", worker: (*struct{ fantasy.Agent })(nil), wantErr: true},
		{name: "scripted worker", worker: worker},
		{name: "value worker", worker: struct{ fantasy.Agent }{worker}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A rejected option must not overwrite a previously valid worker.
			f := coordinatorFixture{worker: worker}
			err := withWorker(tt.worker)(&f)
			if tt.wantErr {
				if !errors.Is(err, errInvalidFixtureWorker) {
					t.Fatalf("withWorker error = %v, want %v", err, errInvalidFixtureWorker)
				}
				if f.worker != worker {
					t.Fatal("invalid option replaced the existing worker")
				}
				return
			}
			if err != nil || f.worker != tt.worker {
				t.Fatalf("withWorker = (%T, %v), want supplied worker without error", f.worker, err)
			}
		})
	}
	if worker.Calls() != 0 {
		t.Fatal("worker was dispatched during option validation")
	}
}

func TestNewTestCoordinatorWorkerDispatch(t *testing.T) {
	for _, name := range []string{"omitted", "scripted", "value"} {
		t.Run(name, func(t *testing.T) {
			worker := scriptWorker(textTurn("fixture dispatch succeeded"))
			var opts []coordinatorOption
			switch name {
			case "scripted":
				opts = append(opts, withWorker(worker))
			case "value":
				opts = append(opts, withWorker(struct{ fantasy.Agent }{worker}))
			}
			c := newTestCoordinator(t, opts...)
			if worker.Calls() != 0 {
				t.Fatal("worker was dispatched during fixture construction")
			}
			if name == "omitted" {
				if c.workerAgentOverride != nil {
					t.Fatal("omitting withWorker must leave the override unset")
				}
				return
			}
			todoID := addTodo(t, c, "fixture dispatch")
			output, err := c.executeTask(t.Context(), TaskDef{Agent: "worker", Goal: "fixture dispatch"}, todoID)
			if err != nil || output != "fixture dispatch succeeded" || worker.Calls() != 1 {
				t.Fatalf("dispatch = (%q, %v), calls = %d", output, err, worker.Calls())
			}
		})
	}
}

func TestNewTestCoordinatorRejectsInvalidWorker(t *testing.T) {
	const envKey = "HUFU_TEST_INVALID_FIXTURE_WORKER"
	if invalid := os.Getenv(envKey); invalid != "" {
		var worker fantasy.Agent
		if invalid == "typed-nil" {
			worker = (*scriptedWorker)(nil)
		}
		// An invalid option must fail immediately, even if a later option
		// would replace it. No dispatch is needed to diagnose the error.
		newTestCoordinator(t, withWorker(worker), withWorker(scriptWorker()))
		t.Fatal("invalid worker accepted")
	}
	for _, name := range []string{"nil", "typed-nil"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNewTestCoordinatorRejectsInvalidWorker$")
			cmd.Env = append(os.Environ(), envKey+"="+name)
			output, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("fixture subprocess = %v, want test failure; output:\n%s", err, output)
			}
			text := string(output)
			if !strings.Contains(text, errInvalidFixtureWorker.Error()) ||
				strings.Contains(text, "panic:") || strings.Contains(text, "invalid worker accepted") {
				t.Fatalf("want fixture validation failure before construction returns; output:\n%s", output)
			}
		})
	}
}
