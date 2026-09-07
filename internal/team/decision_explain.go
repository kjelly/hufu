package team

import "fmt"

// Decision routing explainability (spec.md v2 explainability; plan.md Stage 8
// follow-up).
//
// Capability routing already resolves and invokes a concrete agent for the
// REFERENCE/JUDGE/CHALLENGE roles and REVISE's reuse of JUDGE's binding
// (decision_reference_capability_runner.go, decision_judge_capability_runner.go,
// decision_challenge_capability_runner.go); this file only renders what those
// runners already recorded onto DecisionOpinion.AgentID /
// DecisionChallenge.AgentID / DecisionRevision.AgentID /
// ReferenceEvidenceResult.ProducerAgentID into a stable, stage-ordered list
// for inspection tooling.

// DecisionAgentBinding names one concrete agent a decision stage actually
// invoked, if capability routing resolved one. Routed is false (and AgentID
// empty) when the stage ran on the team's legacy judge-model sidecar
// instead. Pinned is true when a routing.pin config forced AgentID rather
// than ranking choosing it (spec.md v2 §34); Reason then carries the pin's
// declared reason.
type DecisionAgentBinding struct {
	Stage   string `json:"stage"`
	Ordinal string `json:"ordinal"`
	AgentID string `json:"agent_id,omitempty"`
	Routed  bool   `json:"routed"`
	Pinned  bool   `json:"pinned,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ExplainDecisionBindings lists which concrete agent, if any, produced each
// stage output in record, in the order those stages ran (reference, then
// judge rounds, then challenges, then revisions). It never fetches anything
// itself: reference is the already-resolved ReferenceEvidenceResult for
// record.ReferenceEvidenceResultRef, or nil when there is none or it could
// not be read.
func ExplainDecisionBindings(record DecisionRecord, reference *ReferenceEvidenceResult) []DecisionAgentBinding {
	var out []DecisionAgentBinding
	if record.ReferenceEvidenceResultRef != nil {
		binding := DecisionAgentBinding{Stage: "reference", Ordinal: "reference"}
		if reference != nil {
			binding.AgentID = reference.ProducerAgentID
			binding.Routed = reference.ProducerAgentID != ""
			binding.Pinned = reference.ProducerPinned
			binding.Reason = reference.ProducerBindingReason
		}
		out = append(out, binding)
	}
	for _, opinion := range record.Opinions {
		out = append(out, DecisionAgentBinding{
			Stage: "judge", Ordinal: opinion.JudgeID, AgentID: opinion.AgentID, Routed: opinion.AgentID != "",
			Pinned: opinion.Pinned, Reason: opinion.BindingReason,
		})
	}
	for i, challenge := range record.Challenges {
		out = append(out, DecisionAgentBinding{
			Stage: "challenge", Ordinal: fmt.Sprintf("challenger-%d", i+1),
			AgentID: challenge.AgentID, Routed: challenge.AgentID != "",
			Pinned: challenge.Pinned, Reason: challenge.BindingReason,
		})
	}
	for _, revision := range record.Revisions {
		out = append(out, DecisionAgentBinding{
			Stage: "revision", Ordinal: revision.JudgeID, AgentID: revision.AgentID, Routed: revision.AgentID != "",
			Pinned: revision.Pinned, Reason: revision.BindingReason,
		})
	}
	return out
}
