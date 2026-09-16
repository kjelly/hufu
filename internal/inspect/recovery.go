package inspect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

const recoveryPolicyRevisionDomain = "hufu-operator-recovery-policy-v1\x00"

// RecoveryEligibilityForTasks reuses the runtime's canonical repair decision
// engine without invoking callbacks or changing persisted state.
func RecoveryEligibilityForTasks(tasks []*team.TodoItem, events []IndexedEvent, runID string, interrupted bool) *operatorpkg.RecoveryEligibility {
	item := recoveryCandidate(tasks)
	return recoveryEligibilityForItem(item, events, runID, interrupted)
}

// RecoveryEligibilityForTask evaluates one already-bound task through the
// same read-only RepairController seam used by the overview selector.
func RecoveryEligibilityForTask(item *team.TodoItem, events []IndexedEvent, runID string, interrupted bool) *operatorpkg.RecoveryEligibility {
	return recoveryEligibilityForItem(item, events, runID, interrupted)
}

func recoveryEligibilityForItem(item *team.TodoItem, events []IndexedEvent, runID string, interrupted bool) *operatorpkg.RecoveryEligibility {
	if item == nil {
		return nil
	}
	policy, policyKnown := effectivePersistedRecoveryPolicy(item)
	allowsReplay := item.Execution.AllowsReplay
	decision := team.RepairDecision{Action: team.RepairBlock, Reason: "effective recovery policy is unavailable"}
	if policyKnown {
		decision = team.NewRepairController().Decide(team.RepairRequest{
			Task: team.TaskDef{
				ID: item.ID, MaxRetries: item.MaxRetries, Recovery: policy, SideEffect: item.SideEffect,
				ReconcileTool: item.ReconcileTool, Escalate: item.Escalate,
				Execution: team.ExecutionContract{AllowsReplay: allowsReplay}, Verify: item.Verify, VerifySpec: item.VerifySpec,
			},
			Attempt: item.Retries, MaxAttempts: item.MaxRetries, RecoveryState: item.RecoveryState,
			FailureDisposition: taskRetryDisposition(item),
		})
	} else if disposition := taskRetryDisposition(item); disposition != "" {
		decision = team.NewRepairController().Decide(team.RepairRequest{
			Task: team.TaskDef{
				ID: item.ID, MaxRetries: item.MaxRetries, Recovery: policy, SideEffect: item.SideEffect,
				ReconcileTool: item.ReconcileTool, Escalate: item.Escalate,
				Execution: team.ExecutionContract{AllowsReplay: allowsReplay}, Verify: item.Verify, VerifySpec: item.VerifySpec,
			},
			Attempt: item.Retries, MaxAttempts: item.MaxRetries, RecoveryState: item.RecoveryState,
			FailureDisposition: disposition,
		})
	}
	latest := latestTaskEvent(events, runID, item.ID)
	refs := taskRecoveryRefs(item, latest)
	policyRevision := recoveryPolicyRevision(item, policy)
	if !policyKnown {
		policyRevision = ""
	}
	expectedRevision := taskRecoveryRevision(item, latest, policyRevision)
	externalState := normalizedExternalEffectState(item)
	capabilityKnown := item.ReconcileTool != "" || item.Verify != "" || item.VerifySpec != nil
	policyDenied := item.FailureEvent != nil && item.FailureEvent.FailureClass == team.FailurePolicy
	if policyKnown && (policy == team.RecoveryManual || policy == team.RecoveryNever || decision.Action == team.RepairBlock) {
		policyDenied = true
	}
	effectivePolicy := string(policy)
	if !policyKnown {
		effectivePolicy = "unknown"
	}
	return &operatorpkg.RecoveryEligibility{
		TaskID: item.ID, Attempt: recoveryAttempt(item), TaskStatus: string(item.Status),
		ExpectedEventID: latest.Event.ID, ExpectedEventHash: latest.Event.Hash,
		ExternalEffectState: externalState, EffectivePolicy: effectivePolicy, PolicyRevision: policyRevision,
		ExpectedRevision: expectedRevision, PolicyDenied: policyDenied, ResumeEligible: interrupted || decision.Action == team.RepairReplan,
		ReconcileEligible: decision.Action == team.RepairReconcile && capabilityKnown,
		RetryEligible:     decision.Action == team.RepairRetry && retryStateProvenSafe(item, externalState),
		CapabilityKnown:   capabilityKnown, ReasonCode: repairReasonCode(decision.Action, policyDenied, policyKnown || taskRetryDisposition(item) != ""), SourceRefs: refs,
	}
}

func taskRetryDisposition(item *team.TodoItem) team.RetryDisposition {
	if item == nil || item.FailureEvent == nil {
		return ""
	}
	return item.FailureEvent.RetryDisposition
}

func retryStateProvenSafe(item *team.TodoItem, externalState string) bool {
	if item == nil {
		return false
	}
	if externalState == team.RecoveryStateNotStarted {
		return true
	}
	return item.SideEffect == team.SideEffectNone || item.SideEffect == team.SideEffectWorkspaceWrite
}

func SessionRecoveryRevision(session *team.SessionData, branchID string) (string, []string) {
	if session == nil || strings.TrimSpace(branchID) == "" {
		return "", []string{}
	}
	type sessionRevisionInput struct {
		BranchID              string                      `json:"branch_id"`
		RecoveryRequired      bool                        `json:"recovery_required"`
		PendingTerminalCommit *team.PendingTerminalCommit `json:"pending_terminal_commit"`
		PendingWrapUp         *team.PendingWrapUp         `json:"pending_wrap_up,omitempty"`
		WorkflowState         team.Phase                  `json:"workflow_state"`
		Tasks                 []sessionTaskRevision       `json:"tasks"`
	}
	tasks := make([]sessionTaskRevision, 0, len(session.Tasks))
	for _, item := range session.Tasks {
		if item == nil {
			continue
		}
		tasks = append(tasks, sessionTaskRevision{
			ID: item.ID, Status: item.Status, OccurrenceRevision: item.OccurrenceRevision,
			DispatchID: item.DispatchID, Retries: item.Retries, RecoveryState: item.RecoveryState,
		})
	}
	slices.SortFunc(tasks, func(left, right sessionTaskRevision) int { return strings.Compare(left.ID, right.ID) })
	input := sessionRevisionInput{
		BranchID: branchID, RecoveryRequired: session.RecoveryRequired,
		PendingTerminalCommit: session.PendingTerminalCommit, PendingWrapUp: session.PendingWrapUp,
		WorkflowState: session.WorkflowState, Tasks: tasks,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", []string{}
	}
	digest := sha256.Sum256(append([]byte("hufu-operator-session-recovery-v1\x00"), encoded...))
	refs := []string{branchID}
	if session.PendingTerminalCommit != nil {
		refs = appendRecoveryRefs(refs, session.PendingTerminalCommit.RunID)
	}
	if session.PendingWrapUp != nil {
		refs = appendRecoveryRefs(refs, session.PendingWrapUp.RunID)
	}
	slices.Sort(refs)
	return "sha256:" + hex.EncodeToString(digest[:]), slices.Compact(refs)
}

type sessionTaskRevision struct {
	ID                 string          `json:"id"`
	Status             team.TaskStatus `json:"status"`
	OccurrenceRevision int             `json:"occurrence_revision"`
	DispatchID         string          `json:"dispatch_id"`
	Retries            int             `json:"retries"`
	RecoveryState      string          `json:"recovery_state"`
}

func effectivePersistedRecoveryPolicy(item *team.TodoItem) (team.RecoveryPolicy, bool) {
	if item == nil || item.Recovery == "" {
		// An omitted durable policy can depend on an execution profile or
		// unattended mode unavailable in the task projection. Never infer a
		// mutation permission from an interactive default.
		return "", false
	}
	return item.Recovery, true
}

func recoveryCandidate(tasks []*team.TodoItem) *team.TodoItem {
	var selected *team.TodoItem
	selectedRank := int(^uint(0) >> 1)
	for index := 0; index < len(tasks); index++ {
		item := tasks[index]
		if item == nil {
			continue
		}
		switch item.Status {
		case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete, team.TaskPaused, team.TaskInProgress:
			rank := recoveryCandidateRank(item)
			if rank < selectedRank {
				selected, selectedRank = item, rank
			}
		}
	}
	return selected
}

func recoveryCandidateRank(item *team.TodoItem) int {
	if normalizedExternalEffectState(item) == team.RecoveryStateUnknown {
		return 0
	}
	if item.FailureEvent != nil && item.FailureEvent.FailureClass == team.FailurePolicy {
		return 1
	}
	if item.Recovery == team.RecoveryManual || item.Recovery == team.RecoveryNever {
		return 1
	}
	if item.Status == team.TaskPaused || item.Status == team.TaskInProgress {
		return 2
	}
	return 3
}

func normalizedExternalEffectState(item *team.TodoItem) string {
	if item == nil {
		return ""
	}
	switch item.SideEffect {
	case team.SideEffectExternalWrite, team.SideEffectInfraMutation, team.SideEffectCredential, team.SideEffectUnknown:
		switch item.RecoveryState {
		case team.RecoveryStateComplete, team.RecoveryStateNotStarted, team.RecoveryStatePartial:
			return item.RecoveryState
		default:
			return team.RecoveryStateUnknown
		}
	default:
		return item.RecoveryState
	}
}

func recoveryAttempt(item *team.TodoItem) int {
	if item == nil {
		return 0
	}
	attempt := item.Retries + 1
	for _, receipt := range item.ExecutionReceipts {
		if receipt.Attempt > attempt {
			attempt = receipt.Attempt
		}
	}
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.Attempt > attempt {
		attempt = item.ExecutionReceipt.Attempt
	}
	return attempt
}

func latestTaskEvent(events []IndexedEvent, runID, taskID string) IndexedEvent {
	var latest IndexedEvent
	for _, event := range events {
		if event.Event.RunID == runID && event.Event.TaskID == taskID && event.Ordinal >= latest.Ordinal {
			latest = event
		}
	}
	return latest
}

func taskRecoveryRefs(item *team.TodoItem, latest IndexedEvent) []string {
	refs := []string{}
	if latest.Event.ID != "" {
		refs = append(refs, latest.Event.ID)
	}
	if item.FailureEvent != nil {
		refs = appendRecoveryRefs(refs, item.FailureEvent.ReceiptID, item.FailureEvent.Fingerprint)
	}
	if receipt := latestReceipt(item); receipt != nil {
		refs = appendRecoveryRefs(refs, receipt.ActionInvocationID, receipt.RuntimeOutputsHash, receipt.TranscriptRef)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

func appendRecoveryRefs(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) != "" {
			values = append(values, candidate)
		}
	}
	return values
}

func latestReceipt(item *team.TodoItem) *team.ExecutionReceipt {
	if item == nil {
		return nil
	}
	var selected *team.ExecutionReceipt
	for index := range item.ExecutionReceipts {
		receipt := &item.ExecutionReceipts[index]
		if selected == nil || receipt.Attempt >= selected.Attempt {
			selected = receipt
		}
	}
	if item.ExecutionReceipt != nil && (selected == nil || item.ExecutionReceipt.Attempt >= selected.Attempt) {
		selected = item.ExecutionReceipt
	}
	return selected
}

func recoveryPolicyRevision(item *team.TodoItem, policy team.RecoveryPolicy) string {
	type policyInput struct {
		Policy       team.RecoveryPolicy  `json:"policy"`
		SideEffect   team.SideEffectClass `json:"side_effect"`
		AllowsReplay *bool                `json:"allows_replay"`
		Reconcile    bool                 `json:"reconcile"`
		Verify       bool                 `json:"verify"`
		MaxRetries   int                  `json:"max_retries"`
		ContractHash string               `json:"contract_hash"`
		ContractRev  int                  `json:"contract_revision"`
	}
	input := policyInput{
		Policy: policy, SideEffect: item.SideEffect, AllowsReplay: item.Execution.AllowsReplay,
		Reconcile: item.ReconcileTool != "", Verify: item.Verify != "" || item.VerifySpec != nil,
		MaxRetries: item.MaxRetries, ContractHash: item.ContractHash, ContractRev: item.ContractRevision,
	}
	encoded, _ := json.Marshal(input)
	digest := sha256.Sum256(append([]byte(recoveryPolicyRevisionDomain), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func taskRecoveryRevision(item *team.TodoItem, latest IndexedEvent, policyRevision string) string {
	type revisionInput struct {
		EventID            string `json:"event_id"`
		EventHash          string `json:"event_hash"`
		Attempt            int    `json:"attempt"`
		ReceiptFingerprint string `json:"receipt_fingerprint"`
		RecoveryState      string `json:"recovery_state"`
		PolicyRevision     string `json:"policy_revision"`
	}
	fingerprint := ""
	if item.FailureEvent != nil {
		fingerprint = firstNonEmpty(item.FailureEvent.Fingerprint, item.FailureEvent.ReceiptID)
	}
	if fingerprint == "" {
		if receipt := latestReceipt(item); receipt != nil {
			fingerprint = firstNonEmpty(receipt.RuntimeOutputsHash, receipt.ActionInvocationID, receipt.TranscriptRef)
		}
	}
	input := revisionInput{
		EventID: latest.Event.ID, EventHash: latest.Event.Hash, Attempt: recoveryAttempt(item),
		ReceiptFingerprint: fingerprint, RecoveryState: normalizedExternalEffectState(item), PolicyRevision: policyRevision,
	}
	if input.EventID == "" || input.EventHash == "" || input.Attempt <= 0 || input.ReceiptFingerprint == "" || input.PolicyRevision == "" {
		return ""
	}
	encoded, _ := json.Marshal(input)
	digest := sha256.Sum256(append([]byte("hufu-operator-task-recovery-v1\x00"), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func repairReasonCode(action team.RepairAction, denied, policyKnown bool) string {
	if !policyKnown {
		return "recovery_policy_unknown"
	}
	if denied {
		return "recovery_policy_denied"
	}
	if action == "" {
		return "recovery_unknown"
	}
	return fmt.Sprintf("recovery_%s", action)
}
