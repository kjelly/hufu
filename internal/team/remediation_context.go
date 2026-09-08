package team

import (
	"encoding/json"

	"github.com/kjelly/hufu/internal/utils"
)

// RemediationContext carries the canonical failure/result of the task whose
// on_failure back-edge most recently reset another task, so that task's next
// dispatch can address the concrete rejection instead of repeating work a
// downstream verifier/reviewer/final gate already showed was insufficient.
//
// It deliberately reuses the same durable, bounded, redacted evidence
// FailureEventPayload and TaskResult already carry (spec.md §9.3: "reuse
// existing event/failure/receipt structures where possible rather than
// adding a coding-only store") rather than a new evidence store.
type RemediationContext struct {
	SourceTaskID  string              `json:"source_task_id"`
	SourceAgent   string              `json:"source_agent"`
	SourceAttempt int                 `json:"source_attempt,omitempty"`
	FailureClass  TaskFailureClass    `json:"failure_class"`
	Status        string              `json:"status,omitempty"`
	Summary       string              `json:"summary,omitempty"`
	Findings      []Finding           `json:"findings,omitempty"`
	Verification  *VerificationResult `json:"verification,omitempty"`
}

const maxRemediationFindings = 12

// MarshalJSON makes direct serialization safe as well as the explicit
// durable/report paths, mirroring FailureEventPayload.MarshalJSON so no
// caller can forget to redact before persisting or displaying this evidence.
func (rc RemediationContext) MarshalJSON() ([]byte, error) {
	type wire RemediationContext
	return json.Marshal(wire(*redactedRemediationContext(&rc)))
}

// redactedRemediationContext returns a detached, bounded, secret-masked copy
// suitable for every durable or user-facing surface.
func redactedRemediationContext(rc *RemediationContext) *RemediationContext {
	if rc == nil {
		return nil
	}
	copyRC := cloneRemediationContext(rc)
	copyRC.SourceTaskID = utils.TruncateString(utils.RedactSecrets(copyRC.SourceTaskID), 200)
	copyRC.SourceAgent = utils.TruncateString(utils.RedactSecrets(copyRC.SourceAgent), 100)
	copyRC.Status = utils.TruncateString(utils.RedactSecrets(copyRC.Status), 80)
	copyRC.Summary = utils.TruncateString(utils.RedactSecrets(copyRC.Summary), 800)
	if len(copyRC.Findings) > maxRemediationFindings {
		copyRC.Findings = copyRC.Findings[:maxRemediationFindings]
	}
	for i, finding := range copyRC.Findings {
		finding.Category = utils.TruncateString(utils.RedactSecrets(finding.Category), 80)
		finding.Summary = utils.TruncateString(utils.RedactSecrets(finding.Summary), 300)
		finding.Detail = utils.TruncateString(utils.RedactSecrets(finding.Detail), 800)
		copyRC.Findings[i] = finding
	}
	if copyRC.Verification != nil {
		v := *copyRC.Verification
		v.Command = utils.TruncateString(utils.RedactSecrets(v.Command), 500)
		v.Stdout = utils.TruncateString(utils.RedactSecrets(v.Stdout), 1500)
		v.Stderr = utils.TruncateString(utils.RedactSecrets(v.Stderr), 1500)
		copyRC.Verification = &v
	}
	return copyRC
}

func cloneRemediationContext(rc *RemediationContext) *RemediationContext {
	if rc == nil {
		return nil
	}
	copyRC := *rc
	if len(rc.Findings) > 0 {
		copyRC.Findings = append([]Finding(nil), rc.Findings...)
	}
	if rc.Verification != nil {
		v := *rc.Verification
		copyRC.Verification = &v
	}
	return &copyRC
}

// buildRemediationContext derives a RemediationContext from the source
// task's own already-canonical, already-redacted evidence (its terminal
// FailureEvent, typed result, and verification result). Called only when the
// source's failure class is authorized to trigger the on_failure reset
// (dagScheduler.handleEvent), so a class this task's contract does not
// consider semantic never becomes remediation evidence for another task.
func buildRemediationContext(sourceTaskID string, source *TodoItem) *RemediationContext {
	if source == nil {
		return nil
	}
	rc := &RemediationContext{
		SourceTaskID:  sourceTaskID,
		SourceAgent:   source.Agent,
		SourceAttempt: source.Retries + 1,
	}
	if source.FailureEvent != nil {
		rc.FailureClass = source.FailureEvent.FailureClass
		rc.Summary = source.FailureEvent.Summary
	}
	if source.TypedResult != nil {
		if rc.Summary == "" {
			rc.Summary = source.TypedResult.Summary
		}
		rc.Status = source.TypedResult.Status
		rc.Findings = append([]Finding(nil), source.TypedResult.Findings...)
	}
	if source.VerifyResult != nil {
		v := *source.VerifyResult
		rc.Verification = &v
	}
	return redactedRemediationContext(rc)
}
