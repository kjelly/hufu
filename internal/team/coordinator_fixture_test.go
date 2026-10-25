package team

import (
	"context"
	"errors"
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

type coordinatorOption func(*coordinatorFixture)

// withWorker routes every worker dispatch to the given agent.
func withWorker(worker fantasy.Agent) coordinatorOption {
	return func(f *coordinatorFixture) { f.worker = worker }
}

// withMaxRetries sets the team-level retry budget.
func withMaxRetries(n int) coordinatorOption {
	return func(f *coordinatorFixture) { f.config.MaxRetries = n }
}

// withTeamConfig edits the team configuration before the session is built.
func withTeamConfig(edit func(*agent.TeamConfig)) coordinatorOption {
	return func(f *coordinatorFixture) { edit(&f.config) }
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
// three retries. The coordinator is closed during test cleanup.
func newTestCoordinator(t *testing.T, opts ...coordinatorOption) *Coordinator {
	t.Helper()
	f := coordinatorFixture{
		config: agent.TeamConfig{Name: "test", Timeout: 30, MaxRetries: 3},
		agents: map[string]*agent.AgentDef{"worker": testWorkerDef()},
		status: func(StatusEvent) {},
		runID:  "run-test",
	}
	for _, opt := range opts {
		opt(&f)
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
	if turn.text == "" {
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
