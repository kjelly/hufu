package team

import (
	"context"
	"fmt"
	"strings"
)

// Dispatch integration (docs/hufu-decision-aware-runtime-spec.md Phase 3.5).
//
// This is the wiring that makes `decision-profile` mean something. Without it
// the whole subsystem parses, validates and then does nothing.
//
// The default path is unchanged: a task with no profile, or the reserved "off"
// profile, never reaches any code below beyond one map lookup.

// SetDecisionProfile applies a run-scoped profile override, the top layer of
// the precedence chain in §8. It is set from --decision-profile.
func (c *Coordinator) SetDecisionProfile(profile string) {
	if c == nil {
		return
	}
	c.decisionProfileOverride = strings.TrimSpace(profile)
}

// DecisionProfileOverride returns the run-scoped override.
func (c *Coordinator) DecisionProfileOverride() string {
	if c == nil {
		return ""
	}
	return c.decisionProfileOverride
}

// decisionConfig returns the team's decision configuration.
func (c *Coordinator) decisionConfig() DecisionConfig {
	if c == nil || c.session == nil {
		return DecisionConfig{}
	}
	return c.session.Config.Decision
}

// prepareTaskDecision forms a decision for a task when its profile calls for
// one, then arms the execution discipline. It returns a cleanup function the
// caller must defer so the discipline is disarmed on every exit path.
//
// A task without a decision profile takes the fast exit: no engine, no arming,
// and both tool hooks stay no-ops.
func (c *Coordinator) prepareTaskDecision(ctx context.Context, task TaskDef, todoID string) (func(), error) {
	noop := func() {}
	if c == nil || todoID == "" {
		return noop, nil
	}

	resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), task)
	if err != nil {
		return noop, err
	}
	if !resolution.Enabled() {
		return noop, nil
	}
	policy, ok := DecisionPolicyFor(c.decisionConfig(), resolution.Profile)
	if !ok {
		return noop, fmt.Errorf("%s: decision profile %q resolved but has no policy",
			ReasonDecisionProfileUnknown, resolution.Profile)
	}

	record, err := c.formTaskDecision(ctx, task, todoID, resolution.Profile, policy)
	if err != nil {
		return noop, err
	}
	if err := c.armDiscipline(ctx, todoID, task, policy, record); err != nil {
		return noop, err
	}
	return func() { c.disarmDiscipline(todoID) }, nil
}

// formTaskDecision runs the decision engine for one task.
//
// Options come from the task contract, not from the model: an LLM proposing
// its own alternatives would make the no-go gate meaningless, since the same
// judgment under scrutiny would decide what counts as an alternative. A task
// that declares a profile but no options is a configuration error, caught here
// rather than producing an empty decision.
func (c *Coordinator) formTaskDecision(
	ctx context.Context,
	task TaskDef,
	todoID string,
	profile string,
	policy DecisionPolicy,
) (*DecisionRecord, error) {
	// A task may declare its options, or let the profile's proposal stage
	// produce them. Declaring neither is a configuration error (spec §19.1).
	if len(task.DecisionOptions) == 0 && !policy.OptionProposal.Enabled {
		return nil, fmt.Errorf("%s: task %s selects decision profile %q, which declares no decision-options and does not enable option-proposal",
			ReasonDecisionMissingAlternative, taskLabel(task, todoID), profile)
	}

	runners := newDecisionRunners(c, todoID)
	if !runners.available() {
		// Silently running with no judges is exactly what §34 forbids.
		return nil, fmt.Errorf("%s: decision profile %q needs a judge model and none is configured",
			ReasonDecisionBudgetInsufficient, profile)
	}

	index, err := c.decisionIndex()
	if err != nil {
		return nil, err
	}

	engine := NewDecisionEngine(DecisionServices{
		Judges:      runners,
		Challengers: runners,
		Premortems:  runners,
		Revisions:   runners,
		Proposer:    runners,
		Journal:     c.EventJournal(),
		Store:       c.decisionArtifactStore(),
		Budget:      c.Budget(),
		Index:       index,
	})

	record, err := engine.Run(ctx, DecisionRequest{
		RunID:          c.executionRunID,
		TaskID:         todoID,
		Profile:        profile,
		Policy:         policy,
		Question:       decisionQuestionFor(task),
		Options:        task.DecisionOptions,
		Assumptions:    task.DecisionAssumptions,
		Role:           "You are an independent reviewer on this team.",
		ProjectContext: c.decisionProjectContext(),
	})
	if err != nil {
		return nil, fmt.Errorf("task %s decision: %w", taskLabel(task, todoID), err)
	}
	c.report(c.newEvent("decision").withTodoID(todoID).withMessage(
		fmt.Sprintf("decision %s chose %q under profile %s", record.ID, record.FinalOption, profile)))
	return record, nil
}

// decisionQuestionFor renders the task's goal as the decision's question.
func decisionQuestionFor(task TaskDef) string {
	question := strings.TrimSpace(task.Goal)
	if constraints := strings.TrimSpace(task.Constraints); constraints != "" {
		question += "\nconstraints: " + constraints
	}
	return question
}

// taskLabel names a task for an error message.
func taskLabel(task TaskDef, todoID string) string {
	if id := strings.TrimSpace(task.ID); id != "" {
		return id
	}
	return todoID
}

// decisionProjectContext returns the declared project context judges may see
// under strict isolation. Sealed isolation drops it (§16).
func (c *Coordinator) decisionProjectContext() string {
	if c == nil || c.session == nil || !c.session.Config.ProjectContext {
		return ""
	}
	return c.cachedWorkerContext
}

// decisionIndex opens the workspace's cross-run index so a decision formed by
// this run can be found afterwards by `hufu decision resolve` (§49.2).
func (c *Coordinator) decisionIndex() (*DecisionIndex, error) {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return nil, nil
	}
	index, err := OpenDecisionIndex(c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("decision index: %w", err)
	}
	return index, nil
}

// DecisionIndexEntries returns the latest derived decision state for display.
// The index is presentation state only; lifecycle mutations still use the
// canonical event journal.
func (c *Coordinator) DecisionIndexEntries() ([]DecisionIndexEntry, error) {
	index, err := c.decisionIndex()
	if err != nil || index == nil {
		return nil, err
	}
	return index.List()
}

// decisionArtifactStore returns the workspace's content-addressed store so a
// DecisionRecord is persisted as evidence, not only as an event body.
func (c *Coordinator) decisionArtifactStore() ArtifactStore {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return nil
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil
	}
	return store
}

// ValidateDecisionProfiles checks a team's static task contracts and the
// run-scoped override at load time, so an unknown profile fails before any
// work starts rather than mid-run (§9).
func (c *Coordinator) ValidateDecisionProfiles(tasks []TaskDef) error {
	cfg := c.decisionConfig()
	if override := c.DecisionProfileOverride(); override != "" && !cfg.HasProfile(override) {
		return fmt.Errorf("%s: --decision-profile %q is not defined by this team",
			ReasonDecisionProfileUnknown, override)
	}
	return ValidateTaskDecisionProfiles(cfg, tasks)
}
