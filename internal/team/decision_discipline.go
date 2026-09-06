package team

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Runtime mount for execution discipline
// (docs/hufu-decision-aware-runtime-spec.md §29-§32).
//
// Discipline is armed per task before EXECUTE and read at the tool boundary.
// When no discipline is armed — every task under the reserved "off" profile,
// which is the default — both hooks are no-ops, so a team that has not adopted
// decision profiles sees byte-identical behavior.

// taskDiscipline is one task's armed execution contract.
type taskDiscipline struct {
	todoID       string
	task         TaskDef
	stop         StopPolicy
	replan       ReplanPolicy
	commit       CommitGatePolicy
	decisionID   string
	evidenceHash string
	assumptions  []DecisionAssumption
	startedAt    time.Time

	mu        sync.Mutex
	toolCalls int
	failures  int
	attempt   int
	// sideEffectState carries the reconcile classification from an
	// interrupted earlier attempt. Empty during normal execution is correct:
	// there is no mutation in doubt (spec §29.1, §38.3).
	sideEffectState string
	// commitAllowed latches the gate verdict per tool, not per task. The
	// require-rollback prerequisite is a property of the invoked tool, so a
	// task that cleared the gate with one mutating tool has proved nothing
	// about the next one.
	commitAllowed map[string]bool
	stopped       bool
	staleMarked   bool
	checkpointErr string
}

// checkpointControlError is a runtime control signal, not a worker failure.
// It stops the dispatch that observed the checkpoint so its ordinary retry or
// failure path cannot overwrite the durable checkpoint projection.
type checkpointControlError struct{ outcome CheckpointOutcome }

const checkpointPersistenceFailed = "checkpoint_persistence_failed"

type checkpointPersistenceError struct{ cause string }

func (e checkpointPersistenceError) Error() string {
	return "checkpoint lifecycle persistence failed: " + e.cause
}

func (e checkpointControlError) Error() string {
	return fmt.Sprintf("checkpoint %s: %s", e.outcome.Action, e.outcome.Detail)
}

func asCheckpointControlError(err error) (CheckpointOutcome, bool) {
	var control checkpointControlError
	if !errors.As(err, &control) {
		return CheckpointOutcome{}, false
	}
	return control.outcome, true
}

// armDiscipline registers a task's stop and commit contract before execution.
// It fails closed when the profile requires kill criteria and the task declared
// none: stop conditions must exist before resources are spent, not after
// (spec §29.3).
func (c *Coordinator) armDiscipline(ctx context.Context, todoID string, task TaskDef, policy DecisionPolicy, record *DecisionRecord) error {
	stop := policy.Discipline.Stop
	if err := ValidateStopPolicyBeforeExecute(stop); err != nil {
		return err
	}
	if err := c.validateTaskToolRecovery(task); err != nil {
		return err
	}

	discipline := &taskDiscipline{
		todoID:    todoID,
		task:      task,
		stop:      stop,
		replan:    policy.Discipline.Replan,
		commit:    policy.Discipline.Commit,
		startedAt: c.disciplineNow(),
		attempt:   c.taskAttempt(todoID),
		// A task resuming after an interrupted mutation must not run on over a
		// side effect nobody can account for; the checkpoint stops on unknown.
		sideEffectState: c.taskRecoveryState(todoID),
	}
	if record != nil {
		discipline.decisionID = record.ID
		discipline.evidenceHash = record.EvidenceHash
		discipline.assumptions = append([]DecisionAssumption(nil), record.Assumptions...)
	}

	c.disciplineMu.Lock()
	if c.disciplines == nil {
		c.disciplines = map[string]*taskDiscipline{}
	}
	c.disciplines[todoID] = discipline
	c.disciplineMu.Unlock()

	// The stop contract is persisted before EXECUTE so the criteria a run was
	// stopped under can be read back exactly as they were armed.
	journal, err := c.decisionJournalFor()
	if err != nil {
		return err
	}
	if journal != nil && discipline.decisionID != "" {
		profile := ""
		if record != nil {
			profile = record.Profile
		}
		if err := appendDecisionEvent(ctx, journal, agent.EventDecisionStarted, decisionEvent{
			DecisionID:     discipline.decisionID,
			Profile:        profile,
			IdempotencyKey: decisionStageEventKey(discipline.decisionID, "execution_armed"),
			Reason: fmt.Sprintf("execution armed for task %s with %d kill criteria, checkpoint every %d tool calls",
				todoID, len(stop.KillCriteria), stop.CheckpointEvery),
		}); err != nil {
			return err
		}
	}
	return nil
}

// validateTaskToolRecovery rejects a rollback path the worker could not take.
// A compensate tool naming something the agent cannot invoke would satisfy
// require-rollback while leaving the mutation just as irreversible, so the
// contract is checked before execution rather than discovered after one.
func (c *Coordinator) validateTaskToolRecovery(task TaskDef) error {
	def, _, err := c.AgentPool().ResolveAgentName(task.Agent)
	if err != nil {
		// An unresolvable agent is rejected by admission; do not turn that
		// into a second, more confusing error here.
		return nil
	}
	declared := map[string]struct{}{}
	for name := range task.ToolRecovery {
		declared[normalizedToolName(name)] = struct{}{}
	}
	if def != nil {
		for name := range def.ToolRecovery {
			declared[normalizedToolName(name)] = struct{}{}
		}
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := resolveToolRecovery(def, task, name)
		if spec.CompensateTool == "" || agentCanInvoke(def, spec.CompensateTool) {
			continue
		}
		return &GateResult{
			Reason: ReasonCommitGateMissingRecovery,
			Detail: fmt.Sprintf("tool %q declares compensate tool %q, which agent %q cannot invoke",
				name, spec.CompensateTool, task.Agent),
		}
	}
	return nil
}

// disarmDiscipline releases a task's armed contract.
func (c *Coordinator) disarmDiscipline(todoID string) {
	c.disciplineMu.Lock()
	delete(c.disciplines, todoID)
	c.disciplineMu.Unlock()
}

// disciplineFor returns the armed contract for a task, or nil.
func (c *Coordinator) disciplineFor(todoID string) *taskDiscipline {
	if c == nil || todoID == "" {
		return nil
	}
	c.disciplineMu.Lock()
	defer c.disciplineMu.Unlock()
	return c.disciplines[todoID]
}

func (c *Coordinator) disciplineNow() time.Time { return time.Now() }

// taskRecoveryState returns the reconcile classification recorded for a task,
// or an empty string when nothing was interrupted.
func (c *Coordinator) taskRecoveryState(todoID string) string {
	if c == nil || c.taskTracker == nil || todoID == "" {
		return ""
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID {
			return item.RecoveryState
		}
	}
	return ""
}

// taskAttempt returns the task's current execution attempt, 1-based. Retries
// records DAG resets, so Retries+1 is the attempt count once a retry exists
// (the same convention anti_thrashing.go uses).
func (c *Coordinator) taskAttempt(todoID string) int {
	if c == nil || c.taskTracker == nil || todoID == "" {
		return 1
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID {
			return item.Retries + 1
		}
	}
	return 1
}

// decisionJournalOrNil returns the run's event journal when one is available.
func (c *Coordinator) decisionJournalOrNil() decisionJournal {
	journal, err := c.decisionJournalFor()
	if err != nil {
		return nil
	}
	return journal
}

// commitGateDenial evaluates the commit gate before a side-effecting tool
// starts. It returns a denial message, or an empty string when the tool may
// run.
//
// The gate is evaluated at the true mutation boundary. A read-only
// observation commits nothing and is never gated; gating the first call of
// any kind would have let a task clear the gate with `ls` and then mutate
// freely. Each mutating tool is evaluated with its own recovery contract, so
// clearing the gate for one tool authorizes only that tool (spec §30).
func (c *Coordinator) commitGateDenial(ctx context.Context, todoID, toolName, toolInput string) string {
	discipline := c.disciplineFor(todoID)
	if discipline == nil {
		return ""
	}
	if isReadOnlyToolCall(toolName, toolInput) {
		return ""
	}
	tool := normalizedToolName(toolName)
	discipline.mu.Lock()
	allowed := discipline.commitAllowed[tool]
	discipline.mu.Unlock()
	if allowed {
		return ""
	}

	decision := EvaluateCommitGate(CommitGateInput{
		Task:         discipline.task,
		Policy:       discipline.commit,
		ToolRecovery: c.toolRecoveryFor(discipline.task, toolName),
	})
	if !decision.Applicable {
		return ""
	}
	if decision.Allowed {
		discipline.mu.Lock()
		if discipline.commitAllowed == nil {
			discipline.commitAllowed = map[string]bool{}
		}
		discipline.commitAllowed[tool] = true
		discipline.mu.Unlock()
		return ""
	}

	if journal := c.decisionJournalOrNil(); journal != nil {
		_ = appendDecisionEvent(ctx, journal, agent.EventCommitGateBlocked, decisionEvent{
			DecisionID:     discipline.decisionID,
			EvidenceHash:   discipline.evidenceHash,
			Reason:         decision.Reason,
			IdempotencyKey: decisionStageEventKey(discipline.decisionID, "commit_gate_blocked", tool, decision.Reason),
		})
	}
	return fmt.Sprintf("policy_blocked: tool %q was not started. %s", toolName, decision.Error())
}

// commitGateActionDenial is the commit gate for execution paths that do not
// run through the fantasy tool boundary: static runtime actions and
// structured steps. They reach real providers and change real state, so the
// prerequisites that guard a worker's mutation guard theirs too.
//
// Unlike a worker tool call there is no per-call read-only classifier here.
// The caller decides what constitutes its mutation boundary — a declared
// mutate effect, or an action whose task class is guarded — and the task's
// side-effect class still decides whether the gate applies at all.
func (c *Coordinator) commitGateActionDenial(ctx context.Context, todoID string, task TaskDef, name string) string {
	discipline := c.disciplineFor(todoID)
	if discipline == nil {
		return ""
	}
	key := normalizedToolName(name)
	discipline.mu.Lock()
	allowed := discipline.commitAllowed[key]
	discipline.mu.Unlock()
	if allowed {
		return ""
	}

	decision := EvaluateCommitGate(CommitGateInput{
		Task:         discipline.task,
		Policy:       discipline.commit,
		ToolRecovery: c.toolRecoveryFor(task, name),
	})
	if !decision.Applicable {
		return ""
	}
	if decision.Allowed {
		discipline.mu.Lock()
		if discipline.commitAllowed == nil {
			discipline.commitAllowed = map[string]bool{}
		}
		discipline.commitAllowed[key] = true
		discipline.mu.Unlock()
		return ""
	}

	if journal := c.decisionJournalOrNil(); journal != nil {
		_ = appendDecisionEvent(ctx, journal, agent.EventCommitGateBlocked, decisionEvent{
			DecisionID:     discipline.decisionID,
			EvidenceHash:   discipline.evidenceHash,
			Reason:         decision.Reason,
			IdempotencyKey: decisionStageEventKey(discipline.decisionID, "commit_gate_blocked", key, decision.Reason),
		})
	}
	return fmt.Sprintf("policy_blocked: %q was not started. %s", name, decision.Error())
}

// toolRecoveryFor resolves the invoked tool's recovery contract against the
// task's agent definition.
func (c *Coordinator) toolRecoveryFor(task TaskDef, toolName string) ToolRecoverySpec {
	var def *agent.AgentDef
	if c != nil {
		def, _, _ = c.AgentPool().ResolveAgentName(task.Agent)
	}
	return resolveToolRecovery(def, task, toolName)
}

// recordToolCall counts a completed tool call and evaluates a checkpoint when
// one is due. The evaluation is deterministic and makes zero LLM calls
// (spec §29.1).
func (c *Coordinator) recordToolCall(ctx context.Context, todoID string, failed bool) CheckpointDecision {
	discipline := c.disciplineFor(todoID)
	if discipline == nil {
		return CheckpointDecision{Action: CheckpointContinue}
	}

	discipline.mu.Lock()
	discipline.toolCalls++
	if failed {
		discipline.failures++
	} else {
		discipline.failures = 0
	}
	toolCalls := discipline.toolCalls
	state := CheckpointState{
		Attempt:             discipline.attempt,
		ToolCalls:           discipline.toolCalls,
		ConsecutiveFailures: discipline.failures,
		Elapsed:             time.Since(discipline.startedAt),
	}
	// Turns without criterion progress is the run-wide stall signal the
	// existing detector already maintains; a checkpoint reads it rather than
	// keeping a second count of the same thing.
	state.NoProgressStreak = c.noProgressCounters().Turns
	stop := discipline.stop
	replan := discipline.replan
	assumptions := append([]DecisionAssumption(nil), discipline.assumptions...)
	discipline.mu.Unlock()

	if !ShouldCheckpoint(stop, toolCalls) {
		return CheckpointDecision{Action: CheckpointContinue}
	}

	if budget := c.Budget(); budget != nil {
		state.TokensUsed = budget.TokensUsed()
	}
	// The reconcile classification is read at the checkpoint, not frozen when
	// the task was armed: a reconciliation that lands mid-execution must be
	// what this checkpoint judges. An interrupted mutation whose outcome could
	// not be classified stops the task rather than letting it continue over
	// unknown state (spec §38.3).
	state.SideEffectState = discipline.sideEffectState
	if current := c.taskRecoveryState(todoID); current != "" {
		state.SideEffectState = current
	}
	if contradicted := CriticalContradiction(assumptions); contradicted != "" {
		state.CriticalAssumptionContradicted = true
		state.ContradictedAssumptionID = contradicted
	}
	state.MaterialEvidenceChanged = c.materialEvidenceChanged(ctx, discipline.decisionID, discipline.evidenceHash)

	decision := EvaluateCheckpoint(stop, replan, state)
	decision.State = state
	decision.DecisionID = discipline.decisionID
	decision.TaskID = todoID
	decision.Attempt = discipline.attempt
	decision.IdempotencyKey = decisionStageEventKey(discipline.decisionID, "checkpoint", fmt.Sprint(discipline.attempt), fmt.Sprint(toolCalls), decision.Action, decision.Reason)
	if decision.Action == CheckpointContinue {
		return decision
	}

	if err := c.actOnCheckpoint(ctx, discipline, decision); err != nil {
		log.Printf("error: checkpoint persistence failed for task %s: %v", todoID, err)
		discipline.mu.Lock()
		discipline.checkpointErr = err.Error()
		discipline.mu.Unlock()
		return CheckpointDecision{Action: checkpointPersistenceFailed, Detail: err.Error()}
	}
	discipline.mu.Lock()
	discipline.stopped = true
	discipline.mu.Unlock()
	return decision
}

// materialEvidenceChanged reads the decision journal's current sealed packet,
// which is the authoritative evidence identity for an armed execution. The
// TaskDef is intentionally not consulted: it is mutable scheduler input and
// cannot prove whether the decision evidence changed after dispatch.
func (c *Coordinator) materialEvidenceChanged(ctx context.Context, decisionID, armedHash string) bool {
	if strings.TrimSpace(decisionID) == "" || strings.TrimSpace(armedHash) == "" {
		return false
	}
	journal := c.decisionJournalOrNil()
	if journal == nil {
		return false
	}
	state, err := projectDecision(ctx, journal, decisionID)
	if err != nil || strings.TrimSpace(state.Packet.Hash) == "" {
		return false
	}
	return state.Packet.Hash != armedHash
}

// actOnCheckpoint gives a checkpoint verdict its durable consequences.
//
// A replan is recorded through RequestReplan so the reason a plan was
// abandoned survives in the log rather than only in a status message, and a
// decision invalidated by its own assumptions is marked stale — which appends
// a superseding row and never edits what was decided (spec §31, §35).
func (c *Coordinator) actOnCheckpoint(ctx context.Context, discipline *taskDiscipline, decision CheckpointDecision) error {
	journal := c.decisionJournalOrNil()
	if journal == nil || discipline.decisionID == "" {
		return fmt.Errorf("checkpoint journal is unavailable")
	}

	if decision.Action == CheckpointReplan {
		if err := RequestReplan(ctx, journal, discipline.decisionID, decision); err != nil {
			return fmt.Errorf("recording replan for decision %s: %w", discipline.decisionID, err)
		}
	} else {
		if err := appendDecisionEvent(ctx, journal, agent.EventKillCriterionTriggered, decisionEvent{
			DecisionID:     discipline.decisionID,
			EvidenceHash:   discipline.evidenceHash,
			Reason:         fmt.Sprintf("%s: %s", decision.Reason, decision.Detail),
			IdempotencyKey: decisionStageEventKey(discipline.decisionID, "kill_criterion_triggered", decision.Criterion, decision.Reason, decision.Detail),
		}); err != nil {
			return err
		}
	}

	// The decision's own inputs turned out not to hold, so the decision itself
	// is superseded, not merely this attempt.
	switch decision.Reason {
	case ReasonAssumptionInvalidated, ReasonDecisionStale:
		if err := c.markDecisionStale(ctx, journal, discipline, decision); err != nil {
			return err
		}
	}
	if err := c.projectCheckpointOutcome(discipline, decision); err != nil {
		return err
	}
	if decision.Action == CheckpointRequestInformation || decision.Action == CheckpointNeedsHuman {
		c.report(c.newEvent("needs_human").withTodoID(discipline.todoID).withMessage(decision.Detail))
	}
	c.report(c.newEvent("checkpoint").withTodoID(discipline.todoID).withMessage(fmt.Sprintf("checkpoint %s: %s", decision.Action, decision.Detail)).withData(map[string]any{
		"checkpoint": decision,
	}))
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	return nil
}

// projectCheckpointOutcome is the canonical Todo projection for a durable
// checkpoint verdict.  The decision event is written first by
// actOnCheckpoint; TodoList's onChange checkpoint then persists session state.
// It deliberately does not synthesize a worker result, so an invalidating
// checkpoint cannot appear as a successful task completion.
func (c *Coordinator) projectCheckpointOutcome(discipline *taskDiscipline, decision CheckpointDecision) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	detail := fmt.Sprintf("checkpoint %s: %s", decision.Action, decision.Detail)
	switch decision.Action {
	case CheckpointReplan:
		// Reset is the scheduler's controlled next occurrence.  A new attempt
		// will pass normal decision admission, so the stale decision is never
		// reused as authorization for the retry.
		return c.CommitTaskResetForRetry(context.Background(), discipline.todoID, detail)
	case CheckpointRequestInformation, CheckpointNeedsHuman:
		if decision.Request != "" {
			detail += ": " + decision.Request
		}
		return c.commitTaskTransitionFromCurrent(context.Background(), discipline.todoID, TaskPaused, detail, "", map[string]interface{}{
			"checkpoint_pause":  true,
			"checkpoint_action": decision.Action,
			"checkpoint_reason": decision.Reason,
		})
	case CheckpointStop:
		return c.commitTaskTransitionFromCurrent(context.Background(), discipline.todoID, TaskBlocked, detail, "", map[string]interface{}{
			"checkpoint_action":    decision.Action,
			"checkpoint_reason":    decision.Reason,
			"checkpoint_criterion": decision.Criterion,
		})
	case CheckpointEscalate:
		// Escalation is only meaningful where the task opted into the existing
		// retry escalation mechanism and a further model exists.  Budget is an
		// admission gate, never a reason to silently fall back to the old model.
		if exceeded, reason := c.budgetExceeded(); exceeded {
			return c.commitTaskTransitionFromCurrent(context.Background(), discipline.todoID, TaskBlocked, detail+"; escalation denied: "+reason, "", map[string]interface{}{
				"checkpoint_action": decision.Action, "checkpoint_reason": decision.Reason,
			})
		}
		task := discipline.task
		if current := todoItemByID(c.taskTracker.TodoList().Items(), discipline.todoID); current != nil {
			task.Model = current.Model
		}
		next := c.escalateTaskModelForRetry(task)
		if next == "" {
			return c.commitTaskTransitionFromCurrent(context.Background(), discipline.todoID, TaskBlocked, detail+"; escalation is not authorized", "", map[string]interface{}{
				"checkpoint_action": decision.Action, "checkpoint_reason": decision.Reason,
			})
		}
		return c.CommitTaskResetForRetry(context.Background(), discipline.todoID, detail+"; admitted for escalated retry", next)
	default:
		return fmt.Errorf("unsupported checkpoint action %q", decision.Action)
	}
}

// markDecisionStale supersedes the governing decision once. Marking is
// idempotent: a second checkpoint hitting the same condition does not append a
// second invalidation.
func (c *Coordinator) markDecisionStale(ctx context.Context, journal decisionJournal, discipline *taskDiscipline, decision CheckpointDecision) error {
	discipline.mu.Lock()
	if discipline.staleMarked {
		discipline.mu.Unlock()
		return nil
	}
	discipline.mu.Unlock()

	record := DecisionRecord{ID: discipline.decisionID, EvidenceHash: discipline.evidenceHash}
	if _, err := MarkDecisionStale(ctx, journal, record, fmt.Sprintf("%s: %s", decision.Reason, decision.Detail)); err != nil {
		return fmt.Errorf("marking decision %s stale: %w", discipline.decisionID, err)
	}
	discipline.mu.Lock()
	discipline.staleMarked = true
	discipline.mu.Unlock()
	return nil
}

// checkpointDenial renders a stopped task's tool denial. An already-stopped
// task refuses further tool calls rather than continuing on momentum.
func (c *Coordinator) checkpointDenial(todoID string) string {
	discipline := c.disciplineFor(todoID)
	if discipline == nil {
		return ""
	}
	discipline.mu.Lock()
	defer discipline.mu.Unlock()
	if !discipline.stopped {
		return ""
	}
	if discipline.checkpointErr != "" {
		return fmt.Sprintf("%s: checkpoint persistence failed; task is blocked pending recovery: %s", ReasonReconcileRequired, discipline.checkpointErr)
	}
	return fmt.Sprintf("%s: execution stopped at a checkpoint; no further tool calls are permitted for this task",
		ReasonKillCriterionReached)
}
