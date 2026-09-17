package team

import (
	"fmt"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

type DecisionBindingPromptAdmission struct {
	BindingID          string
	EvidenceHash       string
	Prompt             []byte
	CountedInputTokens uint64
	CountMethod        string
	ContextWindow      uint64
	MaxOutputTokens    uint64
	SafetyMarginTokens uint64
	ToolIDs            []string
}

type DecisionContextAdmissionReceiptV1 struct {
	SchemaVersion  int                                `json:"schema_version"`
	Kind           string                             `json:"kind"`
	EvidenceHash   string                             `json:"evidence_hash"`
	BindingResults []DecisionBindingAdmissionResultV1 `json:"binding_results"`
	Digest         string                             `json:"digest"`
}

type DecisionBindingAdmissionResultV1 struct {
	BindingID          string `json:"binding_id"`
	PromptBytes        uint64 `json:"prompt_bytes"`
	CountedInputTokens uint64 `json:"counted_input_tokens"`
	ContextWindow      uint64 `json:"context_window"`
	MaxOutputTokens    uint64 `json:"max_output_tokens"`
	SafetyMarginTokens uint64 `json:"safety_margin_tokens"`
	CountMethod        string `json:"count_method"`
}

// AdmitPrimaryDecisionContexts verifies the complete prompt for every planned
// binding against one sealed evidence hash. Unknown capacity fails closed.
func AdmitPrimaryDecisionContexts(plan *DecisionRoleBindingPlanV1, evidenceHash string, prompts []DecisionBindingPromptAdmission) (*DecisionContextAdmissionReceiptV1, error) {
	if err := ValidateDecisionRoleBindingPlan(plan); err != nil {
		return nil, err
	}
	if !validDecisionDigest(evidenceHash) {
		return nil, fmt.Errorf("%w: invalid evidence hash", ErrDecisionContextAdmission)
	}
	byID := make(map[string]DecisionBindingPromptAdmission, len(prompts))
	for _, prompt := range prompts {
		if _, duplicate := byID[prompt.BindingID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate prompt for %s", ErrDecisionContextAdmission, prompt.BindingID)
		}
		byID[prompt.BindingID] = prompt
	}
	results := make([]DecisionBindingAdmissionResultV1, 0, len(plan.Bindings))
	for _, binding := range plan.Bindings {
		prompt, ok := byID[binding.BindingID]
		if !ok {
			return nil, fmt.Errorf("%w: missing prompt for %s", ErrDecisionContextAdmission, binding.BindingID)
		}
		if prompt.EvidenceHash != evidenceHash || prompt.ContextWindow == 0 || prompt.MaxOutputTokens == 0 || prompt.CountedInputTokens == 0 {
			return nil, fmt.Errorf("%w: incomplete admission for %s", ErrDecisionContextAdmission, binding.BindingID)
		}
		if prompt.CountMethod != "pinned_tokenizer" && prompt.CountMethod != "utf8_byte_upper_budget" {
			return nil, fmt.Errorf("%w: invalid count method for %s", ErrDecisionContextAdmission, binding.BindingID)
		}
		if prompt.CountMethod == "utf8_byte_upper_budget" && prompt.CountedInputTokens < uint64(len(prompt.Prompt)) {
			return nil, fmt.Errorf("%w: byte budget undercounts prompt for %s", ErrDecisionContextAdmission, binding.BindingID)
		}
		if prompt.CountedInputTokens > prompt.ContextWindow || prompt.MaxOutputTokens > prompt.ContextWindow-prompt.CountedInputTokens || prompt.SafetyMarginTokens > prompt.ContextWindow-prompt.CountedInputTokens-prompt.MaxOutputTokens {
			return nil, fmt.Errorf("%w: prompt for %s does not fit context window", ErrDecisionContextAdmission, binding.BindingID)
		}
		if binding.ToolPolicy == "none" && len(prompt.ToolIDs) > 0 || binding.ToolPolicy == "authorized-read-only" && !sameSortedStrings(prompt.ToolIDs, binding.AllowedToolIDs) {
			return nil, fmt.Errorf("%w: prompt tools differ from binding %s", ErrDecisionContextAdmission, binding.BindingID)
		}
		results = append(results, DecisionBindingAdmissionResultV1{
			BindingID: binding.BindingID, PromptBytes: uint64(len(prompt.Prompt)), CountedInputTokens: prompt.CountedInputTokens,
			ContextWindow: prompt.ContextWindow, MaxOutputTokens: prompt.MaxOutputTokens, SafetyMarginTokens: prompt.SafetyMarginTokens,
			CountMethod: prompt.CountMethod,
		})
		delete(byID, binding.BindingID)
	}
	if len(byID) > 0 {
		return nil, fmt.Errorf("%w: prompts include unknown bindings", ErrDecisionContextAdmission)
	}
	digest, err := DecisionContractDigest("hufu/decision-context-admission/v1", map[string]any{"evidence_hash": evidenceHash, "results": results})
	if err != nil {
		return nil, err
	}
	return &DecisionContextAdmissionReceiptV1{SchemaVersion: 1, Kind: "decision_context_admission", EvidenceHash: evidenceHash, BindingResults: results, Digest: digest}, nil
}

type PrimaryDecisionBudgetReservation struct {
	Tokens          uint64
	ActiveDuration  time.Duration
	ledger          *budgetLedger
	stepReservation tokenStepReservation
	settled         bool
}

// ReservePrimaryDecisionBudget reserves a generation before any provider call.
// Both the V2 bundle cap and the shared run ledger must admit the request.
func ReservePrimaryDecisionBudget(ledger *budgetLedger, bundle agent.DecisionProfileBundleV2, logicalTokensUsed, requestedTokens uint64, activeDurationUsed, requestedDuration time.Duration) (*PrimaryDecisionBudgetReservation, error) {
	if ledger == nil || requestedTokens == 0 || requestedDuration < 0 || activeDurationUsed < 0 {
		return nil, fmt.Errorf("%w: invalid reservation request", ErrDecisionBudgetReservation)
	}
	if logicalTokensUsed > bundle.Limits.TotalDecisionTokens || requestedTokens > bundle.Limits.TotalDecisionTokens-logicalTokensUsed {
		return nil, fmt.Errorf("%w: decision token limit", ErrDecisionBudgetReservation)
	}
	maxDuration := time.Duration(bundle.Limits.ActiveDecisionDurationMS) * time.Millisecond
	if activeDurationUsed > maxDuration || requestedDuration > maxDuration-activeDurationUsed {
		return nil, fmt.Errorf("%w: active decision duration limit", ErrDecisionBudgetReservation)
	}
	if requestedTokens > ^uint64(0)>>1 {
		return nil, fmt.Errorf("%w: token reservation exceeds int64", ErrDecisionBudgetReservation)
	}
	reservation, err := ledger.reserve(int64(requestedTokens))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecisionBudgetReservation, err)
	}
	return &PrimaryDecisionBudgetReservation{Tokens: requestedTokens, ActiveDuration: requestedDuration, ledger: ledger, stepReservation: reservation}, nil
}

// Settle releases the run-wide reservation and charges observed usage once.
// Unknown usage retains the full reservation as required by all V2 bundles.
func (reservation *PrimaryDecisionBudgetReservation) Settle(observedTokens *uint64) bool {
	if reservation == nil || reservation.ledger == nil || reservation.settled {
		return false
	}
	charged := reservation.Tokens
	if observedTokens != nil {
		charged = *observedTokens
	}
	if charged > ^uint64(0)>>1 {
		charged = reservation.Tokens
	}
	settled := reservation.ledger.commit(&reservation.stepReservation, int64(charged))
	reservation.settled = settled
	return settled
}

func sameSortedStrings(left, right []string) bool {
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}
