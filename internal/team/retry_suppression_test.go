package team

import "testing"

func TestRetrySuppressionReason(t *testing.T) {
	base := RecoveryDecisionInput{
		FailureClass:     FailureExecution,
		Attempt:          1,
		MaxRetries:       2,
		EvidenceComplete: true,
		Replayable:       true,
	}
	tests := []struct {
		name        string
		input       RecoveryDecisionInput
		disposition RetryDisposition
		want        string
		ok          bool
	}{
		{name: "repeated fingerprint", input: func() RecoveryDecisionInput {
			in := base
			in.FailureFingerprint = "same"
			in.PreviousFingerprint = "same"
			return in
		}(), disposition: ReplanRequired, want: retrySuppressionRepeatedFingerprint, ok: true},
		{name: "incomplete evidence", input: func() RecoveryDecisionInput { in := base; in.EvidenceComplete = false; return in }(), disposition: ReplanRequired, want: retrySuppressionEvidenceIncomplete, ok: true},
		{name: "unfixable verifier", input: func() RecoveryDecisionInput { in := base; in.UnfixableVerify = true; return in }(), disposition: ReplanRequired, want: retrySuppressionUnfixableVerifier, ok: true},
		{name: "retry allowed", input: base, disposition: RetryWorker},
		{name: "retry budget exhausted", input: func() RecoveryDecisionInput { in := base; in.Attempt = 2; return in }(), disposition: RetryNone},
		{name: "cancelled", input: func() RecoveryDecisionInput { in := base; in.ContextCancelled = true; return in }(), disposition: RetryNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := retrySuppressionReason(tt.input, tt.disposition)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("retrySuppressionReason() = (%q, %t), want (%q, %t)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRetrySuppressionEventAndMetricsSurviveEventReplay(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-r3", "session-r3")
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		eventStore:                store,
		executionRunID:            "run-r3",
		taskTracker:               NewTaskTracker(),
		retrySuppressionsByReason: make(map[string]int),
		retrySuppressionSeen:      make(map[string]bool),
	}
	c.recordRetrySuppression("task-1", "fingerprint-1", ReplanRequired, retrySuppressionRepeatedFingerprint)
	c.recordRetrySuppression("task-1", "fingerprint-1", ReplanRequired, retrySuppressionRepeatedFingerprint)

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "retry_suppressed" {
		t.Fatalf("events = %#v, want one retry_suppressed event", events)
	}
	if got := c.Metrics(); got.RetrySuppressions != 1 || got.RetrySuppressionsByReason[retrySuppressionRepeatedFingerprint] != 1 {
		t.Fatalf("live metrics = %#v, want one repeated-fingerprint suppression", got)
	}

	reopened, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	restored := &Coordinator{eventStore: reopened, executionRunID: "run-r3", taskTracker: NewTaskTracker()}
	if got := restored.Metrics(); got.RetrySuppressions != 1 || got.RetrySuppressionsByReason[retrySuppressionRepeatedFingerprint] != 1 {
		t.Fatalf("replayed metrics = %#v, want one repeated-fingerprint suppression", got)
	}
}
