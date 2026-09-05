package team

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Assumption lifecycle and replan (docs/hufu-decision-aware-runtime-spec.md
// §18, §31).
//
// The invariant that shapes this file: a decision that turns out to rest on a
// false assumption stays exactly as it was recorded. The runtime marks it
// stale and starts a new lineage. Editing the old record until it looks
// consistent with the present destroys the only evidence of what was actually
// believed at the time.

// Assumption transition sources. The runtime never infers a status; it is
// changed only by one of these (spec §18.1).
const (
	AssumptionSourceTaskResult   = "task_result"
	AssumptionSourceVerification = "verification"
	AssumptionSourceOperator     = "operator"
)

// AssumptionTransition is one recorded status change.
type AssumptionTransition struct {
	DecisionID   string
	AssumptionID string
	From         string
	To           string
	Source       string
	EvidenceRefs []ArtifactRef
	At           time.Time
}

// ValidAssumptionSource reports whether a status change came from a source the
// runtime trusts to make one.
func ValidAssumptionSource(source string) bool {
	switch source {
	case AssumptionSourceTaskResult, AssumptionSourceVerification, AssumptionSourceOperator:
		return true
	}
	return false
}

// ApplyAssumptionTransition records a status change on a copy of the
// assumptions and returns the updated set. It never mutates in place and never
// infers: an unrecognized source or status is refused.
func ApplyAssumptionTransition(assumptions []DecisionAssumption, transition AssumptionTransition) ([]DecisionAssumption, DecisionAssumption, error) {
	if !ValidAssumptionSource(transition.Source) {
		return nil, DecisionAssumption{}, fmt.Errorf(
			"assumption %s: %q is not a source that may change assumption status",
			transition.AssumptionID, transition.Source)
	}
	if !ValidAssumptionStatus(transition.To) {
		return nil, DecisionAssumption{}, fmt.Errorf(
			"assumption %s: %q is not a declared lifecycle state", transition.AssumptionID, transition.To)
	}

	out := append([]DecisionAssumption(nil), assumptions...)
	for i := range out {
		if out[i].ID != transition.AssumptionID {
			continue
		}
		updated := out[i]
		updated.Status = transition.To
		updated.CheckedAt = transition.At
		if len(transition.EvidenceRefs) > 0 {
			updated.EvidenceRefs = append(append([]ArtifactRef(nil), updated.EvidenceRefs...), transition.EvidenceRefs...)
		}
		out[i] = updated
		return out, updated, nil
	}
	return nil, DecisionAssumption{}, fmt.Errorf("assumption %q is not declared on this decision", transition.AssumptionID)
}

// RecordAssumptionTransition applies the change and appends its lifecycle event.
func RecordAssumptionTransition(
	ctx context.Context,
	journal decisionJournal,
	assumptions []DecisionAssumption,
	transition AssumptionTransition,
) ([]DecisionAssumption, DecisionAssumption, error) {
	updated, assumption, err := ApplyAssumptionTransition(assumptions, transition)
	if err != nil {
		return nil, DecisionAssumption{}, err
	}
	if transition.From == "" {
		transition.From = AssumptionUnknown
	}
	if err := appendDecisionEvent(ctx, journal, AssumptionStatusEvent(transition.To), decisionEvent{
		DecisionID:   transition.DecisionID,
		AssumptionID: transition.AssumptionID, From: transition.From, To: transition.To,
		Source: transition.Source, EvidenceRefs: transition.EvidenceRefs, At: transition.At,
		Reason: fmt.Sprintf("assumption %s: %s -> %s (source: %s)",
			transition.AssumptionID, transition.From, transition.To, transition.Source),
	}); err != nil {
		return nil, DecisionAssumption{}, err
	}
	return updated, assumption, nil
}

// CriticalContradiction returns the first critical assumption in ascending ID
// order that has been contradicted, or an empty string when none has.
func CriticalContradiction(assumptions []DecisionAssumption) string {
	found := ""
	for _, assumption := range assumptions {
		if !assumption.Critical || assumption.EffectiveStatus() != AssumptionContradicted {
			continue
		}
		if found == "" || assumption.ID < found {
			found = assumption.ID
		}
	}
	return found
}

// MarkDecisionStale marks a durable record superseded. Stale and StaleReason
// are the only fields that may be set after a record is persisted, and only
// once, unset to set (spec §35). Everything else stays exactly as recorded.
func MarkDecisionStale(
	ctx context.Context,
	journal decisionJournal,
	record DecisionRecord,
	reason string,
) (DecisionRecord, error) {
	if record.Stale {
		return record, nil
	}
	if strings.TrimSpace(reason) == "" {
		return record, fmt.Errorf("decision %s cannot be marked stale without a reason", record.ID)
	}
	superseded := record
	superseded.Stale = true
	superseded.StaleReason = reason

	if err := appendDecisionEvent(ctx, journal, agent.EventDecisionInvalidated, decisionEvent{
		DecisionID: record.ID, EvidenceHash: record.EvidenceHash, Reason: reason,
	}); err != nil {
		return record, err
	}
	return superseded, nil
}

// RequestReplan records that a checkpoint decided to replan, so the reason a
// plan was abandoned survives in the log rather than only in a status message.
func RequestReplan(ctx context.Context, journal decisionJournal, decisionID string, decision CheckpointDecision) error {
	return appendDecisionEvent(ctx, journal, agent.EventReplanRequested, decisionEvent{
		DecisionID: decisionID,
		Reason:     fmt.Sprintf("%s: %s", decision.Reason, decision.Detail),
	})
}

// ReplanActionIsTerminal reports whether an action ends the current execution
// rather than continuing it.
func ReplanActionIsTerminal(action string) bool {
	switch action {
	case agent.ReplanStop, agent.ReplanNeedsHuman, agent.ReplanEscalate:
		return true
	}
	return false
}
