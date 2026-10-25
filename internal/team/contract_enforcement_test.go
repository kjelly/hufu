package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// These tests cover how the coordinator enforces the verifier contract on the
// execution path: which lint mode blocks dispatch, where contract_warning
// events are emitted and deduplicated, and how isolated extra-model clones
// share that state with their parent. The pure validation functions are
// covered by contract_preflight_test.go.

// contractWarningTask carries a structurally non-asserting verifier: the
// `|| echo` fallback always exits 0.
func contractWarningTask() TaskDef {
	return TaskDef{
		Agent:     "worker",
		Goal:      "do work",
		Verify:    "test -f artifact || echo FAIL",
		Execution: ExecutionContract{Kind: ExecutionKindInline},
	}
}

// contractWarningFindings are the warn-mode findings for contractWarningTask.
func contractWarningFindings() []ContractFinding {
	return ValidateExecutionContractFull(contractWarningTask(), agent.VerifierLintWarn).Findings
}

// contractWarningLog records contract_warning status events; it is safe for
// concurrent reporters.
type contractWarningLog struct {
	mu       sync.Mutex
	messages []string
}

func (l *contractWarningLog) record(e StatusEvent) {
	if e.Type != "contract_warning" {
		return
	}
	l.mu.Lock()
	l.messages = append(l.messages, e.Message)
	l.mu.Unlock()
}

func (l *contractWarningLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.messages)
}

func (l *contractWarningLog) first() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.messages) == 0 {
		return ""
	}
	return l.messages[0]
}

// contractLintCoordinator builds a bare coordinator in the given verifier lint
// mode. It has no agents, so ExecuteTasks fails at agent resolution after the
// contract preflight, which is what these tests observe.
func contractLintCoordinator(t *testing.T, mode string) (*Coordinator, *contractWarningLog) {
	t.Helper()
	log := &contractWarningLog{}
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.SetStatusReporter(log.record)
	c.session = &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:        "contract-lint",
			Reliability: agent.ReliabilityConfig{VerifierLintMode: mode},
		},
	}
	return c, log
}

func TestContractLintModeOnExecutionPath(t *testing.T) {
	structural := func(c *Coordinator) error { return c.validateContractStructural(contractWarningTask(), "1") }
	reported := func(c *Coordinator) error { return c.validateAndReportContract(contractWarningTask(), "") }
	tests := []struct {
		name            string
		mode            string
		check           func(*Coordinator) error
		wantErr         bool
		wantErrContains string
	}{
		{"structural check blocks in error mode", agent.VerifierLintError, structural, true, ""},
		{"reported check blocks in error mode", agent.VerifierLintError, reported, true, "verifier contract error"},
		{"reported check passes in off mode", agent.VerifierLintOff, reported, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, log := contractLintCoordinator(t, tt.mode)
			err := tt.check(c)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error=%v", err, tt.wantErr)
			}
			if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Errorf("err = %v, want it to mention %q", err, tt.wantErrContains)
			}
			if got := log.count(); got != 0 {
				t.Errorf("contract_warning events = %d, want 0 outside warn mode", got)
			}
		})
	}
}

func TestExecuteTasksWarnModeDefersContractWarningToExecuteTask(t *testing.T) {
	c, log := contractLintCoordinator(t, agent.VerifierLintWarn)
	// The preflight must neither reject the task nor emit the warning; the
	// per-task structural check in executeTask is the single emitter, which
	// keeps the warning from being duplicated across both paths.
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{contractWarningTask()})
	if err != nil && strings.Contains(err.Error(), "verifier contract error") {
		t.Fatalf("warn mode rejected the task at contract preflight: %v", err)
	}
	if got := log.count(); got != 0 {
		t.Errorf("contract_warning events at preflight = %d, want 0", got)
	}
}

func TestValidateContractStructuralDedupsWarningsPerTodo(t *testing.T) {
	c, log := contractLintCoordinator(t, agent.VerifierLintWarn)
	task := contractWarningTask()
	steps := []struct {
		todoID string
		want   int
	}{
		{"1", 1}, // first dispatch emits
		{"1", 1}, // same todo is deduplicated
		{"2", 2}, // another todo emits again
	}
	for i, step := range steps {
		if err := c.validateContractStructural(task, step.todoID); err != nil {
			t.Fatalf("step %d: warn mode must not block dispatch: %v", i, err)
		}
		if got := log.count(); got != step.want {
			t.Fatalf("step %d (todo %s): contract_warning events = %d, want %d", i, step.todoID, got, step.want)
		}
	}
	if first := log.first(); !strings.Contains(first, FindingVerifierNotAsserting) {
		t.Errorf("warning %q should name the finding code %s", first, FindingVerifierNotAsserting)
	}
}

func TestCloneCoordinatorSharesContractWarningDedup(t *testing.T) {
	c, log := contractLintCoordinator(t, agent.VerifierLintWarn)
	findings := contractWarningFindings()
	c.emitContractWarnings("1", findings)
	if got := log.count(); got != 1 {
		t.Fatalf("contract_warning events from parent = %d, want 1", got)
	}

	// An isolated extra-model clone must share the parent's dedup set, so
	// re-running the same todo through the clone does not emit again.
	clone := cloneCoordinator(c, c.session)
	if clone.contractWarnings == nil || clone.contractWarnings != c.contractWarnings {
		t.Fatalf("clone contractWarnings = %p, want the parent's %p", clone.contractWarnings, c.contractWarnings)
	}
	clone.emitContractWarnings("1", findings)
	if got := log.count(); got != 1 {
		t.Errorf("contract_warning events after clone re-emits todo 1 = %d, want 1", got)
	}
	clone.emitContractWarnings("2", findings)
	if got := log.count(); got != 2 {
		t.Errorf("contract_warning events after clone emits todo 2 = %d, want 2", got)
	}
}

// The three tests below are race tests: run them with -race. They refute the
// unsynchronized nil check that used to initialize contractWarnings.

func TestEmitContractWarningsConcurrentFirstUseInitializesOnce(t *testing.T) {
	c, log := contractLintCoordinator(t, agent.VerifierLintWarn)
	if c.contractWarnings != nil {
		t.Fatal("precondition: contractWarnings must be nil")
	}
	findings := contractWarningFindings()

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { c.emitContractWarnings(fmt.Sprintf("todo-%d", i), findings) })
	}
	wg.Wait()

	if got := log.count(); got != n {
		t.Errorf("contract_warning events = %d, want one per todo (%d)", got, n)
	}
	if c.contractWarnings == nil {
		t.Fatal("contractWarnings should be initialized after concurrent emissions")
	}
}

func TestEmitContractWarningsConcurrentSameTodoEmitsOnce(t *testing.T) {
	c, log := contractLintCoordinator(t, agent.VerifierLintWarn)
	findings := contractWarningFindings()

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			c.emitContractWarnings("same-todo", findings)
		})
	}
	close(start)
	wg.Wait()

	if got := log.count(); got != 1 {
		t.Errorf("contract_warning events for one todo under concurrency = %d, want 1", got)
	}
}

func TestCloneCoordinatorConcurrentClonesShareDedupSet(t *testing.T) {
	c, _ := contractLintCoordinator(t, agent.VerifierLintWarn)
	if c.contractWarnings != nil {
		t.Fatal("precondition: contractWarnings must be nil")
	}

	// Parallel executeSingleAgentWithModel goroutines clone a parent that has
	// never emitted; every clone must resolve the same shared set.
	const n = 30
	var wg sync.WaitGroup
	clones := make([]*Coordinator, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			clones[i] = cloneCoordinator(c, c.session)
		})
	}
	close(start)
	wg.Wait()

	if c.contractWarnings == nil {
		t.Fatal("parent contractWarnings should be initialized by cloneCoordinator")
	}
	for i, clone := range clones {
		if clone == nil || clone.contractWarnings != c.contractWarnings {
			t.Errorf("clone %d does not share the parent's contractWarnings", i)
		}
	}
}

func TestExecuteTaskContractCheckOnResume(t *testing.T) {
	// Crash-resume enters executeTask directly and bypasses the ExecuteTasks
	// preflight, so the contract check must run there too.
	tests := []struct {
		name         string
		mode         string
		wantErr      bool
		wantWarnings int
		wantTerminal bool
	}{
		{"error mode marks the resumed todo terminal", agent.VerifierLintError, true, 0, true},
		{"warn mode emits the warning before dispatch", agent.VerifierLintWarn, false, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log := &contractWarningLog{}
			c := newTestCoordinator(t,
				withWorker(scriptWorker(textTurn("done"))),
				withTeamConfig(func(cfg *agent.TeamConfig) {
					cfg.Reliability = agent.ReliabilityConfig{VerifierLintMode: tt.mode, HardEnforcement: true}
				}),
			)
			c.SetStatusReporter(log.record)
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
				Agent:     "worker",
				Desc:      "resume task",
				Kind:      TaskKindOutcome,
				Verify:    "test -f artifact || echo FAIL",
				Execution: ExecutionContract{Kind: ExecutionKindInline},
			}})[0]

			_, err := c.executeTask(context.Background(), taskDefFromTodoItem(item), item.ID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("executeTask err = %v, want error=%v", err, tt.wantErr)
			}
			if got := log.count(); got != tt.wantWarnings {
				t.Errorf("contract_warning events = %d, want %d", got, tt.wantWarnings)
			}
			resumed := todoByID(t, c, item.ID)
			// A contract-failed todo left pending or interrupted would be
			// re-driven forever by the next resume.
			if tt.wantTerminal && (resumed.Status == TaskPending || isInterruptedStatus(resumed.Status)) {
				t.Errorf("resumed task status = %q, want a terminal status", resumed.Status)
			}
		})
	}
}

func TestRecordContractFailurePersistsContractClass(t *testing.T) {
	c := newTestCoordinator(t)
	c.sessionData = NewSession()
	store, err := NewEventStore(c.session.Workspace, "run-test", "sess-test")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = store

	findings := []ContractFinding{{
		Severity: FindingSeverityError,
		Code:     FindingVerifierNotAsserting,
		Field:    "verify",
		Message:  "verifier uses `|| echo ...` as a terminal fallback which always exits 0",
	}}
	c.recordContractFailure(contractWarningTask(), "", findings)

	// The persisted detail starts with "source=contract", which the
	// classifier must map back to the contract class.
	detail := c.FailureDetail(errors.New("contract preflight failed: verify (verifier_not_asserting): verifier uses `|| echo ...`"), string(FailureContract))
	if got := classifyTaskFailure(errors.New(detail)); got != FailureContract {
		t.Errorf("classifyTaskFailure(%q) = %q, want %q", detail, got, FailureContract)
	}

	events, _ := store.ReadEvents()
	for _, e := range events {
		if e.Type != "failure_fingerprint" {
			continue
		}
		var payload struct {
			Fingerprint struct {
				Class string `json:"class"`
			} `json:"fingerprint"`
		}
		if json.Unmarshal(e.Payload, &payload) == nil && TaskFailureClass(payload.Fingerprint.Class) == FailureContract {
			return
		}
	}
	t.Errorf("no failure_fingerprint event with class %q was persisted", FailureContract)
}
