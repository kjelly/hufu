package team

import "testing"

// DecideRecovery maps one attempt's failure evidence to a retry disposition.
// The early-break paths are checked in order (terminal block, protocol
// failure, replayability, unfixable verify, repeated failure) before the
// class-based mapping; see disposition.go. The retry loop that consumes these
// dispositions is covered by retry_loop_test.go.

func TestDecideRecoveryEarlyBreakPaths(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		// Path 1: a blocked terminal needs a human, ahead of every later path.
		{"terminal blocked", RecoveryDecisionInput{TerminalBlocked: true, Replayable: true, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3}, NeedsHuman},
		{"terminal blocked overrides every later path", RecoveryDecisionInput{TerminalBlocked: true, ProtocolFailure: true, Replayable: false, UnfixableVerify: true, SameFailureRepeated: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3}, NeedsHuman},

		// Path 2: a protocol failure without a safe repair retry reconciles.
		{"protocol failure non-replayable", RecoveryDecisionInput{ProtocolFailure: true, Replayable: false, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"protocol failure replayable but repair retry disallowed", RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: false, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"protocol failure replayable with repair retry allowed", RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"protocol failure beats unfixable verify", RecoveryDecisionInput{ProtocolFailure: true, Replayable: false, UnfixableVerify: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3}, ReconcileOnly},

		// Path 3: a non-replayable task reconciles.
		{"non-replayable execution failure", RecoveryDecisionInput{Replayable: false, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"non-replayable verify failure", RecoveryDecisionInput{Replayable: false, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"non-replayable beats unfixable verify", RecoveryDecisionInput{Replayable: false, UnfixableVerify: true, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3}, ReconcileOnly},

		// Path 4: an unfixable verify command replans while attempts remain.
		{"unfixable verify with remaining attempts", RecoveryDecisionInput{Replayable: true, UnfixableVerify: true, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3}, ReplanRequired},
		{"unfixable verify on last attempt is budget exhausted", RecoveryDecisionInput{Replayable: true, UnfixableVerify: true, FailureClass: FailureVerify, Attempt: 3, MaxRetries: 3}, RetryNone},
		{"unfixable verify beats repeated failure", RecoveryDecisionInput{Replayable: true, UnfixableVerify: true, SameFailureRepeated: true, FailureClass: FailureVerify, Attempt: 2, MaxRetries: 3}, ReplanRequired},

		// Path 5: the same failure twice replans while attempts remain.
		{"same failure repeated with remaining attempts", RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 2, MaxRetries: 3}, ReplanRequired},
		{"same failure repeated on last attempt is budget exhausted", RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 3, MaxRetries: 3}, RetryNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := DecideRecovery(tt.in); got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecideRecoveryAllowsBoundedReplayAfterProvenReadOnlyProtocolFailure(t *testing.T) {
	in := RecoveryDecisionInput{
		FailureClass:        FailureProtocol,
		ProtocolFailure:     true,
		ProtocolRetrySafe:   true,
		Replayable:          true,
		ProtocolRepairRetry: true,
		EvidenceComplete:    true,
		RecoveryPolicy:      RecoveryRetry,
		Attempt:             1,
		MaxRetries:          3,
	}
	if got, reason := DecideRecovery(in); got != RetryWorker || reason == "" {
		t.Fatalf("safe protocol recovery = (%q, %q), want RetryWorker with reason", got, reason)
	}
	in.ProtocolRetrySafe = false
	if got, _ := DecideRecovery(in); got != ReconcileOnly {
		t.Fatalf("unproven protocol recovery = %q, want ReconcileOnly", got)
	}
}

// TestDecideRecoveryClassBasedDisposition covers the failure-class mapping for
// replayable tasks when no early-break path triggers, including the resolved
// recovery policy and the evidence gate.
func TestDecideRecoveryClassBasedDisposition(t *testing.T) {
	tests := []struct {
		name               string
		class              TaskFailureClass
		sideEffect         SideEffectClass
		policy             RecoveryPolicy
		evidenceIncomplete bool
		want               RetryDisposition
	}{
		{name: "execution → retry", class: FailureExecution, policy: RecoveryRetry, want: RetryWorker},
		{name: "execution workspace_write → retry", class: FailureExecution, sideEffect: SideEffectWorkspaceWrite, policy: RecoveryRetry, want: RetryWorker},
		{name: "verify → retry", class: FailureVerify, policy: RecoveryRetry, want: RetryWorker},
		{name: "timeout replayable → retry", class: FailureTimeout, policy: RecoveryRetry, want: RetryWorker},
		{name: "timeout workspace_write → retry", class: FailureTimeout, sideEffect: SideEffectWorkspaceWrite, policy: RecoveryRetry, want: RetryWorker},
		{name: "timeout external_write → reconcile", class: FailureTimeout, sideEffect: SideEffectExternalWrite, policy: RecoveryRetry, want: ReconcileOnly},
		{name: "timeout infra_mutation → reconcile", class: FailureTimeout, sideEffect: SideEffectInfraMutation, policy: RecoveryRetry, want: ReconcileOnly},
		{name: "timeout credential → reconcile", class: FailureTimeout, sideEffect: SideEffectCredential, policy: RecoveryRetry, want: ReconcileOnly},
		{name: "contract → replan", class: FailureContract, policy: RecoveryRetry, want: ReplanRequired},
		{name: "environment → replan", class: FailureEnvironment, policy: RecoveryRetry, want: ReplanRequired},
		{name: "policy → replan", class: FailurePolicy, policy: RecoveryRetry, want: ReplanRequired},
		{name: "protocol → reconcile", class: FailureProtocol, policy: RecoveryRetry, want: ReconcileOnly},
		{name: "cancelled (worker self-cancel) → none", class: FailureCancelled, policy: RecoveryRetry, want: RetryNone},
		{name: "empty policy allows retry (backward compat)", class: FailureExecution, want: RetryWorker},
		{name: "execution under reconcile policy → reconcile", class: FailureExecution, policy: RecoveryReconcile, want: ReconcileOnly},
		{name: "execution under manual policy → needs human", class: FailureExecution, policy: RecoveryManual, want: NeedsHuman},
		{name: "execution under never policy → none", class: FailureExecution, policy: RecoveryNever, want: RetryNone},
		{name: "verify under reconcile policy → reconcile", class: FailureVerify, policy: RecoveryReconcile, want: ReconcileOnly},
		{name: "timeout under manual policy → needs human", class: FailureTimeout, policy: RecoveryManual, want: NeedsHuman},
		{name: "evidence incomplete → replan", class: FailureExecution, policy: RecoveryRetry, evidenceIncomplete: true, want: ReplanRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := RecoveryDecisionInput{
				FailureClass:     tt.class,
				SideEffect:       tt.sideEffect,
				RecoveryPolicy:   tt.policy,
				Replayable:       true,
				Attempt:          1,
				MaxRetries:       3,
				EvidenceComplete: !tt.evidenceIncomplete,
			}
			if got, _ := DecideRecovery(in); got != tt.want {
				t.Errorf("DecideRecovery(class=%s, sideEffect=%s, policy=%q) = %q, want %q", tt.class, tt.sideEffect, tt.policy, got, tt.want)
			}
		})
	}
}

// TestDecideRecoveryCancelledPrecedence: cancellation, whether from the parent
// context or the FailureCancelled class, is never retried.
func TestDecideRecoveryCancelledPrecedence(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
	}{
		{"context cancelled on replayable execution failure", RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3}},
		{"FailureCancelled class without parent cancel", RecoveryDecisionInput{Replayable: true, FailureClass: FailureCancelled, EvidenceComplete: true, Attempt: 1, MaxRetries: 3}},
		{"FailureCancelled class with parent cancel", RecoveryDecisionInput{Replayable: true, FailureClass: FailureCancelled, ContextCancelled: true, Attempt: 1, MaxRetries: 3}},
		{"context cancelled beats class-based retry", RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3}},
		{"cancelled beats non-replayable reconciliation", RecoveryDecisionInput{ContextCancelled: true, Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 1, MaxRetries: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := DecideRecovery(tt.in); got != RetryNone {
				t.Errorf("DecideRecovery() = %q, want %q", got, RetryNone)
			}
		})
	}
}

// TestDecideRecoveryBudgetExhausted: once attempt >= maxRetries a retryable
// failure stops, while non-retry classes keep their own disposition.
func TestDecideRecoveryBudgetExhausted(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		{"execution", RecoveryDecisionInput{Replayable: true, FailureClass: FailureExecution, EvidenceComplete: true, Attempt: 3, MaxRetries: 3}, RetryNone},
		{"contract", RecoveryDecisionInput{Replayable: true, FailureClass: FailureContract, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"environment", RecoveryDecisionInput{Replayable: false, FailureClass: FailureEnvironment, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"policy", RecoveryDecisionInput{Replayable: true, FailureClass: FailurePolicy, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"protocol", RecoveryDecisionInput{Replayable: true, FailureClass: FailureProtocol, Attempt: 3, MaxRetries: 3}, ReconcileOnly},
		{"non-replayable timeout", RecoveryDecisionInput{Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 3, MaxRetries: 3}, ReconcileOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := DecideRecovery(tt.in); got != tt.want {
				t.Fatalf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecideRecoveryReasonNonEmpty: every disposition carries a reason the
// retry loop can report.
func TestDecideRecoveryReasonNonEmpty(t *testing.T) {
	inputs := []RecoveryDecisionInput{
		{TerminalBlocked: true, Replayable: true, Attempt: 1, MaxRetries: 3},
		{ProtocolFailure: true, Replayable: false, Attempt: 1, MaxRetries: 3},
		{Replayable: false, Attempt: 1, MaxRetries: 3},
		{Replayable: true, UnfixableVerify: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, SameFailureRepeated: true, Attempt: 2, MaxRetries: 3},
		{ContextCancelled: true, Replayable: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, Attempt: 3, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureEnvironment, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailurePolicy, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureTimeout, SideEffect: SideEffectNone, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureExecution, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureVerify, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureCancelled, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureExecution, EvidenceComplete: false, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureExecution, RecoveryPolicy: RecoveryReconcile, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureExecution, RecoveryPolicy: RecoveryManual, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
		{Replayable: true, FailureClass: FailureExecution, RecoveryPolicy: RecoveryNever, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
	}
	for i, in := range inputs {
		if _, reason := DecideRecovery(in); reason == "" {
			t.Errorf("input[%d]: reason is empty", i)
		}
	}
}

// TestDecideRecoveryFingerprintRepeatDetection: repeats are detected from the
// normalized failure fingerprints, falling back to SameFailureRepeated only
// when no fingerprints are available.
func TestDecideRecoveryFingerprintRepeatDetection(t *testing.T) {
	// "attempt N" is volatile and normalized away, so differently formatted
	// errors share one digest.
	first := NewFailureFingerprint("task-1", "worker", "verify:test", FailureExecution, "attempt 1 failed: timeout").Digest
	second := NewFailureFingerprint("task-1", "worker", "verify:test", FailureExecution, "attempt 2 failed: timeout").Digest
	if first != second {
		t.Fatalf("differently formatted errors produced different fingerprints: %q vs %q", first, second)
	}

	tests := []struct {
		name         string
		current      string
		previous     string
		sameRepeated bool
		want         RetryDisposition
	}{
		{name: "matching fingerprints are a repeat", current: "ffp_same", previous: "ffp_same", want: ReplanRequired},
		{name: "differently formatted errors with one digest are a repeat", current: second, previous: first, want: ReplanRequired},
		{name: "different fingerprints retry", current: "ffp_a", previous: "ffp_b", want: RetryWorker},
		{name: "different fingerprints override the raw-text flag", current: "ffp_a", previous: "ffp_b", sameRepeated: true, want: RetryWorker},
		{name: "empty fingerprints fall back to the raw-text flag", sameRepeated: true, want: ReplanRequired},
		{name: "empty fingerprints without repeat retry", want: RetryWorker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := RecoveryDecisionInput{
				Replayable:          true,
				FailureClass:        FailureExecution,
				EvidenceComplete:    true,
				FailureFingerprint:  tt.current,
				PreviousFingerprint: tt.previous,
				SameFailureRepeated: tt.sameRepeated,
				Attempt:             2,
				MaxRetries:          3,
			}
			if got, _ := DecideRecovery(in); got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecideRecoveryAcceptanceMatrix maps the §11 acceptance scenarios of the
// generic task reliability design to dispositions.
func TestDecideRecoveryAcceptanceMatrix(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		{"executable missing from PATH or pipeline → environment replan", RecoveryDecisionInput{Replayable: true, FailureClass: FailureEnvironment, Attempt: 1, MaxRetries: 3}, ReplanRequired},
		{"malformed or non-asserting verifier → contract replan", RecoveryDecisionInput{Replayable: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3}, ReplanRequired},
		{"worker succeeded but omitted submit_result → reconcile", RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"non-replayable task timeout → reconcile", RecoveryDecisionInput{Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 1, MaxRetries: 3}, ReconcileOnly},
		{"read-only task timeout → retry", RecoveryDecisionInput{Replayable: true, FailureClass: FailureTimeout, SideEffect: SideEffectNone, EvidenceComplete: true, Attempt: 1, MaxRetries: 3}, RetryWorker},
		{"same fingerprint second failure → replan", RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 2, MaxRetries: 3}, ReplanRequired},
		{"verifier exit 0 but self-reported failure → verification retry", RecoveryDecisionInput{Replayable: true, FailureClass: FailureVerify, EvidenceComplete: true, Attempt: 1, MaxRetries: 3}, RetryWorker},
		{"user SIGINT → none", RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureCancelled, Attempt: 1, MaxRetries: 3}, RetryNone},
		{"worker self-cancel with live parent → none", RecoveryDecisionInput{Replayable: true, FailureClass: FailureCancelled, EvidenceComplete: true, Attempt: 1, MaxRetries: 3}, RetryNone},
		{"repair submitted progress not final → execution retry", RecoveryDecisionInput{Replayable: true, FailureClass: FailureExecution, EvidenceComplete: true, Attempt: 1, MaxRetries: 3}, RetryWorker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := DecideRecovery(tt.in); got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDispositionPredicates(t *testing.T) {
	tests := []struct {
		disposition RetryDisposition
		isRetry     bool
		isStop      bool
		shouldBlock bool
	}{
		{RetryWorker, true, false, false},
		{RetryNone, false, true, false},
		{ReconcileOnly, false, true, true},
		{ReplanRequired, false, true, false},
		{NeedsHuman, false, true, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.disposition), func(t *testing.T) {
			if got := IsRetryDisposition(tt.disposition); got != tt.isRetry {
				t.Errorf("IsRetryDisposition = %v, want %v", got, tt.isRetry)
			}
			if got := IsStopDisposition(tt.disposition); got != tt.isStop {
				t.Errorf("IsStopDisposition = %v, want %v", got, tt.isStop)
			}
			if got := ShouldBlockTask(tt.disposition); got != tt.shouldBlock {
				t.Errorf("ShouldBlockTask = %v, want %v", got, tt.shouldBlock)
			}
		})
	}
}
