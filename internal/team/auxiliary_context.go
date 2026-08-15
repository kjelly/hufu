package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const auxiliaryPromptMaxRunes = 24000

func (c *Coordinator) prepareAuxiliaryPrompt(ctx context.Context, purpose, rawPrompt string) (string, error) {
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
	request := c.newAuxiliaryContextRequest(todoID, attempt, agentName, phase, purpose, rawPrompt)
	request.AssignRequestID()
	// Auxiliary reviewers are intentionally isolated: they receive only their
	// purpose contract/candidate evidence, never the worker's STM/LTM or raw
	// transcript. The shared compiler still enforces budget and redaction.
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, WorkerContextInput{Request: request, Goal: request.Goal, DisableMemory: true, ModelContext: globalRegistry.GetSpec("")})
	if err != nil {
		return "", err
	}
	manifest := BuildContextInjectionManifest(request, compiled, nil, purpose, time.Now().UTC())
	if err := c.persistContextManifest(&manifest); err != nil {
		return "", err
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
	switch purpose {
	case "skill_matcher":
		request.Trigger = ContextTriggerSkillMatch
	case "guard_reviewer", "path_reviewer":
		request.Trigger = ContextTriggerGuardReview
		request.Failure = &ContextFailure{ToolName: strings.TrimSuffix(purpose, "_reviewer"), ToolInputHash: hashContentKey(utils.RedactSecrets(rawPrompt))}
	case "plan_reviewer":
		request.Trigger = ContextTriggerPlanReview
		request.VerificationCriteria = "review plan against task acceptance criteria"
	case "judge":
		request.Trigger = ContextTriggerJudge
		request.CandidateIDs = []string{"bounded-candidate-set"}
		request.SelectionContract = "select the best candidate under the provided contract"
	case "skeptic":
		request.Trigger = ContextTriggerSkeptic
		request.CandidateIDs = []string{"bounded-candidate"}
		request.VerificationCriteria = "challenge the candidate against the provided verification contract"
	case "reflection", "result_repair", "final_summary_repair", "protocol_repair":
		request.Trigger = ContextTriggerRepair
		request.RecoveryDisposition = "approved_recovery_only"
		request.Failure = &ContextFailure{ErrorClass: "recovery", EvidenceRefs: []string{"opaque-recovery-evidence"}}
	default:
		request.Trigger = ContextTriggerSidecarTask
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
	request := c.newAuxiliaryContextRequest(todoID, attempt, purpose, phase, purpose, "deterministic auxiliary fallback")
	request.ActionType = fmt.Sprintf("fallback:%s:%d", purpose, c.contextRequestSeq.Add(1))
	request.AssignRequestID()
	manifest := BuildContextInjectionManifest(request, CompiledContext{}, nil, purpose, time.Now().UTC())
	manifest.ModelCalled = false
	manifest.Outcome = strings.TrimSpace(outcome)
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return c.persistContextManifest(&manifest)
}
