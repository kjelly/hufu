package team

import (
	"context"
	"fmt"
	"log"
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

	mu            sync.Mutex
	toolCalls     int
	failures      int
	attempt       int
	commitDone    bool
	stopped       bool
	staleMarked   bool
	checkpointErr string
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

	discipline := &taskDiscipline{
		todoID:    todoID,
		task:      task,
		stop:      stop,
		replan:    policy.Discipline.Replan,
		commit:    policy.Discipline.Commit,
		startedAt: c.disciplineNow(),
		attempt:   c.taskAttempt(todoID),
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
	if journal := c.decisionJournalOrNil(); journal != nil && discipline.decisionID != "" {
		if err := appendDecisionEvent(ctx, journal, agent.EventDecisionStarted, decisionEvent{
			DecisionID: discipline.decisionID,
			Reason: fmt.Sprintf("execution armed for task %s with %d kill criteria, checkpoint every %d tool calls",
				todoID, len(stop.KillCriteria), stop.CheckpointEvery),
		}); err != nil {
			return err
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
	if c == nil {
		return nil
	}
	journal := c.EventJournal()
	if journal == nil {
		return nil
	}
	return journal
}

// commitGateDenial evaluates the commit gate before a side-effecting tool
// starts. It returns a denial message, or an empty string when the tool may
// run. The gate is evaluated once per armed task: the prerequisites are
// properties of the task contract, not of an individual call.
func (c *Coordinator) commitGateDenial(ctx context.Context, todoID, toolName string) string {
	discipline := c.disciplineFor(todoID)
	if discipline == nil {
		return ""
	}
	discipline.mu.Lock()
	if discipline.commitDone {
		discipline.mu.Unlock()
		return ""
	}
	discipline.mu.Unlock()

	decision := EvaluateCommitGate(CommitGateInput{
		Task:   discipline.task,
		Policy: discipline.commit,
	})
	if !decision.Applicable {
		return ""
	}
	if decision.Allowed {
		discipline.mu.Lock()
		discipline.commitDone = true
		discipline.mu.Unlock()
		return ""
	}

	if journal := c.decisionJournalOrNil(); journal != nil {
		_ = appendDecisionEvent(ctx, journal, agent.EventCommitGateBlocked, decisionEvent{
			DecisionID:   discipline.decisionID,
			EvidenceHash: discipline.evidenceHash,
			Reason:       decision.Reason,
		})
	}
	return fmt.Sprintf("policy_blocked: tool %q was not started. %s", toolName, decision.Error())
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
	if contradicted := CriticalContradiction(assumptions); contradicted != "" {
		state.CriticalAssumptionContradicted = true
		state.ContradictedAssumptionID = contradicted
	}

	decision := EvaluateCheckpoint(stop, replan, state)
	if decision.Action == CheckpointContinue {
		return decision
	}

	discipline.mu.Lock()
	discipline.stopped = true
	discipline.mu.Unlock()

	if err := c.actOnCheckpoint(ctx, discipline, decision); err != nil {
		log.Printf("error: checkpoint persistence failed for task %s: %v", todoID, err)
		discipline.mu.Lock()
		discipline.checkpointErr = err.Error()
		discipline.mu.Unlock()
	}
	return decision
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
			DecisionID:   discipline.decisionID,
			EvidenceHash: discipline.evidenceHash,
			Reason:       fmt.Sprintf("%s: %s", decision.Reason, decision.Detail),
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
	return nil
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
