package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const auxiliaryPromptMaxRunes = 24000

func (c *Coordinator) prepareAuxiliaryPrompt(ctx context.Context, purpose, rawPrompt string) (string, error) {
	return c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, rawPrompt, true)
}

// prepareAuxiliaryProjectionPrompt compiles an auxiliary prompt for a
// transient projection. Compilation still uses the invocation-bound model
// context, but no manifest, session, event, or usage projection is persisted.
func (c *Coordinator) prepareAuxiliaryProjectionPrompt(ctx context.Context, purpose, rawPrompt string) (string, error) {
	return c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, rawPrompt, false)
}

func (c *Coordinator) prepareAuxiliaryPromptWithPersistence(ctx context.Context, purpose, rawPrompt string, persist bool) (string, error) {
	prompt, _, err := c.compileAuxiliaryPrompt(ctx, purpose, rawPrompt, persist)
	return prompt, err
}

// prepareAuxiliaryInvocation binds the exact compiled manifest to transient
// diagnostics. It leaves authorization, accounting and persisted fields alone.
func (c *Coordinator) prepareAuxiliaryInvocation(ctx context.Context, purpose, rawPrompt string) (context.Context, string, error) {
	return c.prepareAuxiliaryInvocationWithPersistence(ctx, purpose, rawPrompt, true)
}

func (c *Coordinator) prepareAuxiliaryProjectionInvocation(ctx context.Context, purpose, rawPrompt string) (context.Context, string, error) {
	return c.prepareAuxiliaryInvocationWithPersistence(ctx, purpose, rawPrompt, false)
}

func (c *Coordinator) prepareAuxiliaryInvocationWithPersistence(ctx context.Context, purpose, rawPrompt string, persist bool) (context.Context, string, error) {
	prompt, manifest, err := c.compileAuxiliaryPrompt(ctx, purpose, rawPrompt, persist)
	if err == nil && purpose == decisionPrimitivePurpose && agent.ProviderTransportObserverEnabled(ctx) {
		metadata, _ := invocationMetadataFromContext(ctx)
		metadata.Purpose, metadata.Trigger = purpose, ContextTriggerSidecarTask
		metadata.ParentRequestID, metadata.ParentManifestFingerprint = "", ""
		ctx = context.WithValue(ctx, contextAuxiliaryDiagnosticInvocationKey{}, &metadata)
	}
	if err == nil && manifest != nil && agent.ProviderTransportObserverEnabled(ctx) {
		metadata := &InvocationMetadata{
			RunID: manifest.RunID, TaskID: manifest.TaskID, Attempt: manifest.Attempt,
			AgentName: manifest.Agent, AgentRole: manifest.AgentRole, ModelExecutionID: manifest.ModelExecutionID,
			Phase: manifest.Phase, Trigger: manifest.Trigger, Purpose: manifest.Purpose,
			ParentRequestID: manifest.RequestID, ParentManifestFingerprint: manifest.Fingerprint,
		}
		ctx = context.WithValue(ctx, contextAuxiliaryDiagnosticInvocationKey{}, metadata)
	}
	return ctx, prompt, err
}

func (c *Coordinator) compileAuxiliaryPrompt(ctx context.Context, purpose, rawPrompt string, persist bool) (string, *ContextInjectionManifest, error) {
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	if _, err := contextPurposePolicy(purpose); err != nil {
		return "", nil, err
	}
	// A decision primitive's prompt is its request. The decision may depend
	// only on context the caller put in that request (decision-primitive.md
	// §7.2), so it reaches the model exactly as the backend built it.
	if purpose == decisionPrimitivePurpose {
		return rawPrompt, nil, nil
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	if attempt < 1 {
		attempt = 1
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	if agentName == "" {
		agentName = purpose
	}
	phase := PhaseExecute
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		phase = c.phaseWorkflow.State()
	}
	var parent InvocationMetadata
	if metadata, ok := invocationMetadataFromContext(ctx); ok {
		parent = metadata
		if metadata.TaskID != "" {
			todoID = metadata.TaskID
		}
		if metadata.Attempt > 0 {
			attempt = metadata.Attempt
		}
		if metadata.AgentName != "" {
			agentName = metadata.AgentName
		}
		if metadata.Phase != "" {
			phase = metadata.Phase
		}
	}
	request := c.newAuxiliaryContextRequest(todoID, attempt, agentName, phase, purpose, rawPrompt)
	if parent.Trigger != "" {
		request.ParentTrigger = parent.Trigger
		request.ParentRequestID = parent.ParentRequestID
		request.ParentManifestFingerprint = parent.ParentManifestFingerprint
		request.EnvironmentFingerprint = parent.EnvironmentFingerprint
		request.ModelExecutionID = contextModelExecutionID(request.TaskID, parent.ModelExecutionID, purpose)
	}
	request.AssignRequestID()
	// Auxiliary reviewers are intentionally isolated: they receive only their
	// purpose contract/candidate evidence, never the worker's STM/LTM or raw
	// transcript. The shared compiler still enforces budget and redaction.
	// An auxiliary call must compile against the model that will handle the
	// sidecar request. The sidecar invocation binder installs this binding
	// before this hook runs; do not resolve a second profile here.
	sidecarModelID := sidecar.ModelIDFromContext(ctx)
	invocation, hasBound := providerBoundInvocationContextFromContext(ctx, sidecarModelID)
	if !hasBound || !invocation.AdmissionContext.IsBound() {
		if sidecarModelID != "" {
			return "", nil, fmt.Errorf("provider-bound context unavailable for sidecar model %q", sidecarModelID)
		}
		return "", nil, fmt.Errorf("provider-bound context unavailable for auxiliary purpose %q", purpose)
	}
	modelSpec := invocation.ModelContext
	input := WorkerContextInput{Request: request, Goal: request.Goal, DisableMemory: true, DisableCanonicalMemory: true, ModelContext: modelSpec}
	// Result repair retains the admitted task's inputs, separately from the
	// bounded diagnostic prompt. These required blocks must never be truncated
	// with the transcript summary or replaced by a model's rejected submission.
	if purpose == "result_repair" || purpose == "protocol_repair" {
		if item := c.todoItemByID(todoID); item != nil {
			input.TaskDef = taskDefFromTodoItem(item)
			input.Constraints = input.TaskDef.Constraints
			input.RuntimeContext = "Admitted task goal:\n" + input.TaskDef.Goal + "\n" + c.taskFinalizationBinding(todoID, false)
			ref, compiledContract, err := c.boundResultContract(todoID)
			if err != nil {
				return "", nil, err
			}
			input.RuntimeContext += resultContractPromptSection(ref, compiledContract, false)
			input.DependencyResults = c.dependencyResultsForTask(todoID)
			if len(input.DependencyResults) != len(resultDependencyIDs(item)) {
				return "", nil, fmt.Errorf("result repair for task %s lacks completed dependency results", todoID)
			}
			if err := c.validateDependencyPayloads(input.DependencyResults); err != nil {
				return "", nil, err
			}
			plans, err := c.fixedCollectionPlans(todoID)
			if err != nil {
				return "", nil, err
			}
			collectionContext, err := resultFixedCollectionContext(plans)
			if err != nil {
				return "", nil, err
			}
			input.RuntimeContext += collectionContext
		}
	}
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, input)
	if err != nil {
		return "", nil, err
	}
	if err := validateRequiredEvidenceContext(input, compiled); err != nil {
		return "", nil, err
	}
	if persist || agent.ProviderTransportObserverEnabled(ctx) {
		manifest, err := BuildContextInjectionManifest(request, compiled, nil, purpose, time.Now().UTC(), c.session.Config.MemoryLearning)
		if err != nil {
			return "", nil, err
		}
		if persist {
			if err := c.persistContextManifest(&manifest, ctx); err != nil {
				return "", nil, err
			}
		}
		return compiled.Prompt, &manifest, nil
	}
	return compiled.Prompt, nil, nil
}

// newAuxiliaryContextRequest turns the sidecar's lightweight purpose label
// into the same typed request contract used by workers. The Sidecar hook only
// carries a prompt, so metadata unavailable at that boundary is represented by
// an opaque, redacted identity rather than fabricated prompt content.
func (c *Coordinator) newAuxiliaryContextRequest(todoID string, attempt int, agentName string, phase Phase, purpose, rawPrompt string) ContextRequest {
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	if agentName == "" {
		agentName = purpose
	}
	request := ContextRequest{
		SchemaVersion:    ContextRequestSchemaVersion,
		RunID:            c.contextRunID(),
		TaskID:           todoID,
		Attempt:          attempt,
		Goal:             utils.TruncateRunes(utils.RedactSecrets(rawPrompt), auxiliaryPromptMaxRunes),
		AgentName:        agentName,
		AgentRole:        "auxiliary",
		Phase:            phase,
		Purpose:          purpose,
		ActionType:       fmt.Sprintf("%s:%d", purpose, c.contextRequestSeq.Add(1)),
		ModelExecutionID: contextModelExecutionID(todoID, agentName, purpose),
	}
	policy, err := contextPurposePolicy(purpose)
	if err != nil {
		// Callers use prepareAuxiliaryPrompt, which rejects unsupported purpose
		// before reaching this constructor. Retain a valid deterministic request
		// for tests/helpers that construct a fallback directly.
		policy = ContextPurposePolicy{Trigger: ContextTriggerSidecarTask}
	}
	request.Trigger = policy.Trigger
	switch purpose {
	case "skill_matcher":
	case "guard_reviewer", "path_reviewer":
		request.Failure = &ContextFailure{ToolName: strings.TrimSuffix(purpose, "_reviewer"), ToolInputHash: hashContentKey(utils.RedactSecrets(rawPrompt))}
	case "plan_reviewer":
		request.VerificationCriteria = "review plan against task acceptance criteria"
	case "judge", "judge_graft_generation":
		request.CandidateIDs = []string{"bounded-candidate-set"}
		request.SelectionContract = "select the best candidate under the provided contract"
	case "skeptic":
		request.CandidateIDs = []string{"bounded-candidate"}
		request.VerificationCriteria = "challenge the candidate against the provided verification contract"
	case "reflection", "result_repair", "final_summary_repair", "protocol_repair":
		request.RecoveryDisposition = "approved_recovery_only"
		request.Failure = &ContextFailure{ErrorClass: "recovery", EvidenceRefs: []string{"opaque-recovery-evidence"}}
	}
	if request.TaskID == "" {
		switch request.Trigger {
		case ContextTriggerPlanReview, ContextTriggerJudge, ContextTriggerSkeptic, ContextTriggerRepair:
			// These contracts require a task identity. A global auxiliary call
			// has no Todo to attach, so use an opaque auxiliary identity and
			// persist it at coordinator scope (persistContextManifest checks
			// actual Todo ownership before choosing the projection).
			request.TaskID = "auxiliary-" + purpose
		}
	}
	request.AssignRequestID()
	return request
}

func (c *Coordinator) recordAuxiliaryFallback(ctx context.Context, purpose, outcome string) error {
	// Minimal in-memory coordinators used by deterministic helpers/tests have
	// no durable session boundary. Do not manufacture a relative runtime store;
	// real CLI coordinators always carry an explicit workspace.
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return nil
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	if attempt < 1 {
		attempt = 1
	}
	phase := PhaseExecute
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		phase = c.phaseWorkflow.State()
	}
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	policy, err := contextPurposePolicy(purpose)
	if err != nil {
		return err
	}
	if !policy.FallbackAllowed {
		return fmt.Errorf("context purpose %q requires model availability or a fail-closed caller", purpose)
	}
	if strings.TrimSpace(outcome) == "" {
		outcome = policy.FallbackOutcome
	}
	request := c.newAuxiliaryContextRequest(todoID, attempt, purpose, phase, purpose, "deterministic auxiliary fallback")
	if parent, ok := invocationMetadataFromContext(ctx); ok {
		request.ParentTrigger = parent.Trigger
		request.ParentRequestID = parent.ParentRequestID
		request.ParentManifestFingerprint = parent.ParentManifestFingerprint
		request.EnvironmentFingerprint = parent.EnvironmentFingerprint
		request.ModelExecutionID = contextModelExecutionID(request.TaskID, parent.ModelExecutionID, purpose)
	}
	request.ActionType = fmt.Sprintf("fallback:%s:%d", purpose, c.contextRequestSeq.Add(1))
	request.AssignRequestID()
	manifest, err := BuildContextInjectionManifest(request, CompiledContext{}, nil, purpose, time.Now().UTC(), c.session.Config.MemoryLearning)
	if err != nil {
		return err
	}
	manifest.ModelCalled = false
	manifest.Outcome = strings.TrimSpace(outcome)
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return c.persistContextManifest(&manifest)
}
