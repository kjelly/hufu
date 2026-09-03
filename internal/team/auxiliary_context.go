package team

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	if _, err := contextPurposePolicy(purpose); err != nil {
		return "", err
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
			return "", fmt.Errorf("provider-bound context unavailable for sidecar model %q", sidecarModelID)
		}
		return "", fmt.Errorf("provider-bound context unavailable for auxiliary purpose %q", purpose)
	}
	modelSpec := invocation.ModelContext
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, WorkerContextInput{Request: request, Goal: request.Goal, DisableMemory: true, ModelContext: modelSpec})
	if err != nil {
		return "", err
	}
	if persist {
		manifest := BuildContextInjectionManifest(request, compiled, nil, purpose, time.Now().UTC())
		if err := c.persistContextManifest(&manifest); err != nil {
			return "", err
		}
	}
	return compiled.Prompt, nil
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
	case "judge":
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
	// no durable session boundary. Do not manufacture a relative session.json;
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
	manifest := BuildContextInjectionManifest(request, CompiledContext{}, nil, purpose, time.Now().UTC())
	manifest.ModelCalled = false
	manifest.Outcome = strings.TrimSpace(outcome)
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return c.persistContextManifest(&manifest)
}
