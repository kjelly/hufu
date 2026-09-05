package team

import (
	"encoding/json"
	"sort"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision observability (docs/hufu-decision-aware-runtime-spec.md §40).
//
// Every counter here is a projection over state the runtime already persisted:
// the append-only event log and the cross-run index. Nothing is counted twice
// and nothing is stored separately, so a metric can never drift from the events
// that produced it — the same reason StopPolicy reads the budget ledger rather
// than keeping its own tally (§29).

// DecisionMetrics is the projected view of decision activity.
type DecisionMetrics struct {
	// Formation
	DecisionCount        int            `json:"decision_count"`
	DecisionProfileCount map[string]int `json:"decision_profile_count,omitempty"`

	// Quality gates
	OutsideViewGateFailures int `json:"outside_view_gate_failures"`
	PremortemFailureModes   int `json:"premortem_failure_modes"`
	NoGoOptionMissing       int `json:"no_go_option_missing"`

	// Judgment
	DecisionDispersion           map[string]float64 `json:"decision_dispersion,omitempty"`
	DecisionRevisionRate         float64            `json:"decision_revision_rate"`
	DecisionStaleCount           int                `json:"decision_stale_count"`
	DecisionOpinionRejectedCount int                `json:"decision_opinion_rejected_count"`
	DecisionBudgetDegradedCount  int                `json:"decision_budget_degraded_count"`

	// Evidence
	IndependentEvidenceGroupCount int `json:"independent_evidence_group_count"`
	SharedOriginWarnings          int `json:"shared_origin_warnings"`

	// Execution discipline
	KillCriteriaTriggered   int `json:"kill_criteria_triggered"`
	ReplanCount             int `json:"replan_count"`
	AssumptionInvalidations int `json:"assumption_invalidations"`
	CommitGateBlocked       int `json:"commit_gate_blocked"`

	// Outcomes. Resolution is the Phase 5 entry condition, so these say how
	// close the sample is to opening it (§49.2).
	ResolvedCount int `json:"resolved_count"`
	VerifiedCount int `json:"verified_count"`
}

// ComputeDecisionMetrics projects decision activity from a run's event log and
// the workspace's cross-run index. Either input may be empty: a workspace with
// no decisions reports zeroes rather than an error.
func ComputeDecisionMetrics(events []RunEvent, entries []DecisionIndexEntry) DecisionMetrics {
	metrics := DecisionMetrics{
		DecisionProfileCount: map[string]int{},
		DecisionDispersion:   map[string]float64{},
	}

	// Decisions that reached an aggregate, and the rounds they ran, are what
	// the revision rate is a ratio of.
	finalized := map[string]bool{}
	revised := map[string]bool{}

	for _, event := range events {
		var payload decisionEvent
		if len(event.Payload) > 0 {
			// Foreign event types share the log; a payload that is not a
			// decision payload simply contributes nothing.
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				payload = decisionEvent{}
			}
		}

		switch event.Type {
		case agent.EventDecisionFinalized:
			if payload.DecisionID != "" {
				finalized[payload.DecisionID] = true
			}
		case agent.EventDecisionRevisionSubmitted:
			if payload.DecisionID != "" {
				revised[payload.DecisionID] = true
			}
		case agent.EventDecisionAggregateComputed:
			if payload.Aggregate != nil && payload.DecisionID != "" {
				metrics.DecisionDispersion[payload.DecisionID] = DispersionOf(*payload.Aggregate)
			}
		case agent.EventDecisionOpinionRejected:
			metrics.DecisionOpinionRejectedCount++
		case agent.EventDecisionBudgetDegraded:
			metrics.DecisionBudgetDegradedCount++
		case agent.EventDecisionPremortemSubmitted:
			if payload.Premortem != nil {
				metrics.PremortemFailureModes += len(payload.Premortem.FailureModes)
			}
		case agent.EventDecisionEvidenceSharedOrigin:
			metrics.SharedOriginWarnings++
		case agent.EventDecisionInvalidated:
			metrics.DecisionStaleCount++
		case agent.EventAssumptionContradicted:
			metrics.AssumptionInvalidations++
		case agent.EventReplanRequested:
			metrics.ReplanCount++
		case agent.EventKillCriterionTriggered:
			metrics.KillCriteriaTriggered++
		case agent.EventCommitGateBlocked:
			metrics.CommitGateBlocked++
		}

		// Gate failures are recorded as reason codes rather than as their own
		// event types, so they are counted from the reason.
		switch payload.Reason {
		case ReasonDecisionOutsideViewMissing:
			metrics.OutsideViewGateFailures++
		case ReasonDecisionNoNoGoOption:
			metrics.NoGoOptionMissing++
		}
	}

	if len(finalized) > 0 {
		metrics.DecisionRevisionRate = roundDecision(float64(len(revised)) / float64(len(finalized)))
	}

	for _, entry := range entries {
		metrics.DecisionCount++
		if entry.Profile != "" {
			metrics.DecisionProfileCount[entry.Profile]++
		}
		metrics.IndependentEvidenceGroupCount += entry.IndependenceGroupCount
		if entry.Resolved() {
			metrics.ResolvedCount++
			if entry.Outcome.Verified {
				metrics.VerifiedCount++
			}
		}
	}
	return metrics
}

// Phase5EntryStatus reports how close the resolved sample is to opening
// Phase 5. The thresholds are the ones §49.2 fixed: enough resolved decisions
// that a Brier score's confidence interval is narrower than the differences it
// would be used to detect, and enough verified ones that they are their own
// group rather than being diluted by asserted outcomes.
type Phase5EntryStatus struct {
	Resolved         int  `json:"resolved"`
	ResolvedRequired int  `json:"resolved_required"`
	Verified         int  `json:"verified"`
	VerifiedRequired int  `json:"verified_required"`
	Open             bool `json:"open"`
}

// Phase 5 entry thresholds (spec §49.2).
const (
	Phase5ResolvedRequired = 30
	Phase5VerifiedRequired = 20
)

// Phase5Entry evaluates the entry condition against the current sample.
func (m DecisionMetrics) Phase5Entry() Phase5EntryStatus {
	return Phase5EntryStatus{
		Resolved:         m.ResolvedCount,
		ResolvedRequired: Phase5ResolvedRequired,
		Verified:         m.VerifiedCount,
		VerifiedRequired: Phase5VerifiedRequired,
		Open:             m.ResolvedCount >= Phase5ResolvedRequired && m.VerifiedCount >= Phase5VerifiedRequired,
	}
}

// SortedProfiles returns the profiles that formed decisions, in a stable order.
func (m DecisionMetrics) SortedProfiles() []string {
	names := make([]string, 0, len(m.DecisionProfileCount))
	for name := range m.DecisionProfileCount {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MeanDispersion returns the average dispersion across decisions that produced
// an aggregate, or zero when none did.
func (m DecisionMetrics) MeanDispersion() float64 {
	if len(m.DecisionDispersion) == 0 {
		return 0
	}
	ids := make([]string, 0, len(m.DecisionDispersion))
	for id := range m.DecisionDispersion {
		ids = append(ids, id)
	}
	// Fixed iteration order keeps the float sum reproducible (§14.4).
	sort.Strings(ids)
	var sum float64
	for _, id := range ids {
		sum += m.DecisionDispersion[id]
	}
	return roundDecision(sum / float64(len(ids)))
}
