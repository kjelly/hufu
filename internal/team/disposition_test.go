package team

import (
	"testing"
)

// TestDecideRecovery_FiveEarlyBreakPaths verifies the five pre-refactoring
// early-break paths produce the same dispositions they originally did as
// separate if-statements in the retry loop. These are the characterization
// tests required by WP-08 ("確保重構前後行為等價").
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-07, WP-08
func TestDecideRecovery_FiveEarlyBreakPaths(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		// Path 1: terminalBlocked → NeedsHuman
		{
			name: "terminal blocked → needs human",
			in:   RecoveryDecisionInput{TerminalBlocked: true, Replayable: true, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3},
			want: NeedsHuman,
		},
		{
			name: "terminal blocked overrides everything",
			in:   RecoveryDecisionInput{TerminalBlocked: true, ProtocolFailure: true, Replayable: false, UnfixableVerify: true, SameFailureRepeated: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3},
			want: NeedsHuman,
		},

		// Path 2: protocolFailure && (!Replayable || !ProtocolRepairRetry) → ReconcileOnly
		{
			name: "protocol failure non-replayable → reconcile",
			in:   RecoveryDecisionInput{ProtocolFailure: true, Replayable: false, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},
		{
			name: "protocol failure replayable but repair retry disallowed → reconcile",
			in:   RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: false, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},
		{
			name: "protocol failure replayable and repair retry allowed → class-based (reconcile)",
			in:   RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},

		// Path 3: !Replayable → ReconcileOnly
		{
			name: "non-replayable execution failure → reconcile",
			in:   RecoveryDecisionInput{Replayable: false, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},
		{
			name: "non-replayable verify failure → reconcile",
			in:   RecoveryDecisionInput{Replayable: false, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},

		// Path 4: UnfixableVerify && attempt < maxRetries → ReplanRequired
		{
			name: "unfixable verify with remaining attempts → replan",
			in:   RecoveryDecisionInput{Replayable: true, UnfixableVerify: true, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "unfixable verify on last attempt → budget exhausted (not replan)",
			in:   RecoveryDecisionInput{Replayable: true, UnfixableVerify: true, FailureClass: FailureVerify, Attempt: 3, MaxRetries: 3},
			want: RetryNone,
		},

		// Path 5: SameFailureRepeated && attempt < maxRetries → ReplanRequired
		{
			name: "same failure repeated with remaining attempts → replan",
			in:   RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 2, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "same failure repeated on last attempt → budget exhausted",
			in:   RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 3, MaxRetries: 3},
			want: RetryNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := DecideRecovery(tt.in)
			if got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDecideRecovery_AllowsBoundedReplayAfterProvenReadOnlyProtocolFailure(t *testing.T) {
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

// TestDecideRecovery_ClassBasedDisposition verifies the §5 failure-class →
// disposition mapping for replayable tasks where none of the five early-break
// paths trigger.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-07
func TestDecideRecovery_ClassBasedDisposition(t *testing.T) {
	tests := []struct {
		name       string
		class      TaskFailureClass
		sideEffect SideEffectClass
		policy     RecoveryPolicy
		want       RetryDisposition
	}{
		{"execution → retry", FailureExecution, SideEffectNone, RecoveryRetry, RetryWorker},
		{"execution workspace_write → retry", FailureExecution, SideEffectWorkspaceWrite, RecoveryRetry, RetryWorker},
		{"verify → retry", FailureVerify, SideEffectNone, RecoveryRetry, RetryWorker},
		{"timeout replayable → retry", FailureTimeout, SideEffectNone, RecoveryRetry, RetryWorker},
		{"timeout workspace_write → retry", FailureTimeout, SideEffectWorkspaceWrite, RecoveryRetry, RetryWorker},
		{"timeout external_write → reconcile", FailureTimeout, SideEffectExternalWrite, RecoveryRetry, ReconcileOnly},
		{"timeout infra_mutation → reconcile", FailureTimeout, SideEffectInfraMutation, RecoveryRetry, ReconcileOnly},
		{"timeout credential → reconcile", FailureTimeout, SideEffectCredential, RecoveryRetry, ReconcileOnly},
		{"contract → replan", FailureContract, SideEffectNone, RecoveryRetry, ReplanRequired},
		{"environment → replan", FailureEnvironment, SideEffectNone, RecoveryRetry, ReplanRequired},
		{"policy → replan", FailurePolicy, SideEffectNone, RecoveryRetry, ReplanRequired},
		{"protocol → reconcile", FailureProtocol, SideEffectNone, RecoveryRetry, ReconcileOnly},
		{"cancelled (worker self-cancel) → none", FailureCancelled, SideEffectNone, RecoveryRetry, RetryNone},
		{"execution under reconcile policy → reconcile", FailureExecution, SideEffectNone, RecoveryReconcile, ReconcileOnly},
		{"execution under manual policy → needs human", FailureExecution, SideEffectNone, RecoveryManual, NeedsHuman},
		{"execution under never policy → none", FailureExecution, SideEffectNone, RecoveryNever, RetryNone},
		{"verify under reconcile policy → reconcile", FailureVerify, SideEffectNone, RecoveryReconcile, ReconcileOnly},
		{"timeout under manual policy → needs human", FailureTimeout, SideEffectNone, RecoveryManual, NeedsHuman},
		{"evidence incomplete → replan", FailureExecution, SideEffectNone, RecoveryRetry, ReplanRequired},
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
				EvidenceComplete: tt.name != "evidence incomplete → replan",
			}
			got, _ := DecideRecovery(in)
			if got != tt.want {
				t.Errorf("DecideRecovery(class=%s, sideEffect=%s) = %q, want %q",
					tt.class, tt.sideEffect, got, tt.want)
			}
		})
	}
}

// TestDecideRecovery_CancelledPrecedence verifies §5.3: cancelled failures
// (context cancellation or FailureCancelled class) are separated from
// execution failures and produce RetryNone, not a retry.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-07
func TestDecideRecovery_CancelledPrecedence(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		{
			name: "context cancelled on replayable execution failure",
			in:   RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureExecution, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "FailureCancelled class on replayable task without parent cancel → none",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureCancelled, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "FailureCancelled class with parent cancel → none",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureCancelled, ContextCancelled: true, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "context cancelled beats class-based retry",
			in:   RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureVerify, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "cancelled beats non-replayable reconciliation",
			in:   RecoveryDecisionInput{ContextCancelled: true, Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := DecideRecovery(tt.in)
			if got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecideRecovery_BudgetExhausted verifies that the retry budget check
// returns RetryNone when attempt >= maxRetries and none of the five paths
// trigger.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-07
func TestDecideRecovery_BudgetExhausted(t *testing.T) {
	in := RecoveryDecisionInput{
		Replayable:       true,
		FailureClass:     FailureExecution,
		EvidenceComplete: true,
		Attempt:          3,
		MaxRetries:       3,
	}
	got, _ := DecideRecovery(in)
	if got != RetryNone {
		t.Errorf("DecideRecovery() = %q, want RetryNone (budget exhausted)", got)
	}
}

func TestDecideRecovery_NonRetryClassesPreserveDispositionAfterBudget(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		{"contract", RecoveryDecisionInput{Replayable: true, FailureClass: FailureContract, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"environment", RecoveryDecisionInput{Replayable: false, FailureClass: FailureEnvironment, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"policy", RecoveryDecisionInput{Replayable: true, FailureClass: FailurePolicy, Attempt: 3, MaxRetries: 3}, ReplanRequired},
		{"protocol", RecoveryDecisionInput{Replayable: true, FailureClass: FailureProtocol, Attempt: 3, MaxRetries: 3}, ReconcileOnly},
		{"non-replayable timeout", RecoveryDecisionInput{Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 3, MaxRetries: 3}, ReconcileOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := DecideRecovery(tt.in)
			if got != tt.want {
				t.Fatalf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecideRecovery_ReasonNonEmpty verifies that the reason string is always
// non-empty for every disposition, so the retry loop can use it in report
// messages.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §9, WP-07
func TestDecideRecovery_ReasonNonEmpty(t *testing.T) {
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
		_, reason := DecideRecovery(in)
		if reason == "" {
			t.Errorf("input[%d]: reason is empty", i)
		}
	}
}

// TestDecideRecovery_PathOrdering verifies that the five early-break paths are
// checked in the correct order — terminalBlocked before protocolFailure,
// protocolFailure before replayable, etc. — matching the pre-refactoring
// loop's if-statement order.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestDecideRecovery_PathOrdering(t *testing.T) {
	// terminalBlocked takes precedence over protocolFailure + non-replayable.
	in := RecoveryDecisionInput{
		TerminalBlocked:     true,
		ProtocolFailure:     true,
		Replayable:          false,
		UnfixableVerify:     true,
		SameFailureRepeated: true,
		FailureClass:        FailureContract,
		Attempt:             1,
		MaxRetries:          3,
	}
	got, _ := DecideRecovery(in)
	if got != NeedsHuman {
		t.Errorf("terminalBlocked should take precedence; got %q, want %q", got, NeedsHuman)
	}

	// protocolFailure non-replayable takes precedence over unfixable.
	in = RecoveryDecisionInput{
		ProtocolFailure: true,
		Replayable:      false,
		UnfixableVerify: true,
		FailureClass:    FailureProtocol,
		Attempt:         1,
		MaxRetries:      3,
	}
	got, _ = DecideRecovery(in)
	if got != ReconcileOnly {
		t.Errorf("protocolFailure non-replayable should take precedence over unfixable; got %q, want %q", got, ReconcileOnly)
	}

	// !Replayable takes precedence over unfixable (path 3 before path 4).
	in = RecoveryDecisionInput{
		Replayable:      false,
		UnfixableVerify: true,
		FailureClass:    FailureExecution,
		Attempt:         1,
		MaxRetries:      3,
	}
	got, _ = DecideRecovery(in)
	if got != ReconcileOnly {
		t.Errorf("!Replayable should take precedence over unfixable; got %q, want %q", got, ReconcileOnly)
	}

	// Unfixable takes precedence over sameFailure (path 4 before path 5).
	in = RecoveryDecisionInput{
		Replayable:          true,
		UnfixableVerify:     true,
		SameFailureRepeated: true,
		FailureClass:        FailureVerify,
		Attempt:             2,
		MaxRetries:          3,
	}
	got, _ = DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("unfixable should take precedence over sameFailure; got %q, want %q", got, ReplanRequired)
	}
}

// TestIsRetryDisposition verifies the helper predicates.
func TestIsRetryDisposition(t *testing.T) {
	if !IsRetryDisposition(RetryWorker) {
		t.Error("IsRetryDisposition(RetryWorker) should be true")
	}
	if IsRetryDisposition(RetryNone) {
		t.Error("IsRetryDisposition(RetryNone) should be false")
	}
	if IsRetryDisposition(ReconcileOnly) {
		t.Error("IsRetryDisposition(ReconcileOnly) should be false")
	}
	if IsRetryDisposition(ReplanRequired) {
		t.Error("IsRetryDisposition(ReplanRequired) should be false")
	}
	if IsRetryDisposition(NeedsHuman) {
		t.Error("IsRetryDisposition(NeedsHuman) should be false")
	}
}

// TestIsStopDisposition verifies the helper predicates.
func TestIsStopDisposition(t *testing.T) {
	for _, d := range []RetryDisposition{RetryNone, ReconcileOnly, ReplanRequired, NeedsHuman} {
		if !IsStopDisposition(d) {
			t.Errorf("IsStopDisposition(%q) should be true", d)
		}
	}
	if IsStopDisposition(RetryWorker) {
		t.Error("IsStopDisposition(RetryWorker) should be false")
	}
}

// TestShouldBlockTask verifies the helper predicate.
func TestShouldBlockTask(t *testing.T) {
	if !ShouldBlockTask(ReconcileOnly) {
		t.Error("ShouldBlockTask(ReconcileOnly) should be true")
	}
	for _, d := range []RetryDisposition{RetryNone, ReplanRequired, NeedsHuman, RetryWorker} {
		if ShouldBlockTask(d) {
			t.Errorf("ShouldBlockTask(%q) should be false", d)
		}
	}
}

// TestDecideRecovery_S11AcceptanceMatrix runs the §11 acceptance test matrix
// cases that are relevant to DecideRecovery. Each case maps a scenario to the
// expected RetryDisposition.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §11, WP-07
func TestDecideRecovery_S11AcceptanceMatrix(t *testing.T) {
	tests := []struct {
		name string
		in   RecoveryDecisionInput
		want RetryDisposition
	}{
		{
			name: "bare executable not in PATH, workdir has same file → environment replan",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureEnvironment, Attempt: 1, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "pipeline upstream command not found → environment replan",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureEnvironment, Attempt: 1, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "malformed typed verifier → contract replan",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "worker succeeded but omitted submit_result (replayable) → reconcile",
			in:   RecoveryDecisionInput{ProtocolFailure: true, Replayable: true, ProtocolRepairRetry: true, FailureClass: FailureProtocol, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},
		{
			name: "non-replayable task timeout → reconcile",
			in:   RecoveryDecisionInput{Replayable: false, FailureClass: FailureTimeout, SideEffect: SideEffectExternalWrite, Attempt: 1, MaxRetries: 3},
			want: ReconcileOnly,
		},
		{
			name: "read-only task timeout (replayable) → retry",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureTimeout, SideEffect: SideEffectNone, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
			want: RetryWorker,
		},
		{
			name: "same fingerprint second failure → replan",
			in:   RecoveryDecisionInput{Replayable: true, SameFailureRepeated: true, FailureClass: FailureExecution, Attempt: 2, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "verifier tail || echo → contract replan",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureContract, Attempt: 1, MaxRetries: 3},
			want: ReplanRequired,
		},
		{
			name: "verifier exit 0 but self-reported failure → verification retry",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureVerify, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
			want: RetryWorker,
		},
		{
			name: "user SIGINT → cancelled none",
			in:   RecoveryDecisionInput{ContextCancelled: true, Replayable: true, FailureClass: FailureCancelled, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "worker self-cancel with live parent → none",
			in:   RecoveryDecisionInput{ContextCancelled: false, Replayable: true, FailureClass: FailureCancelled, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
			want: RetryNone,
		},
		{
			name: "repair submitted progress not final → execution retry (after reclassification)",
			in:   RecoveryDecisionInput{Replayable: true, FailureClass: FailureExecution, EvidenceComplete: true, Attempt: 1, MaxRetries: 3},
			want: RetryWorker,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := DecideRecovery(tt.in)
			if got != tt.want {
				t.Errorf("DecideRecovery() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecideRecovery_FingerprintRepeatDetection verifies that DecideRecovery
// derives repeat detection from the FailureFingerprint/PreviousFingerprint
// inputs (§6.1), not from raw err.Error() comparison. Two differently
// formatted errors that normalize to the same fingerprint must be detected
// as a repeat.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-07 (reviewer P1)
func TestDecideRecovery_FingerprintRepeatDetection(t *testing.T) {
	// Two errors with different formatting but the same normalized
	// fingerprint (the "attempt N" part is volatile and normalized away).
	fp := "ffp_same_digest"

	// With matching fingerprints → ReplanRequired (repeat detected)
	in := RecoveryDecisionInput{
		Replayable:          true,
		FailureClass:        FailureExecution,
		EvidenceComplete:    true,
		FailureFingerprint:  fp,
		PreviousFingerprint: fp,
		Attempt:             2,
		MaxRetries:          3,
	}
	got, _ := DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("matching fingerprints → %q, want ReplanRequired (repeat detected via fingerprint, §6.1)", got)
	}

	// With different fingerprints → RetryWorker (not a repeat)
	in = RecoveryDecisionInput{
		Replayable:          true,
		FailureClass:        FailureExecution,
		EvidenceComplete:    true,
		FailureFingerprint:  "ffp_digest_a",
		PreviousFingerprint: "ffp_digest_b",
		Attempt:             2,
		MaxRetries:          3,
	}
	got, _ = DecideRecovery(in)
	if got != RetryWorker {
		t.Errorf("different fingerprints → %q, want RetryWorker (not a repeat)", got)
	}

	// With empty fingerprints → fallback to SameFailureRepeated
	in = RecoveryDecisionInput{
		Replayable:          true,
		FailureClass:        FailureExecution,
		EvidenceComplete:    true,
		SameFailureRepeated: true,
		Attempt:             2,
		MaxRetries:          3,
	}
	got, _ = DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("empty fingerprints + SameFailureRepeated → %q, want ReplanRequired (fallback)", got)
	}

	// With empty fingerprints and no SameFailureRepeated → RetryWorker
	in = RecoveryDecisionInput{
		Replayable:       true,
		FailureClass:     FailureExecution,
		EvidenceComplete: true,
		Attempt:          2,
		MaxRetries:       3,
	}
	got, _ = DecideRecovery(in)
	if got != RetryWorker {
		t.Errorf("empty fingerprints + no repeat → %q, want RetryWorker", got)
	}
}

// TestDecideRecovery_DifferentlyFormattedErrorsSameFingerprint verifies that
// two errors with different raw text but the same normalized fingerprint
// (e.g. "attempt 1 failed: timeout" and "attempt 2 failed: timeout") are
// detected as a repeat. This is the reviewer's specific request: "add direct
// unit tests that fail if each required input is ignored, including
// differently formatted errors with the same normalized fingerprint."
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-07 (reviewer P1)
func TestDecideRecovery_DifferentlyFormattedErrorsSameFingerprint(t *testing.T) {
	// Simulate the actual normalization: "attempt 1 failed: timeout" and
	// "attempt 2 failed: timeout" both normalize to "attempt=<volatile>
	// failed: timeout".
	err1 := "attempt 1 failed: timeout"
	err2 := "attempt 2 failed: timeout"
	fp1 := NewFailureFingerprint("task-1", "worker", "verify:test", FailureExecution, err1).Digest
	fp2 := NewFailureFingerprint("task-1", "worker", "verify:test", FailureExecution, err2).Digest

	if fp1 != fp2 {
		t.Fatalf("expected same fingerprint for differently formatted errors, got %q vs %q", fp1, fp2)
	}

	// DecideRecovery with matching fingerprints should detect a repeat.
	in := RecoveryDecisionInput{
		Replayable:          true,
		FailureClass:        FailureExecution,
		EvidenceComplete:    true,
		FailureFingerprint:  fp2,
		PreviousFingerprint: fp1,
		Attempt:             2,
		MaxRetries:          3,
	}
	got, _ := DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("differently formatted errors with same fingerprint → %q, want ReplanRequired (repeat detected, §6.1)", got)
	}

	// The old SameFailureRepeated based on raw err.Error() would NOT detect
	// this as a repeat (the strings differ). Verify the fingerprint path
	// is actually used by checking that SameFailureRepeated=false still
	// triggers ReplanRequired via fingerprints.
	in.SameFailureRepeated = false
	got, _ = DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("fingerprint match with SameFailureRepeated=false → %q, want ReplanRequired (fingerprint must be used, not SameFailureRepeated)", got)
	}
}

// TestDecideRecovery_RecoveryPolicyGatesRetry verifies that the resolved
// RecoveryPolicy gates RetryWorker so a profile that resolves to
// reconcile/manual/never cannot be bypassed by the raw task.Recovery field
// (§6.1). This is the reviewer's P1 finding about profile recovery policy
// bypass.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestDecideRecovery_RecoveryPolicyGatesRetry(t *testing.T) {
	tests := []struct {
		name   string
		policy RecoveryPolicy
		want   RetryDisposition
	}{
		{"retry policy allows retry", RecoveryRetry, RetryWorker},
		{"reconcile policy blocks retry", RecoveryReconcile, ReconcileOnly},
		{"manual policy needs human", RecoveryManual, NeedsHuman},
		{"never policy stops", RecoveryNever, RetryNone},
		{"empty policy allows retry (backward compat)", "", RetryWorker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := RecoveryDecisionInput{
				Replayable:       true,
				FailureClass:     FailureExecution,
				RecoveryPolicy:   tt.policy,
				EvidenceComplete: true,
				Attempt:          1,
				MaxRetries:       3,
			}
			got, _ := DecideRecovery(in)
			if got != tt.want {
				t.Errorf("RecoveryPolicy=%q → %q, want %q", tt.policy, got, tt.want)
			}
		})
	}
}

// TestDecideRecovery_EvidenceCompleteGatesRetry verifies that when
// EvidenceComplete is false, DecideRecovery does not prescribe RetryWorker
// (§6.1: retry prompt must include class, evidence, last command/exit).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestDecideRecovery_EvidenceCompleteGatesRetry(t *testing.T) {
	in := RecoveryDecisionInput{
		Replayable:       true,
		FailureClass:     FailureExecution,
		RecoveryPolicy:   RecoveryRetry,
		EvidenceComplete: false,
		Attempt:          1,
		MaxRetries:       3,
	}
	got, _ := DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("EvidenceComplete=false → %q, want ReplanRequired (cannot retry without complete evidence, §6.1)", got)
	}
}

// TestDecideRecovery_AllRequiredInputsUsed verifies that every field
// specified by §6.1 is actually read by DecideRecovery. Each sub-test changes
// one input and verifies the disposition changes accordingly.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-07 (reviewer P1)
func TestDecideRecovery_AllRequiredInputsUsed(t *testing.T) {
	base := RecoveryDecisionInput{
		Replayable:       true,
		FailureClass:     FailureExecution,
		RecoveryPolicy:   RecoveryRetry,
		EvidenceComplete: true,
		Attempt:          1,
		MaxRetries:       3,
	}

	// FailureClass: execution → retry, contract → replan
	if got, _ := DecideRecovery(base); got != RetryWorker {
		t.Errorf("base → %q, want RetryWorker", got)
	}
	contractIn := base
	contractIn.FailureClass = FailureContract
	if got, _ := DecideRecovery(contractIn); got != ReplanRequired {
		t.Errorf("FailureClass=contract → %q, want ReplanRequired", got)
	}

	// SideEffect: none → retry (timeout), external_write → reconcile (timeout)
	timeoutNone := base
	timeoutNone.FailureClass = FailureTimeout
	timeoutNone.SideEffect = SideEffectNone
	if got, _ := DecideRecovery(timeoutNone); got != RetryWorker {
		t.Errorf("timeout+none → %q, want RetryWorker", got)
	}
	timeoutExt := timeoutNone
	timeoutExt.SideEffect = SideEffectExternalWrite
	if got, _ := DecideRecovery(timeoutExt); got != ReconcileOnly {
		t.Errorf("timeout+external → %q, want ReconcileOnly", got)
	}

	// RecoveryPolicy: retry → retry, reconcile → reconcile
	reconIn := base
	reconIn.RecoveryPolicy = RecoveryReconcile
	if got, _ := DecideRecovery(reconIn); got != ReconcileOnly {
		t.Errorf("RecoveryPolicy=reconcile → %q, want ReconcileOnly", got)
	}

	// Attempt/MaxRetries: budget exhausted → none
	budgetIn := base
	budgetIn.Attempt = 3
	budgetIn.MaxRetries = 3
	if got, _ := DecideRecovery(budgetIn); got != RetryNone {
		t.Errorf("budget exhausted → %q, want RetryNone", got)
	}

	// EvidenceComplete: true → retry, false → replan
	noEvidence := base
	noEvidence.EvidenceComplete = false
	if got, _ := DecideRecovery(noEvidence); got != ReplanRequired {
		t.Errorf("EvidenceComplete=false → %q, want ReplanRequired", got)
	}

	// FailureFingerprint/PreviousFingerprint: match → replan (repeat)
	repeatIn := base
	repeatIn.FailureFingerprint = "fp_match"
	repeatIn.PreviousFingerprint = "fp_match"
	repeatIn.Attempt = 2
	if got, _ := DecideRecovery(repeatIn); got != ReplanRequired {
		t.Errorf("matching fingerprints → %q, want ReplanRequired", got)
	}
}
