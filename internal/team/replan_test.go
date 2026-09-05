package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func assumptionsFixture() []DecisionAssumption {
	return []DecisionAssumption{
		{ID: "A1", Statement: "traffic stays flat", Critical: true, Status: AssumptionSupported},
		{ID: "A2", Statement: "the vendor API is stable"},
	}
}

// Only the three declared sources may change a status; the runtime never
// infers one (spec §18.1).
func TestAssumptionTransitionSources(t *testing.T) {
	for _, source := range []string{AssumptionSourceTaskResult, AssumptionSourceVerification, AssumptionSourceOperator} {
		if !ValidAssumptionSource(source) {
			t.Fatalf("ValidAssumptionSource(%q) = false", source)
		}
		if _, _, err := ApplyAssumptionTransition(assumptionsFixture(), AssumptionTransition{
			AssumptionID: "A1", To: AssumptionContradicted, Source: source,
		}); err != nil {
			t.Fatalf("source %q rejected: %v", source, err)
		}
	}
	for _, source := range []string{"", "inference", "judge"} {
		if ValidAssumptionSource(source) {
			t.Fatalf("ValidAssumptionSource(%q) = true", source)
		}
		if _, _, err := ApplyAssumptionTransition(assumptionsFixture(), AssumptionTransition{
			AssumptionID: "A1", To: AssumptionContradicted, Source: source,
		}); err == nil {
			t.Fatalf("source %q accepted", source)
		}
	}
}

func TestApplyAssumptionTransition(t *testing.T) {
	original := assumptionsFixture()
	updated, assumption, err := ApplyAssumptionTransition(original, AssumptionTransition{
		AssumptionID: "A1", To: AssumptionContradicted, Source: AssumptionSourceVerification,
		At: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if assumption.Status != AssumptionContradicted || updated[0].Status != AssumptionContradicted {
		t.Fatalf("transition did not apply: %#v", updated[0])
	}
	// The input slice is never mutated in place.
	if original[0].Status != AssumptionSupported {
		t.Fatalf("original assumptions were mutated: %#v", original[0])
	}

	if _, _, err := ApplyAssumptionTransition(original, AssumptionTransition{
		AssumptionID: "A9", To: AssumptionSupported, Source: AssumptionSourceOperator,
	}); err == nil {
		t.Fatal("a transition on an undeclared assumption was accepted")
	}
	if _, _, err := ApplyAssumptionTransition(original, AssumptionTransition{
		AssumptionID: "A1", To: "probably", Source: AssumptionSourceOperator,
	}); err == nil {
		t.Fatal("an undeclared lifecycle state was accepted")
	}
}

func TestRecordAssumptionTransitionEmitsLifecycleEvent(t *testing.T) {
	journal := &memoryJournal{}
	_, _, err := RecordAssumptionTransition(context.Background(), journal, assumptionsFixture(), AssumptionTransition{
		DecisionID: "dec-1", AssumptionID: "A1", From: AssumptionSupported,
		To: AssumptionContradicted, Source: AssumptionSourceVerification,
	})
	if err != nil {
		t.Fatal(err)
	}
	if journal.count(agent.EventAssumptionContradicted) != 1 {
		t.Fatalf("events = %v, want an assumption_contradicted event", journal.typesOf())
	}
}

// An unchecked assumption stays unknown and does not block execution: gating on
// it would deadlock every decision that declares one (spec §18.2).
func TestUncheckedAssumptionDoesNotBlock(t *testing.T) {
	assumptions := []DecisionAssumption{{ID: "A1", Critical: true}}
	if got := (assumptions[0]).EffectiveStatus(); got != AssumptionUnknown {
		t.Fatalf("EffectiveStatus = %q, want unknown", got)
	}
	if contradicted := CriticalContradiction(assumptions); contradicted != "" {
		t.Fatalf("CriticalContradiction = %q, want empty for an unchecked assumption", contradicted)
	}
}

func TestCriticalContradiction(t *testing.T) {
	assumptions := []DecisionAssumption{
		{ID: "A3", Critical: true, Status: AssumptionContradicted},
		{ID: "A1", Critical: false, Status: AssumptionContradicted},
		{ID: "A2", Critical: true, Status: AssumptionContradicted},
	}
	if got := CriticalContradiction(assumptions); got != "A2" {
		t.Fatalf("CriticalContradiction = %q, want the ascending-ID first critical one", got)
	}
	if got := CriticalContradiction([]DecisionAssumption{{ID: "A1", Status: AssumptionSupported, Critical: true}}); got != "" {
		t.Fatalf("CriticalContradiction = %q, want empty", got)
	}
}

// A superseded decision keeps every field it was recorded with. Editing history
// to look consistent with the present destroys the evidence of what was
// actually believed at the time (spec §31, §35).
func TestMarkDecisionStalePreservesTheRecord(t *testing.T) {
	journal := &memoryJournal{}
	record := fullDecisionRecord()
	before, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}

	superseded, err := MarkDecisionStale(context.Background(), journal, record, "critical assumption A1 was contradicted")
	if err != nil {
		t.Fatal(err)
	}
	if !superseded.Stale || superseded.StaleReason == "" {
		t.Fatalf("record was not marked stale: %#v", superseded)
	}

	// The caller's record is untouched, and only the two stale fields differ.
	after, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("MarkDecisionStale mutated the caller's record")
	}
	superseded.Stale = false
	superseded.StaleReason = ""
	restored, err := json.Marshal(superseded)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(before) {
		t.Fatalf("marking stale changed other fields:\n got %s\nwant %s", restored, before)
	}

	if journal.count(agent.EventDecisionInvalidated) != 1 {
		t.Fatalf("events = %v, want decision_invalidated", journal.typesOf())
	}
}

func TestMarkDecisionStaleIsOneWayAndNeedsAReason(t *testing.T) {
	journal := &memoryJournal{}
	record := DecisionRecord{ID: "dec-1"}

	if _, err := MarkDecisionStale(context.Background(), journal, record, "  "); err == nil {
		t.Fatal("a record was marked stale without a reason")
	}

	stale, err := MarkDecisionStale(context.Background(), journal, record, "evidence changed")
	if err != nil {
		t.Fatal(err)
	}
	again, err := MarkDecisionStale(context.Background(), journal, stale, "some other reason")
	if err != nil {
		t.Fatal(err)
	}
	if again.StaleReason != "evidence changed" {
		t.Fatalf("StaleReason = %q, want the first reason preserved", again.StaleReason)
	}
	if journal.count(agent.EventDecisionInvalidated) != 1 {
		t.Fatalf("invalidation events = %d, want exactly 1", journal.count(agent.EventDecisionInvalidated))
	}
}

func TestRequestReplanRecordsTheReason(t *testing.T) {
	journal := &memoryJournal{}
	err := RequestReplan(context.Background(), journal, "dec-1", CheckpointDecision{
		Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated, Detail: "critical assumption A1 was contradicted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if journal.count(agent.EventReplanRequested) != 1 {
		t.Fatalf("events = %v, want replan_requested", journal.typesOf())
	}
	payload := journal.events[0].Payload
	if !strings.Contains(string(payload), "A1") {
		t.Fatalf("payload = %s, want the reason recorded", payload)
	}
}

func TestReplanActionIsTerminal(t *testing.T) {
	for action, want := range map[string]bool{
		agent.ReplanContinue:           false,
		agent.ReplanReplan:             false,
		agent.ReplanRequestInformation: false,
		agent.ReplanStop:               true,
		agent.ReplanEscalate:           true,
		agent.ReplanNeedsHuman:         true,
	} {
		if got := ReplanActionIsTerminal(action); got != want {
			t.Errorf("ReplanActionIsTerminal(%q) = %v, want %v", action, got, want)
		}
	}
}
