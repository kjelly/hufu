package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
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
// A task without a decision profile takes the no-op path: after any durable
// occurrence lookup, no engine is run, no discipline is armed, and both tool
// hooks stay no-ops.
func (c *Coordinator) prepareTaskDecision(ctx context.Context, task TaskDef, todoID string) (func(), error) {
	noop := func() {}
	if c == nil || todoID == "" {
		return noop, nil
	}

	// A durable journal is required to resolve whether this task occurrence owns
	// an interrupted decision. When it exists, resume owns the occurrence and
	// must be checked before consulting any current profile, contract, evidence,
	// or policy: those inputs may have changed since the interrupted run and are
	// not authoritative for it.
	if c.hasDurableEventJournal() {
		admission, found, err := c.validateTaskOccurrenceAdmission(ctx, task, todoID, c.taskAttempt(todoID))
		if err != nil {
			return noop, err
		} else if found {
			if !admission.Enabled {
				return noop, nil
			}
			record, err := c.formTaskDecisionAdmitted(ctx, task, todoID, admission)
			if err != nil {
				return noop, err
			}
			if err := c.armDiscipline(ctx, todoID, task, *admission.Policy, record); err != nil {
				return noop, err
			}
			return func() { c.disarmDiscipline(todoID) }, nil
		}
		if record, policy, resumed, err := c.resumeTaskDecision(ctx, task, todoID); err != nil {
			return noop, err
		} else if resumed {
			if err := c.armDiscipline(ctx, todoID, task, policy, record); err != nil {
				return noop, err
			}
			return func() { c.disarmDiscipline(todoID) }, nil
		}
		// A persisted executable task may never re-resolve mutable profile
		// configuration. The only compatibility authority is a durable legacy
		// envelope handled immediately above.
		if c.todoItemByID(todoID) != nil {
			return noop, fmt.Errorf("durable task %s attempt %d has no decision admission", todoID, c.taskAttempt(todoID))
		}
	}

	resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), task)
	if err != nil {
		return noop, err
	}
	if !resolution.Enabled() {
		return noop, nil
	}
	if !c.hasDurableEventJournal() {
		return noop, fmt.Errorf("decision profile %q requires a durable event journal: event journal is unavailable", resolution.Profile)
	}
	if !c.decisionConfig().RequestContract.Enabled {
		return noop, fmt.Errorf("decision request contract is required when profile %q is enabled", resolution.Profile)
	}
	if err := ValidateTaskDecisionEvidence(task); err != nil {
		return noop, fmt.Errorf("decision task evidence: %w", err)
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

func (c *Coordinator) formTaskDecisionAdmitted(ctx context.Context, task TaskDef, todoID string, admission DecisionAdmission) (*DecisionRecord, error) {
	if admission.RequestContractRef == "" || admission.RequestContractArtifact.ID == "" {
		return nil, fmt.Errorf("decision admission for task %s has no request contract", todoID)
	}
	if admission.Policy == nil {
		return nil, fmt.Errorf("decision admission for task %s has no policy", todoID)
	}
	return c.formTaskDecisionWithAdmission(ctx, task, todoID, admission.Profile, *admission.Policy, &admission)
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
	return c.formTaskDecisionWithAdmission(ctx, task, todoID, profile, policy, nil)
}

func (c *Coordinator) formTaskDecisionWithAdmission(
	ctx context.Context,
	task TaskDef,
	todoID string,
	profile string,
	policy DecisionPolicy,
	admission *DecisionAdmission,
) (*DecisionRecord, error) {
	engine, err := c.newDecisionEngine(todoID)
	if err != nil {
		return nil, err
	}
	runners := newDecisionRunners(c, todoID)

	// A task may declare its options, or let the profile's proposal stage
	// produce them. Declaring neither is a configuration error (spec §19.1).
	if len(task.DecisionOptions) == 0 && !policy.OptionProposal.Enabled {
		return nil, fmt.Errorf("%s: task %s selects decision profile %q, which declares no decision-options and does not enable option-proposal",
			ReasonDecisionMissingAlternative, taskLabel(task, todoID), profile)
	}

	if !runners.available() {
		// Silently running with no judges is exactly what §34 forbids.
		return nil, fmt.Errorf("%s: decision profile %q needs a judge model and none is configured",
			ReasonDecisionBudgetInsufficient, profile)
	}

	var requestContract *RequestContract
	var requestContractRef string
	var requestContractRevision uint64
	var requestContractArtifact ArtifactRef
	if admission != nil {
		envelope, contractErr := loadRequestContract(ctx, c.decisionArtifactStore(), admission.RequestContractArtifact)
		if contractErr != nil {
			return nil, contractErr
		}
		requestContractArtifact = admission.RequestContractArtifact
		requestContract = ptrRequestContract(envelope.RequestContract())
		requestContractRef = admission.RequestContractRef
		requestContractRevision = admission.RequestContractRevision
		if requestContractRef != requestContractArtifact.ID || requestContractRevision != envelope.Revision {
			return nil, fmt.Errorf("decision admission request contract identity is invalid for task %s", todoID)
		}
	} else if contractConfig := c.decisionConfig().RequestContract; contractConfig.Enabled {
		envelope, contractErr := c.requestContractFor(ctx, contractConfig)
		if contractErr != nil {
			return nil, contractErr
		}
		requestContractArtifact = envelope.artifact
		requestContract = ptrRequestContract(envelope.envelope.RequestContract())
		requestContractRef = envelope.artifact.ID
		requestContractRevision = envelope.envelope.Revision
	}
	runID, decisionID := c.executionRunID, ""
	if admission != nil {
		runID, decisionID = admission.RunID, admission.DecisionID
	}

	record, err := engine.Run(ctx, DecisionRequest{
		RunID:          runID,
		TaskID:         todoID,
		Attempt:        c.taskAttempt(todoID),
		Profile:        profile,
		Policy:         policy,
		Question:       decisionQuestionFor(task),
		Options:        task.DecisionOptions,
		Facts:          cloneDecisionFacts(task.DecisionFacts),
		Artifacts:      append([]ArtifactRef(nil), task.DecisionArtifacts...),
		BaseRates:      cloneBaseRateEvidence(task.DecisionBaseRates),
		Assumptions:    task.DecisionAssumptions,
		Provenance:     cloneEvidenceProvenance(task.DecisionProvenance),
		Role:           "You are an independent reviewer on this team.",
		ProjectContext: c.decisionProjectContext(),
		Contract:       requestContract, RequireRequestContract: true, RequestContractRef: requestContractRef,
		RequestContractRevision: requestContractRevision, RequestContractArtifact: requestContractArtifact,
		AdmissionInputDigest: func() string {
			if admission != nil {
				return admission.TaskInputDigest
			}
			return ""
		}(),
		DecisionID: decisionID,
	})
	if err != nil {
		return nil, fmt.Errorf("task %s decision: %w", taskLabel(task, todoID), err)
	}
	c.report(c.newEvent("decision").withTodoID(todoID).withMessage(
		fmt.Sprintf("decision %s chose %q under profile %s", record.ID, record.FinalOption, profile)))
	return record, nil
}

func (c *Coordinator) newDecisionEngine(todoID string) (DecisionEngine, error) {
	index, err := c.decisionIndex()
	if err != nil {
		return nil, err
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return nil, err
	}
	runners := newDecisionRunners(c, todoID)
	return NewDecisionEngine(DecisionServices{
		Judges:               runners,
		CoordinatorFinalizer: runners,
		JudgeFinalizer:       runners,
		Challengers:          runners,
		Premortems:           runners,
		Revisions:            runners,
		Proposer:             runners,
		ReferenceEvidence:    runners,
		Journal:              journal,
		Store:                c.decisionArtifactStore(),
		Budget:               c.Budget(),
		Index:                index,
	}), nil
}

// resumeTaskDecision resolves and resumes the decision owned by this exact
// task occurrence. The returned policy is loaded from the immutable envelope,
// never from the current team configuration, so discipline is armed under the
// policy that admitted the interrupted run.
func (c *Coordinator) resumeTaskDecision(ctx context.Context, task TaskDef, todoID string) (*DecisionRecord, DecisionPolicy, bool, error) {
	decisionID, err := c.decisionForTaskOccurrence(ctx, todoID, c.taskAttempt(todoID))
	if err != nil {
		return nil, DecisionPolicy{}, false, err
	}
	if decisionID == "" {
		return nil, DecisionPolicy{}, false, nil
	}
	engine, err := c.newDecisionEngine(todoID)
	if err != nil {
		return nil, DecisionPolicy{}, false, err
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return nil, DecisionPolicy{}, false, err
	}
	state, err := projectDecision(ctx, journal, decisionID)
	if err != nil {
		return nil, DecisionPolicy{}, false, err
	}
	if state.EnvelopeRef.ID == "" {
		// Preserve the engine's typed legacy error for unfinished decisions. A
		// finalized legacy record is readable, but has no policy snapshot with
		// which it can safely arm execution discipline.
		record, resumeErr := engine.Resume(ctx, decisionID)
		if resumeErr != nil {
			return nil, DecisionPolicy{}, false, fmt.Errorf("task %s decision resume: %w", taskLabel(task, todoID), resumeErr)
		}
		return record, DecisionPolicy{}, true, fmt.Errorf("task %s decision resume: finalized legacy decision has no durable policy snapshot", taskLabel(task, todoID))
	}
	envelope, err := loadDecisionRunEnvelope(ctx, c.decisionArtifactStore(), state.EnvelopeRef)
	if err != nil {
		return nil, DecisionPolicy{}, false, fmt.Errorf("task %s decision resume policy: %w", taskLabel(task, todoID), err)
	}
	record, err := engine.Resume(ctx, decisionID)
	if err != nil {
		return nil, DecisionPolicy{}, false, fmt.Errorf("task %s decision resume: %w", taskLabel(task, todoID), err)
	}
	c.report(c.newEvent("decision").withTodoID(todoID).withMessage(
		fmt.Sprintf("decision %s resumed for profile %s", record.ID, record.Profile)))
	return record, envelope.Policy, true, nil
}

// decisionForTaskOccurrence finds the unique decision lifecycle bound to one
// immutable task occurrence. A missing match means this is a new occurrence;
// an ambiguous match is unsafe because selecting either decision could replay
// the wrong envelope.
func (c *Coordinator) decisionForTaskOccurrence(ctx context.Context, todoID string, attempt int) (string, error) {
	if c == nil || strings.TrimSpace(todoID) == "" || attempt < 1 {
		return "", fmt.Errorf("decision occurrence binding requires a task and positive attempt")
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return "", err
	}
	if journal == nil {
		return "", fmt.Errorf("decision occurrence binding: event journal is unavailable")
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return "", fmt.Errorf("decision occurrence binding: read event journal: %w", err)
	}
	candidates := map[string]struct{}{}
	anchored := false
	for _, event := range events {
		if event.Type != agent.EventDecisionRunEnvelopeAnchored || len(event.Payload) == 0 {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return "", fmt.Errorf("decision occurrence binding: decode %s event: %w", event.Type, err)
		}
		if payload.DecisionID == "" {
			continue
		}
		eventTaskID, eventAttempt := payload.TaskID, payload.Attempt
		if eventTaskID == "" {
			eventTaskID = event.TaskID
		}
		if eventAttempt == 0 {
			eventAttempt = event.Attempt
		}
		if eventTaskID != todoID {
			continue
		}
		if eventAttempt != attempt {
			continue
		}
		anchored = true
		candidates[payload.DecisionID] = struct{}{}
	}
	if anchored {
		if len(candidates) > 1 {
			return "", fmt.Errorf("decision occurrence binding is ambiguous for task %s attempt %d (%d anchored decisions)", todoID, attempt, len(candidates))
		}
		for decisionID := range candidates {
			return decisionID, nil
		}
	}

	runIDs := c.taskOccurrenceRunIDs(todoID, attempt)
	if len(runIDs) == 0 {
		if runID := strings.TrimSpace(c.executionRunID); runID != "" {
			runIDs[runID] = struct{}{}
		} else if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			if runID := strings.TrimSpace(c.taskTracker.TodoList().RunID()); runID != "" {
				runIDs[runID] = struct{}{}
			}
		}
	}
	if len(runIDs) == 0 {
		return "", nil
	}
	// Pre-envelope events remain readable for compatibility. They are only
	// considered when no anchored occurrence was found; an anchored envelope
	// is the authoritative owner when both forms are present.
	if !anchored {
		for _, event := range events {
			if !isDecisionLifecycleEvent(event.Type) || len(event.Payload) == 0 {
				continue
			}
			var payload decisionEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", fmt.Errorf("decision occurrence binding: decode %s event: %w", event.Type, err)
			}
			if payload.DecisionID == "" {
				continue
			}
			eventRunID, eventTaskID, eventAttempt := payload.RunID, payload.TaskID, payload.Attempt
			if eventRunID == "" {
				eventRunID = event.RunID
			}
			if eventTaskID == "" {
				eventTaskID = event.TaskID
			}
			if eventAttempt == 0 {
				eventAttempt = event.Attempt
			}
			if _, ok := runIDs[eventRunID]; !ok || eventTaskID != todoID {
				continue
			}
			// An unfinished legacy event may identify run/task but not attempt. It
			// still binds to this occurrence so Engine.Resume returns the typed
			// envelope-required error instead of silently forking a new decision.
			if eventAttempt != 0 && eventAttempt != attempt {
				continue
			}
			candidates[payload.DecisionID] = struct{}{}
		}
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("decision occurrence binding is ambiguous for task %s attempt %d across %d receipt runs (%d decisions)", todoID, attempt, len(runIDs), len(candidates))
	}
	for decisionID := range candidates {
		return decisionID, nil
	}
	return "", nil
}

// taskOccurrenceRunIDs returns every durable run identity recorded for the
// checkpointed task attempt. A recovery run can append another receipt with
// the same attempt, so selecting the newest receipt would hide the original
// run that owns an anchored decision envelope.
func (c *Coordinator) taskOccurrenceRunIDs(todoID string, attempt int) map[string]struct{} {
	runIDs := map[string]struct{}{}
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return runIDs
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != todoID {
			continue
		}
		for _, receipt := range item.ExecutionReceipts {
			if receipt.Attempt == attempt {
				if runID := strings.TrimSpace(receipt.RunID); runID != "" {
					runIDs[runID] = struct{}{}
				}
			}
		}
		if item.ExecutionReceipt != nil && item.ExecutionReceipt.Attempt == attempt {
			if runID := strings.TrimSpace(item.ExecutionReceipt.RunID); runID != "" {
				runIDs[runID] = struct{}{}
			}
		}
		break
	}
	return runIDs
}

func isDecisionLifecycleEvent(eventType string) bool {
	switch eventType {
	case agent.EventDecisionStarted,
		agent.EventRequestContractCommitted,
		agent.EventDecisionOptionsProposed,
		agent.EventDecisionEvidenceSealed,
		agent.EventDecisionEvidenceChanged,
		agent.EventDecisionRunEnvelopeAnchored,
		agent.EventDecisionOpinionSubmitted,
		agent.EventDecisionOpinionRejected,
		agent.EventDecisionAggregateComputed,
		agent.EventDecisionChallengeSubmitted,
		agent.EventDecisionChallengeSkipped,
		agent.EventDecisionRevisionSubmitted,
		agent.EventDecisionPremortemSubmitted,
		agent.EventDecisionBudgetDegraded,
		agent.EventDecisionEvidenceSharedOrigin,
		agent.EventDecisionFinalizationResult,
		agent.EventDecisionFinalized,
		agent.EventDecisionInvalidated,
		agent.EventDecisionReferenceStarted,
		agent.EventDecisionReferenceCompleted,
		agent.EventDecisionReferenceFailed:
		return true
	default:
		return false
	}
}

func ptrRequestContract(contract RequestContract) *RequestContract { return &contract }

type cachedRequestContract struct {
	envelope RequestContractEnvelope
	artifact ArtifactRef
}

func (c *Coordinator) requestContractFor(ctx context.Context, cfg agent.RequestContractConfig) (cachedRequestContract, error) {
	c.requestContractMu.Lock()
	defer c.requestContractMu.Unlock()
	input := c.requestContractInput
	if input == "" {
		input = strings.TrimSpace(c.initialPrompt)
	}
	if c.requestContract != nil && c.requestContractInput == input {
		return cachedRequestContract{envelope: *c.requestContract, artifact: c.requestContractArtifact}, nil
	}
	revision := c.requestContractRevision + 1
	if revision == 0 {
		revision = 1
	}
	envelope, data, err := BuildRequestContract(input, input, cfg, revision, c.disciplineNow())
	if err != nil {
		return cachedRequestContract{}, fmt.Errorf("build request contract: %w", err)
	}
	artifact, err := PersistRequestContract(ctx, c.decisionArtifactStore(), envelope, data)
	if err != nil {
		return cachedRequestContract{}, err
	}
	c.requestContract = &envelope
	c.requestContractRef = artifact.ID
	c.requestContractArtifact = artifact
	c.requestContractRevision = revision
	c.requestContractInput = input
	return cachedRequestContract{envelope: envelope, artifact: artifact}, nil
}

func (c *Coordinator) advanceRequestContractRevision(prompt string) {
	if c == nil || !c.decisionConfig().RequestContract.Enabled {
		return
	}
	input := strings.TrimSpace(c.initialPrompt)
	if strings.TrimSpace(prompt) != "" {
		input = strings.TrimSpace(input + "\n" + strings.TrimSpace(prompt))
	}
	c.requestContractMu.Lock()
	if c.requestContractInput != input {
		c.requestContract = nil
		c.requestContractInput = input
	}
	c.requestContractMu.Unlock()
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
	journal, err := c.decisionJournalFor()
	if err != nil {
		return nil, err
	}
	store := c.decisionArtifactStore()
	c.decisionControlPlaneMu.Lock()
	defer c.decisionControlPlaneMu.Unlock()
	control := c.decisionControlPlane
	if control != nil {
		return control.index, nil
	}
	if c.decisionIndexProjection != nil {
		c.decisionIndexProjection.SetEventJournal(journal)
		c.decisionIndexProjection.SetArtifactStore(store)
		return c.decisionIndexProjection, nil
	}
	index, err := OpenDecisionIndex(c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("decision index: %w", err)
	}
	index.SetJournal(journal)
	index.SetArtifactStore(store)
	c.decisionIndexProjection = index
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
	c.decisionControlPlaneMu.Lock()
	control := c.decisionControlPlane
	store := c.decisionStore
	c.decisionControlPlaneMu.Unlock()
	if control != nil {
		return control.artifactStore
	}
	if store != nil {
		return store
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil
	}
	c.decisionControlPlaneMu.Lock()
	if c.decisionStore == nil {
		c.decisionStore = store
	} else {
		store = c.decisionStore
	}
	c.decisionControlPlaneMu.Unlock()
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
