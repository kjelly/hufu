package team

import (
	"encoding/json"
	"reflect"
	"testing"
)

func controlObservationEvent(t *testing.T, payload controlDecisionPayload) RunEvent {
	t.Helper()
	payload.Version = 1
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return RunEvent{Type: string(EventControlDecisionObserved), Actor: "coder", Payload: data}
}

func TestSummarizeControlDecisionsAggregatesPerPointAndMode(t *testing.T) {
	yes, no := true, false
	events := []RunEvent{
		controlObservationEvent(t, controlDecisionPayload{Point: "path-reviewer", Mode: "shadow", Applied: "legacy", Status: "decided", Value: "false", Confidence: 0.98, Accepted: true, Threshold: 0.9, Legacy: "false", Agree: &yes, DurationMS: 100}),
		controlObservationEvent(t, controlDecisionPayload{Point: "path-reviewer", Mode: "shadow", Applied: "legacy", Status: "decided", Value: "true", Confidence: 0.6, Threshold: 0.9, Legacy: "false", Agree: &no, DurationMS: 300}),
		controlObservationEvent(t, controlDecisionPayload{Point: "path-reviewer", Mode: "shadow", Applied: "legacy", Status: "error", ErrorCode: "timeout", Threshold: 0.9, Legacy: "true", DurationMS: 5000}),
		controlObservationEvent(t, controlDecisionPayload{Point: "path-reviewer", Mode: "active", Applied: "primitive", Status: "decided", Value: "false", Confidence: 0.99, Accepted: true, Threshold: 0.9, DurationMS: 200}),
		controlObservationEvent(t, controlDecisionPayload{Point: "agent-matcher", Mode: "active", Applied: "safe_default", Status: "decided", Value: "1", Confidence: 0.5, Threshold: 0.6, Candidates: 2, DurationMS: 400}),
		{Type: string(EventControlDecisionObserved), Actor: "coder", Payload: json.RawMessage(`{"version":1,"point":"bogus"}`)},
		{Type: string(EventTaskCreated), Actor: "coder", Payload: json.RawMessage(`{}`)},
	}
	got := SummarizeControlDecisions(events)
	want := []ControlDecisionSummary{
		{Point: "agent-matcher", Mode: "active", Calls: 1, Decided: 1, BelowThreshold: 1, AppliedSafeDefault: 1, MeanConfidence: 0.5, P50MS: 400, P95MS: 400},
		{Point: "path-reviewer", Mode: "active", Calls: 1, Decided: 1, AppliedPrimitive: 1, MeanConfidence: 0.99, P50MS: 200, P95MS: 200},
		{Point: "path-reviewer", Mode: "shadow", Calls: 3, Decided: 2, Errors: 1, Compared: 2, Agreed: 1, BelowThreshold: 1, AppliedLegacy: 3,
			MeanConfidence: (0.98 + 0.6) / 2, P50MS: 300, P95MS: 5000, ErrorCodes: map[string]int{"timeout": 1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries =\n%#v\nwant\n%#v", got, want)
	}
	if len(SummarizeControlDecisions(nil)) != 0 {
		t.Fatal("no observations must summarize to nothing")
	}
}
