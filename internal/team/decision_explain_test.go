package team

import "testing"

func TestExplainDecisionBindings(t *testing.T) {
	record := DecisionRecord{
		ReferenceEvidenceResultRef: &ArtifactRef{ID: "ref-1"},
		Opinions: []DecisionOpinion{
			{JudgeID: "judge-1", AgentID: "cand-a"},
			{JudgeID: "judge-2"},
		},
		Challenges: []DecisionChallenge{
			{AgentID: "cand-b"},
			{},
		},
		Revisions: []DecisionRevision{
			{JudgeID: "judge-1", AgentID: "cand-a"},
		},
	}
	reference := &ReferenceEvidenceResult{ProducerAgentID: "cand-c"}

	got := ExplainDecisionBindings(record, reference)
	want := []DecisionAgentBinding{
		{Stage: "reference", Ordinal: "reference", AgentID: "cand-c", Routed: true},
		{Stage: "judge", Ordinal: "judge-1", AgentID: "cand-a", Routed: true},
		{Stage: "judge", Ordinal: "judge-2", Routed: false},
		{Stage: "challenge", Ordinal: "challenger-1", AgentID: "cand-b", Routed: true},
		{Stage: "challenge", Ordinal: "challenger-2", Routed: false},
		{Stage: "revision", Ordinal: "judge-1", AgentID: "cand-a", Routed: true},
	}
	if len(got) != len(want) {
		t.Fatalf("bindings = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bindings[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// A decision with no reference stage and an unresolved reference result must
// not fabricate a routed binding.
func TestExplainDecisionBindings_NoReferenceStageOrUnresolvedResult(t *testing.T) {
	record := DecisionRecord{Opinions: []DecisionOpinion{{JudgeID: "judge-1"}}}
	got := ExplainDecisionBindings(record, nil)
	want := []DecisionAgentBinding{{Stage: "judge", Ordinal: "judge-1", Routed: false}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("bindings = %#v, want %#v", got, want)
	}

	record.ReferenceEvidenceResultRef = &ArtifactRef{ID: "ref-1"}
	got = ExplainDecisionBindings(record, nil)
	if len(got) != 2 || got[0].Stage != "reference" || got[0].Routed || got[0].AgentID != "" {
		t.Fatalf("bindings[0] = %#v, want an unrouted reference stage when the result could not be read", got[0])
	}
}

// A pinned binding (spec.md v2 §34) must surface its Pinned/Reason markers
// alongside AgentID, for every stage kind and for the reference stage's
// producer-prefixed fields.
func TestExplainDecisionBindings_SurfacesPinnedAndReason(t *testing.T) {
	record := DecisionRecord{
		ReferenceEvidenceResultRef: &ArtifactRef{ID: "ref-1"},
		Opinions:                   []DecisionOpinion{{JudgeID: "judge-1", AgentID: "cand-a", Pinned: true, BindingReason: "regulatory requirement"}},
		Challenges:                 []DecisionChallenge{{AgentID: "cand-b", Pinned: true, BindingReason: "licensed specialist"}},
		Revisions:                  []DecisionRevision{{JudgeID: "judge-1", AgentID: "cand-a", Pinned: true, BindingReason: "regulatory requirement"}},
	}
	reference := &ReferenceEvidenceResult{ProducerAgentID: "cand-c", ProducerPinned: true, ProducerBindingReason: "user explicitly requested named agent"}

	got := ExplainDecisionBindings(record, reference)
	want := []DecisionAgentBinding{
		{Stage: "reference", Ordinal: "reference", AgentID: "cand-c", Routed: true, Pinned: true, Reason: "user explicitly requested named agent"},
		{Stage: "judge", Ordinal: "judge-1", AgentID: "cand-a", Routed: true, Pinned: true, Reason: "regulatory requirement"},
		{Stage: "challenge", Ordinal: "challenger-1", AgentID: "cand-b", Routed: true, Pinned: true, Reason: "licensed specialist"},
		{Stage: "revision", Ordinal: "judge-1", AgentID: "cand-a", Routed: true, Pinned: true, Reason: "regulatory requirement"},
	}
	if len(got) != len(want) {
		t.Fatalf("bindings = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bindings[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
