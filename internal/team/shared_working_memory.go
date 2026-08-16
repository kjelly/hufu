package team

// Shared working-memory reduction turns authoritative task transitions into
// typed session context. It deliberately does not infer durable knowledge
// from model prose; persistent extraction is handled separately after the
// completion gate accepts evidence.

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

type TaskResultMemoryInput struct {
	TodoID   string
	Agent    *agent.AgentDef
	Result   *TaskResult
	Output   string
	Verified bool
	Attempt  int
}

// VerificationFailureInput carries the authoritative failure transition for a
// task whose verification (or terminal execution) did not succeed. It is the
// failure counterpart of TaskResultMemoryInput and is reduced to a typed
// ContextError item so failed verification is retrievable/auditable as an
// error, never as generic progress.
type VerificationFailureInput struct {
	TodoID      string
	Agent       *agent.AgentDef
	Attempt     int
	Err         error
	Verify      *VerificationResult
	ReceiptIDs  []string
	ArtifactIDs []string
}

// reduceTaskResultToSharedMemory is called only after the canonical todo
// transition has reached done and verification has completed. Failure is
// best-effort like the prior STM write: it is observable and queued for
// repair, but never changes an already-verified task into a failed task.
func (c *Coordinator) reduceTaskResultToSharedMemory(ctx context.Context, input TaskResultMemoryInput) {
	if c == nil || c.contextRepo == nil || c.session == nil || c.historicalMemoryDisabled() {
		return
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	if runID == "" {
		runID = "run-unknown"
	}
	producer := "worker"
	if input.Agent != nil && strings.TrimSpace(input.Agent.Name) != "" {
		producer = strings.ToLower(input.Agent.Name)
	}
	metadata := map[string]string{
		"visibility":      "shared",
		"memory_lifetime": "session",
		"run_id":          runID,
		"task_id":         input.TodoID,
		"attempt":         fmt.Sprintf("%d", input.Attempt),
		"producer":        producer,
		"verified":        fmt.Sprintf("%t", input.Verified),
	}
	scope := c.contextScope()
	items := make([]contextstore.ContextItem, 0)
	// Every reduced item is bound to the task that produced it so provenance
	// survives deduplication and later extraction can trace the source.
	taskEvidence := []contextstore.EvidenceRef{{Type: "task", Ref: input.TodoID}}
	appendItem := func(kind contextstore.ContextKind, content string, evidence []contextstore.EvidenceRef) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		itemMetadata := make(map[string]string, len(metadata))
		for key, value := range metadata {
			itemMetadata[key] = value
		}
		items = append(items, contextstore.ContextItem{
			Kind: kind, Content: content, Scope: scope,
			Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
			Priority: contextstore.PriorityNormal, Confidence: 1,
			Source:   contextstore.SourceRef{Type: "task_result_reducer", Ref: runID + ":" + input.TodoID + ":" + fmt.Sprintf("%d", input.Attempt)},
			Evidence: evidence, Metadata: itemMetadata, Lifecycle: contextstore.LifecycleConfirmed,
		})
	}
	result := input.Result
	if result != nil {
		for _, finding := range result.Findings {
			content := strings.TrimSpace(finding.Summary)
			if detail := strings.TrimSpace(finding.Detail); detail != "" {
				content += "\n" + detail
			}
			appendItem(contextstore.ContextObservation, content, taskEvidence)
		}
		for _, decision := range result.Decisions {
			content := strings.TrimSpace(decision.Topic) + ": " + strings.TrimSpace(decision.Choice)
			if reason := strings.TrimSpace(decision.Reason); reason != "" {
				content += "\nReason: " + reason
			}
			appendItem(contextstore.ContextDecision, content, taskEvidence)
		}
		for _, question := range result.OpenQuestions {
			appendItem(contextstore.ContextOpenQuestion, question, taskEvidence)
		}
		for _, verification := range result.Verification {
			if isVerifySuccess(&verification) {
				content := strings.TrimSpace(verification.Command)
				if content == "" && verification.Spec != nil {
					content = string(verification.Spec.Type)
				}
				evidence := append([]contextstore.EvidenceRef(nil), taskEvidence...)
				if verification.Fingerprint != "" {
					evidence = append(evidence, contextstore.EvidenceRef{Type: "verification", Ref: verification.Fingerprint})
				}
				appendItem(contextstore.ContextVerification, content, evidence)
			}
		}
		// Artifacts are authoritative typed refs, not arbitrary filesystem
		// paths. Bind each to its artifact ID so traceability survives.
		for _, artifact := range result.Artifacts {
			content := strings.TrimSpace(artifact.Path)
			if content == "" {
				content = artifact.ID
			}
			evidence := append([]contextstore.EvidenceRef(nil), taskEvidence...)
			if artifact.ID != "" {
				evidence = append(evidence, contextstore.EvidenceRef{Type: "artifact", Ref: artifact.ID})
			}
			appendItem(contextstore.ContextArtifact, content, evidence)
		}
	}
	if len(items) == 0 && strings.TrimSpace(input.Output) != "" {
		appendItem(contextstore.ContextProgress, utils.TruncateRunes(input.Output, summaryMaxRunes), taskEvidence)
	}
	if len(items) == 0 {
		return
	}
	// The reducer appends by execution identity so two tasks reporting the same
	// finding keep distinct provenance instead of collapsing into one item.
	if err := c.contextRepo.AppendReducer(ctx, items...); err != nil {
		redacted := contextstore.RedactSecrets(err.Error())
		log.Printf("warning: shared working-memory reduction failed: %s", redacted)
		_ = c.emitEvent("shared_memory_reduce_error", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "error": redacted})
		for _, item := range items {
			if pendingErr := contextstore.AppendPendingWrite(c.contextPendingPath(), item, err); pendingErr != nil {
				log.Printf("warning: pending shared memory write failed: %s", contextstore.RedactSecrets(pendingErr.Error()))
			}
		}
		return
	}
	for _, item := range items {
		_ = c.emitEvent("shared_working_memory_saved", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "kind": item.Kind})
	}
	if err := c.rebuildLegacyContextProjections(ctx); err != nil {
		log.Printf("warning: shared working-memory projection rebuild failed: %v", err)
		_ = c.emitEvent("shared_memory_projection_error", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "error": contextstore.RedactSecrets(err.Error())})
	}
}

// recordVerificationFailure reduces an authoritative failed verification/task
// transition to a typed ContextError item with task plus verification/receipt
// evidence. It is invoked only after the canonical todo transition has reached
// a terminal error state, so the diagnostic is never stored as generic
// ContextProgress. A repository write failure is observable and queued for
// repair, but never changes the already-failed task's status.
func (c *Coordinator) recordVerificationFailure(ctx context.Context, input VerificationFailureInput) {
	if c == nil || c.contextRepo == nil || c.session == nil || c.historicalMemoryDisabled() {
		return
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	if runID == "" {
		runID = "run-unknown"
	}
	producer := "worker"
	if input.Agent != nil && strings.TrimSpace(input.Agent.Name) != "" {
		producer = strings.ToLower(input.Agent.Name)
	}
	metadata := map[string]string{
		"visibility":      "shared",
		"memory_lifetime": "session",
		"run_id":          runID,
		"task_id":         input.TodoID,
		"attempt":         fmt.Sprintf("%d", input.Attempt),
		"producer":        producer,
		"verified":        "false",
	}
	scope := c.contextScope()
	evidence := []contextstore.EvidenceRef{{Type: "task", Ref: input.TodoID}}
	if input.Verify != nil && input.Verify.Fingerprint != "" {
		evidence = append(evidence, contextstore.EvidenceRef{Type: "verification", Ref: input.Verify.Fingerprint})
	}
	for _, rid := range input.ReceiptIDs {
		if strings.TrimSpace(rid) != "" {
			evidence = append(evidence, contextstore.EvidenceRef{Type: "receipt", Ref: rid})
		}
	}
	for _, aid := range input.ArtifactIDs {
		if strings.TrimSpace(aid) != "" {
			evidence = append(evidence, contextstore.EvidenceRef{Type: "artifact", Ref: aid})
		}
	}
	content := strings.TrimSpace(fmt.Sprintf("task %s failed: %v", input.TodoID, input.Err))
	if input.Verify != nil {
		cmd := strings.TrimSpace(input.Verify.Command)
		if cmd == "" && input.Verify.Spec != nil {
			cmd = string(input.Verify.Spec.Type)
		}
		if cmd != "" {
			content += fmt.Sprintf("\nverification: %s (exit %d)", cmd, input.Verify.ExitCode)
		}
	}
	item := contextstore.ContextItem{
		Kind: contextstore.ContextError, Content: content, Scope: scope,
		Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
		Priority: contextstore.PriorityHigh, Confidence: 1,
		Source:   contextstore.SourceRef{Type: "verification_failure_reducer", Ref: runID + ":" + input.TodoID + ":" + fmt.Sprintf("%d", input.Attempt)},
		Evidence: evidence, Metadata: metadata, Lifecycle: contextstore.LifecycleConfirmed,
	}
	if err := c.contextRepo.AppendReducer(ctx, item); err != nil {
		redacted := contextstore.RedactSecrets(err.Error())
		log.Printf("warning: verification-failure reduction failed: %s", redacted)
		_ = c.emitEvent("shared_memory_reduce_error", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "error": redacted})
		if pendingErr := contextstore.AppendPendingWrite(c.contextPendingPath(), item, err); pendingErr != nil {
			log.Printf("warning: pending verification-failure write failed: %s", contextstore.RedactSecrets(pendingErr.Error()))
		}
		return
	}
	_ = c.emitEvent("shared_working_memory_saved", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "kind": item.Kind})
	if err := c.rebuildLegacyContextProjections(ctx); err != nil {
		log.Printf("warning: verification-failure projection rebuild failed: %v", err)
		_ = c.emitEvent("shared_memory_projection_error", "coordinator", input.TodoID, map[string]interface{}{"run_id": runID, "error": contextstore.RedactSecrets(err.Error())})
	}
}
