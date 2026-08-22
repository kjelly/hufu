package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// RepairAction is the machine-readable action proposed by diagnosis. It is
// deliberately narrower than the free-form coordinator/reflexion text.
type RepairAction string

const (
	RepairActionRetry     RepairAction = "retry"
	RepairActionEscalate  RepairAction = "escalate"
	RepairActionReconcile RepairAction = "reconcile"
	RepairActionReplan    RepairAction = "replan"
	RepairActionBlock     RepairAction = "block"
	RepairActionRollback  RepairAction = "rollback"
)

type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// CapabilityFinding is a bounded preflight/capability observation included in
// a diagnostic packet. It contains no prompt or unbounded command output.
type CapabilityFinding struct {
	Name      string    `json:"name"`
	Scope     string    `json:"scope,omitempty"`
	Available bool      `json:"available"`
	Reason    string    `json:"reason,omitempty"`
	Evidence  string    `json:"evidence,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

type BudgetSnapshot struct {
	MaxDurationSeconds int64 `json:"max_duration_seconds,omitempty"`
	MaxTokens          int64 `json:"max_tokens,omitempty"`
	TokensUsed         int64 `json:"tokens_used,omitempty"`
	Attempt            int   `json:"attempt,omitempty"`
	MaxAttempts        int   `json:"max_attempts,omitempty"`
	DiagnosticTasks    int   `json:"diagnostic_tasks,omitempty"`
	MaxDiagnosticTasks int   `json:"max_diagnostic_tasks,omitempty"`
}

// UnmarshalJSON keeps sessions written by older Hufu versions loadable. Those
// versions incorrectly redacted numeric telemetry such as tokens_used to the
// string "[REDACTED]"; treating that legacy marker as an unknown/zero value is
// safe and avoids discarding the entire session during a version upgrade.
func (b *BudgetSnapshot) UnmarshalJSON(data []byte) error {
	type budgetSnapshot BudgetSnapshot
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"max_tokens", "tokens_used"} {
		if raw, ok := fields[name]; ok {
			var marker string
			if err := json.Unmarshal(raw, &marker); err == nil && marker == "[REDACTED]" {
				fields[name] = json.RawMessage("0")
			}
		}
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var decoded budgetSnapshot
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*b = BudgetSnapshot(decoded)
	return nil
}

type RepairHypothesis struct {
	ID             string        `json:"id"`
	Cause          string        `json:"cause"`
	EvidenceRefs   []EvidenceRef `json:"evidence_refs,omitempty"`
	ProposedAction RepairAction  `json:"proposed_action"`
	ExpectedSignal string        `json:"expected_signal"`
	Risk           RiskLevel     `json:"risk"`
	Confidence     float64       `json:"confidence"`
	Source         string        `json:"source,omitempty"`
	Authoritative  bool          `json:"authoritative"`
}

// DiagnosticPacket is the immutable, replayable failure contract required by
// Phase 1. Textual errors are bounded evidence; disposition is selected by
// DiagnosisPolicy and never inferred by a later model from the text.
type DiagnosticPacket struct {
	ID                string              `json:"id"`
	RunID             string              `json:"run_id"`
	TaskID            string              `json:"task_id"`
	Attempt           int                 `json:"attempt"`
	PlanRevisionID    string              `json:"plan_revision_id,omitempty"`
	FailureClass      TaskFailureClass    `json:"failure_class"`
	Disposition       RetryDisposition    `json:"disposition"`
	Confidence        float64             `json:"confidence"`
	EvidenceRefs      []EvidenceRef       `json:"evidence_refs,omitempty"`
	VerifyResult      *VerificationResult `json:"verify_result,omitempty"`
	CapabilityFinding []CapabilityFinding `json:"capability_findings,omitempty"`
	EnvironmentDigest string              `json:"environment_digest,omitempty"`
	ArtifactDigests   []string            `json:"artifact_digests,omitempty"`
	SideEffect        SideEffectClass     `json:"side_effect,omitempty"`
	Recovery          RecoveryPolicy      `json:"recovery,omitempty"`
	BudgetSnapshot    BudgetSnapshot      `json:"budget_snapshot,omitempty"`
	Hypotheses        []RepairHypothesis  `json:"hypotheses,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	FailureSummary    string              `json:"failure_summary,omitempty"`
}

// DiagnosisInput combines deterministic failure signals with the existing
// recovery decision input. Sidecar availability is informational only; it can
// lower confidence but cannot override a deterministic safety disposition.
type DiagnosisInput struct {
	RecoveryDecisionInput
	FailureClass      TaskFailureClass
	TaskID            string
	RunID             string
	Attempt           int
	Detail            string
	VerifyResult      *VerificationResult
	EvidenceRefs      []EvidenceRef
	Capabilities      []CapabilityFinding
	Artifacts         []ArtifactRef
	SideEffect        SideEffectClass
	Recovery          RecoveryPolicy
	Budget            BudgetSnapshot
	SidecarAvailable  bool
	LocalHints        []string
	SidecarReflection string
}

type DiagnosisPolicy struct{}

func (DiagnosisPolicy) Diagnose(input DiagnosisInput) (DiagnosticPacket, error) {
	if input.RecoveryDecisionInput.FailureClass == "" {
		input.RecoveryDecisionInput.FailureClass = input.FailureClass
	}
	if input.FailureClass == "" {
		input.FailureClass = input.RecoveryDecisionInput.FailureClass
	}
	if input.TaskID == "" {
		return DiagnosticPacket{}, fmt.Errorf("diagnostic packet requires task id")
	}
	if input.FailureClass == "" {
		return DiagnosticPacket{}, fmt.Errorf("diagnostic packet requires failure class")
	}
	disposition, reason := DecideRecovery(input.RecoveryDecisionInput)
	// Permission/capability failures are not made safe by a replayable task or
	// a sidecar suggestion. They require an operator or an explicit capability
	// change, so force the conservative human-blocking disposition.
	if isPermissionBlockedFailureDetail(input.Detail) || hasUnavailableCapability(input.Capabilities) {
		disposition = NeedsHuman
		reason = "capability or permission is unavailable"
	}
	if input.FailureClass == FailureCancelled {
		disposition = RetryNone
		reason = "cancelled"
	}
	action := actionForDisposition(disposition)
	risk := riskForSideEffect(input.SideEffect)
	confidence := 1.0
	if !input.SidecarAvailable {
		// The local classifier remains authoritative, but the packet records
		// that no auxiliary hypothesis was available.
		confidence = 0.9
	}
	created := time.Now().UTC()
	packet := DiagnosticPacket{
		ID:                diagnosticPacketID(input.RunID, input.TaskID, input.Attempt, input.FailureClass, input.Detail, created),
		RunID:             input.RunID,
		TaskID:            input.TaskID,
		Attempt:           input.Attempt,
		FailureClass:      input.FailureClass,
		Disposition:       disposition,
		Confidence:        confidence,
		EvidenceRefs:      append([]EvidenceRef(nil), input.EvidenceRefs...),
		VerifyResult:      cloneVerificationResult(input.VerifyResult),
		CapabilityFinding: append([]CapabilityFinding(nil), input.Capabilities...),
		SideEffect:        input.SideEffect,
		Recovery:          input.Recovery,
		BudgetSnapshot:    input.Budget,
		CreatedAt:         created,
		FailureSummary:    utils.TruncateString(utils.RedactSecrets(input.Detail), 500),
	}
	for _, artifact := range input.Artifacts {
		if artifact.SHA256 != "" {
			packet.ArtifactDigests = append(packet.ArtifactDigests, artifact.SHA256)
		}
	}
	if packet.FailureSummary != "" {
		packet.EnvironmentDigest = digestText(packet.FailureSummary)
	}
	packet.Hypotheses = []RepairHypothesis{{
		ID:             packet.ID + ":hypothesis-1",
		Cause:          packet.FailureSummary,
		EvidenceRefs:   append([]EvidenceRef(nil), packet.EvidenceRefs...),
		ProposedAction: action,
		ExpectedSignal: reason,
		Risk:           risk,
		Confidence:     confidence,
		Source:         "deterministic-policy",
		Authoritative:  true,
	}}
	for _, hint := range append(append([]string(nil), input.LocalHints...), input.SidecarReflection) {
		hint = boundedDiagnosticText(hint, 500)
		if strings.TrimSpace(hint) == "" {
			continue
		}
		source := "local-hint"
		if hint == boundedDiagnosticText(input.SidecarReflection, 500) {
			source = "sidecar-reflection"
		}
		packet.Hypotheses = append(packet.Hypotheses, RepairHypothesis{
			ID:    packet.ID + fmt.Sprintf(":candidate-%d", len(packet.Hypotheses)),
			Cause: hint, EvidenceRefs: cloneEvidenceRefs(packet.EvidenceRefs),
			ProposedAction: action, ExpectedSignal: "candidate hint; deterministic policy remains authoritative",
			Risk: risk, Confidence: 0.5, Source: source, Authoritative: false,
		})
	}
	packet = normalizeDiagnosticPacket(packet)
	return packet, nil
}

func actionForDisposition(disposition RetryDisposition) RepairAction {
	switch disposition {
	case RetryWorker:
		return RepairActionRetry
	case ReconcileOnly:
		return RepairActionReconcile
	case ReplanRequired:
		return RepairActionReplan
	case NeedsHuman:
		return RepairActionBlock
	default:
		return RepairActionBlock
	}
}

func riskForSideEffect(effect SideEffectClass) RiskLevel {
	switch effect {
	case SideEffectCredential:
		return RiskCritical
	case SideEffectInfraMutation:
		return RiskHigh
	case SideEffectExternalWrite:
		return RiskHigh
	case SideEffectWorkspaceWrite:
		return RiskMedium
	default:
		return RiskLow
	}
}

func hasUnavailableCapability(findings []CapabilityFinding) bool {
	for _, finding := range findings {
		if !finding.Available && (strings.Contains(strings.ToLower(finding.Name), "permission") || strings.Contains(strings.ToLower(finding.Name), "capability")) {
			return true
		}
	}
	return false
}

func digestText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func diagnosticPacketID(runID, taskID string, attempt int, class TaskFailureClass, detail string, created time.Time) string {
	seed := fmt.Sprintf("%s|%s|%d|%s|%s|%d", runID, taskID, attempt, class, utils.RedactSecrets(detail), created.UnixNano())
	sum := sha256.Sum256([]byte(seed))
	return "diag-" + hex.EncodeToString(sum[:8])
}

func boundedDiagnosticText(value string, limit int) string {
	return utils.TruncateRunes(utils.RedactSecrets(value), limit)
}

func cloneEvidenceRefs(refs []EvidenceRef) []EvidenceRef {
	cloned := make([]EvidenceRef, len(refs))
	for i, ref := range refs {
		cloned[i] = ref
		cloned[i].Description = boundedDiagnosticText(ref.Description, 500)
		cloned[i].Value = boundedDiagnosticText(ref.Value, 500)
		cloned[i].SystemHMAC = boundedDiagnosticText(ref.SystemHMAC, 200)
	}
	return cloned
}

// normalizeDiagnosticPacket is the single boundary for every nested textual
// field, including verifier specs and future nested evidence fields.
func normalizeDiagnosticPacket(packet DiagnosticPacket) DiagnosticPacket {
	type wire DiagnosticPacket
	raw, err := json.Marshal(wire(packet))
	if err != nil {
		return packet
	}
	redacted, err := utils.RedactJSON(raw)
	if err != nil {
		return packet
	}
	var value any
	if json.Unmarshal(redacted, &value) != nil {
		return packet
	}
	boundDiagnosticJSON(value, 1000)
	bounded, err := json.Marshal(value)
	if err != nil {
		return packet
	}
	var normalized wire
	if json.Unmarshal(bounded, &normalized) != nil {
		return packet
	}
	return DiagnosticPacket(normalized)
}

func boundDiagnosticJSON(value any, limit int) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if text, ok := child.(string); ok {
				item[key] = boundedDiagnosticText(text, limit)
			} else {
				boundDiagnosticJSON(child, limit)
			}
		}
	case []any:
		for _, child := range item {
			boundDiagnosticJSON(child, limit)
		}
	}
}

func (p DiagnosticPacket) MarshalJSON() ([]byte, error) {
	type wire DiagnosticPacket
	return json.Marshal(wire(normalizeDiagnosticPacket(p)))
}

func (p DiagnosticPacket) IsTerminalSafetyDecision() bool {
	return p.Disposition == NeedsHuman || p.Disposition == ReconcileOnly || p.Disposition == ReplanRequired
}

func (c *Coordinator) recordDiagnosticPacket(item *TodoItem, class TaskFailureClass, disposition RetryDisposition, detail string, fingerprint FailureFingerprint, repeated, systemic bool) RetryDisposition {
	if c == nil || item == nil || item.ID == "" {
		return disposition
	}
	// PersistFailure may have mutated the canonical todo (for example by
	// appending a fingerprint) after it captured its initial snapshot. Refresh
	// here so diagnostic hints and receipts from the same attempt are included.
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, current := range c.taskTracker.TodoList().Items() {
			if current != nil && current.ID == item.ID {
				item = current
				break
			}
		}
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	resolvedRecovery := ResolveRecoveryPolicy(item.Recovery, item.SideEffect, c.unattended, c.ExecutionProfile())
	verify := item.VerifyResult
	evidence := []EvidenceRef{{TaskID: item.ID, RunID: runID, Type: "failure", Description: "bounded task failure evidence", Value: fingerprint.Digest}}
	if verify != nil {
		evidence = append(evidence, EvidenceRef{TaskID: item.ID, RunID: runID, Type: "verification", Description: "objective verifier result", Value: fmt.Sprintf("exit:%d", verify.ExitCode)})
	}
	var capabilities []CapabilityFinding
	if isPermissionBlockedFailureDetail(detail) {
		capabilities = append(capabilities, CapabilityFinding{Name: "permission", Available: false, Reason: utils.TruncateString(utils.RedactSecrets(detail), 300), CheckedAt: time.Now().UTC()})
	}
	if findings := environmentFindingsFromVerifyResult(verify); len(findings) > 0 {
		for _, finding := range findings {
			capabilities = append(capabilities, CapabilityFinding{Name: finding.Code, Scope: finding.Field, Available: false, Reason: finding.Message, Evidence: finding.Hint, CheckedAt: time.Now().UTC()})
		}
	}
	metrics := c.Metrics()
	limits := c.reliabilityConfig()
	decisionInput := RecoveryDecisionInput{
		FailureClass: class, SideEffect: item.SideEffect, RecoveryPolicy: resolvedRecovery,
		Attempt: item.Retries + 1, MaxRetries: item.MaxRetries + 1,
		EvidenceComplete:   item.ExecutionReceipt != nil || verify != nil || item.TypedResult != nil,
		FailureFingerprint: fingerprint.Digest, SameFailureRepeated: repeated,
		Replayable: CanAutomaticallyReplay(taskDefFromTodoItem(item)),
	}
	if systemic {
		decisionInput.SameFailureRepeated = true
	}
	var artifacts []ArtifactRef
	if item.TypedResult != nil {
		artifacts = item.TypedResult.Artifacts
	}
	var localHints []string
	var sidecarReflection string
	if item.RecoveryHypothesis != nil {
		localHints = append(localHints, item.RecoveryHypothesis.HypothesizedCause, item.RecoveryHypothesis.ProposedChange, item.RecoveryHypothesis.ExpectedChange)
	}
	if item.TypedResult != nil {
		sidecarReflection = item.TypedResult.RetryHint
	}
	if len(item.DiagnosticHints) > 0 {
		sidecarReflection = strings.Join(item.DiagnosticHints, "\n")
	}
	packet, err := (DiagnosisPolicy{}).Diagnose(DiagnosisInput{
		RecoveryDecisionInput: decisionInput, TaskID: item.ID, RunID: runID,
		Attempt: item.Retries + 1, Detail: detail, VerifyResult: verify,
		EvidenceRefs: evidence, Capabilities: capabilities, Artifacts: artifacts,
		SideEffect: item.SideEffect, Recovery: resolvedRecovery,
		Budget: BudgetSnapshot{MaxTokens: c.tokenBudget, TokensUsed: c.tokensUsed.Load(), Attempt: item.Retries + 1, MaxAttempts: item.MaxRetries + 1,
			DiagnosticTasks: metrics.DiagnosticTasksSinceProgress, MaxDiagnosticTasks: limits.MaxDiagnosticTasksWithoutProgress},
		SidecarAvailable: c.sidecarInst != nil,
		LocalHints:       localHints, SidecarReflection: sidecarReflection,
	})
	if err != nil {
		return disposition
	}
	// Compatibility callers may still provide a legacy disposition. Merge by
	// monotonic safety rank so human blocking can never be weakened.
	packet.Disposition = mergeDiagnosticDisposition(disposition, packet.Disposition)
	if len(packet.Hypotheses) > 0 {
		packet.Hypotheses[0].ProposedAction = actionForDisposition(packet.Disposition)
	}
	c.diagnosticPacketsMu.Lock()
	c.diagnosticPackets = append(c.diagnosticPackets, packet)
	c.diagnosticPacketsMu.Unlock()
	if c.sessionData != nil {
		c.sessionMu.Lock()
		c.sessionData.DiagnosticPackets = append(c.sessionData.DiagnosticPackets, packet)
		c.sessionMu.Unlock()
	}
	payload := map[string]interface{}{"packet": packet}
	if c.eventStore == nil {
		c.diagnosticPacketsMu.Lock()
		if c.pendingDiagnosticPackets == nil {
			c.pendingDiagnosticPackets = make(map[string]DiagnosticPacket)
		}
		c.pendingDiagnosticPackets[packet.ID] = packet
		c.diagnosticPacketsMu.Unlock()
	} else if err := c.emitEvent("diagnostic_packet", "coordinator", item.ID, payload); err != nil {
		c.diagnosticPacketsMu.Lock()
		if c.pendingDiagnosticPackets == nil {
			c.pendingDiagnosticPackets = make(map[string]DiagnosticPacket)
		}
		c.pendingDiagnosticPackets[packet.ID] = packet
		c.diagnosticPacketsMu.Unlock()
	}
	return packet.Disposition
}

func mergeDiagnosticDisposition(applied, diagnosed RetryDisposition) RetryDisposition {
	if applied == "" {
		return diagnosed
	}
	if diagnosed == "" {
		return applied
	}
	if dispositionSafetyRank(diagnosed) >= dispositionSafetyRank(applied) {
		return diagnosed
	}
	return applied
}

func dispositionSafetyRank(value RetryDisposition) int {
	switch value {
	case NeedsHuman:
		return 5
	case ReconcileOnly:
		return 4
	case ReplanRequired:
		return 3
	case RetryNone:
		return 2
	case RetryWorker:
		return 1
	default:
		return 0
	}
}

func (c *Coordinator) emitPendingDiagnosticPackets() {
	if c == nil || c.eventStore == nil {
		return
	}
	c.diagnosticPacketsMu.RLock()
	pending := make(map[string]DiagnosticPacket, len(c.pendingDiagnosticPackets))
	for id, packet := range c.pendingDiagnosticPackets {
		pending[id] = packet
	}
	c.diagnosticPacketsMu.RUnlock()
	for id, packet := range pending {
		if err := c.emitEvent("diagnostic_packet", "coordinator", packet.TaskID, map[string]interface{}{"packet": packet}); err != nil {
			continue
		}
		c.diagnosticPacketsMu.Lock()
		delete(c.pendingDiagnosticPackets, id)
		c.diagnosticPacketsMu.Unlock()
	}
}
