package team

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestRunTerminationRunsOnePreparationForConcurrentFinishers(t *testing.T) {
	c := &Coordinator{executionRunID: "run-terminal-decision"}
	logicalID := "ldr_01010101010101010101010101010101"
	binding := terminalTestBinding(t, logicalID)
	eventID := "evt-primary-bound"
	var calls atomic.Int32
	preparer := DecisionTerminalPreparerFunc(func(_ context.Context, request DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
		calls.Add(1)
		if request.ExecutionRunID != "run-terminal-decision" || request.Intent.EntryPoint != TerminalEntryFinishTool {
			t.Fatalf("preparation request = %#v", request)
		}
		return DecisionTerminalPreparationResult{
			Action: TerminalPreparationCommitTerminal, PrimaryBinding: &binding,
			PrimaryBindingEventID: &eventID, SupportRevisionDigest: &binding.SupportRevisionDigest,
		}, nil
	})
	if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{LogicalRunID: logicalID, BranchID: "main", Generation: 1, Preparer: preparer}); err != nil {
		t.Fatal(err)
	}
	c.resetDecisionTerminalInvocation("run-terminal-decision")
	candidate := &RunResult{RunID: "run-terminal-decision", Outcome: RunOutcomeCompleted, GoalSatisfied: true}

	const finishers = 8
	results := make(chan TerminalPreparation, finishers)
	var wg sync.WaitGroup
	for range finishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prepared, err := c.RequestRunTermination(context.Background(), TerminalIntent{
				EntryPoint: TerminalEntryFinishTool, Cause: "success_requested", WantsSuccess: true, CanContinueSupporting: true,
			}, candidate, nil)
			if err != nil {
				t.Errorf("RequestRunTermination: %v", err)
			}
			results <- prepared
		}()
	}
	wg.Wait()
	close(results)
	if calls.Load() != 1 {
		t.Fatalf("preparer calls = %d, want 1", calls.Load())
	}
	for prepared := range results {
		if prepared.Action != TerminalPreparationCommitTerminal || prepared.Candidate != candidate || prepared.PrimaryBinding == nil {
			t.Fatalf("preparation = %#v", prepared)
		}
	}
	if candidate.Outcome != RunOutcomeCompleted || !candidate.GoalSatisfied {
		t.Fatalf("candidate = %#v, want completed", candidate)
	}
}

func TestDecisionTerminalPromotesUnverifiedOnlyAfterPrimaryBinding(t *testing.T) {
	c := &Coordinator{executionRunID: "run-unverified-primary"}
	logicalID := "ldr_09090909090909090909090909090909"
	binding := terminalTestBinding(t, logicalID)
	eventID := "evt-primary-bound"
	preparer := DecisionTerminalPreparerFunc(func(context.Context, DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
		return DecisionTerminalPreparationResult{Action: TerminalPreparationCommitTerminal, PrimaryBinding: &binding, PrimaryBindingEventID: &eventID}, nil
	})
	if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{LogicalRunID: logicalID, BranchID: "main", Generation: 1, Preparer: preparer}); err != nil {
		t.Fatal(err)
	}
	c.resetDecisionTerminalInvocation("run-unverified-primary")
	candidate := &RunResult{RunID: "run-unverified-primary", Outcome: RunOutcomeUnverified, ExitCode: 7, Acceptance: &AcceptanceResult{State: AcceptanceNotConfigured}}
	prepared, err := c.PrepareDecisionForTerminal(context.Background(), TerminalIntent{EntryPoint: TerminalEntryFinishTool}, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Candidate == nil || prepared.Candidate.Outcome != RunOutcomeCompleted || !prepared.Candidate.GoalSatisfied || prepared.Candidate.ExitCode != 0 {
		t.Fatalf("prepared candidate = %#v", prepared.Candidate)
	}
}

func TestHardStopCancelsDecisionPreparationAndOwnsTerminalCandidate(t *testing.T) {
	c := &Coordinator{executionRunID: "run-terminal-signal"}
	logicalID := "ldr_02020202020202020202020202020202"
	started := make(chan struct{})
	returned := make(chan struct{})
	preparer := DecisionTerminalPreparerFunc(func(ctx context.Context, _ DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
		close(started)
		<-ctx.Done()
		close(returned)
		return DecisionTerminalPreparationResult{}, context.Cause(ctx)
	})
	if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{LogicalRunID: logicalID, BranchID: "main", Generation: 1, Preparer: preparer}); err != nil {
		t.Fatal(err)
	}
	c.resetDecisionTerminalInvocation("run-terminal-signal")
	success := &RunResult{RunID: "run-terminal-signal", Outcome: RunOutcomeCompleted, GoalSatisfied: true}
	ownerDone := make(chan TerminalPreparation, 1)
	go func() {
		prepared, _ := c.RequestRunTermination(context.Background(), TerminalIntent{EntryPoint: TerminalEntryCoordinatorEOF, WantsSuccess: true}, success, nil)
		ownerDone <- prepared
	}()
	<-started
	cancelled := &RunResult{RunID: "run-terminal-signal", Outcome: RunOutcomeCancelled, ExitCode: 130, Reason: "SIGINT"}
	prepared, err := c.RequestRunTermination(context.Background(), TerminalIntent{EntryPoint: TerminalEntrySignal, Cause: "cancelled"}, cancelled, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("preparer did not observe hard-stop cancellation")
	}
	owner := <-ownerDone
	if prepared.Candidate != cancelled || owner.Candidate != cancelled {
		t.Fatalf("hard-stop candidate mismatch: signal=%p owner=%p want=%p", prepared.Candidate, owner.Candidate, cancelled)
	}
	if cancelled.Outcome != RunOutcomeCancelled || success.Outcome != RunOutcomeCompleted {
		t.Fatalf("terminal candidates mutated: cancelled=%#v success=%#v", cancelled, success)
	}
}

func TestFinalizeRunDecisionIntentRejectsSuccessWithoutPreparationProof(t *testing.T) {
	c := &Coordinator{executionRunID: "run-terminal-missing-proof"}
	if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{
		LogicalRunID: "ldr_03030303030303030303030303030303", BranchID: "main", Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	c.resetDecisionTerminalInvocation("run-terminal-missing-proof")
	result := c.FinalizeRun(context.Background(), &RunResult{
		RunID: "run-terminal-missing-proof", Outcome: RunOutcomeCompleted, GoalSatisfied: true,
	}, nil)
	if result.Outcome != RunOutcomeBlocked || result.GoalSatisfied || result.ExitCode != 7 || result.Reason != ReasonDecisionTerminalPreparationMissing {
		t.Fatalf("FinalizeRun result = %#v", result)
	}
}

func TestEveryTerminalEntryPointUsesItsPrimaryAuthorizationMode(t *testing.T) {
	for _, entryPoint := range TerminalEntryPoints() {
		t.Run(string(entryPoint), func(t *testing.T) {
			logicalID := "ldr_04040404040404040404040404040404"
			binding := terminalTestBinding(t, logicalID)
			eventID := "evt-primary-bound"
			calls := 0
			c := &Coordinator{executionRunID: "run-entry-matrix"}
			if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{
				LogicalRunID: logicalID, BranchID: "main", Generation: 1,
				Preparer: DecisionTerminalPreparerFunc(func(context.Context, DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
					calls++
					return DecisionTerminalPreparationResult{Action: TerminalPreparationCommitTerminal, PrimaryBinding: &binding, PrimaryBindingEventID: &eventID}, nil
				}),
			}); err != nil {
				t.Fatal(err)
			}
			c.resetDecisionTerminalInvocation("run-entry-matrix")
			prepared, err := c.PrepareDecisionForTerminal(context.Background(), TerminalIntent{
				EntryPoint: entryPoint, WantsSuccess: true, CanContinueSupporting: true,
			}, &RunResult{RunID: "run-entry-matrix", Outcome: RunOutcomeCompleted, GoalSatisfied: true})
			if err != nil {
				t.Fatal(err)
			}
			policy, _ := TerminalEntryPolicyFor(entryPoint)
			switch policy.PrimaryMode {
			case TerminalPrimaryStart:
				if calls != 1 || prepared.PrimaryBinding == nil {
					t.Fatalf("calls=%d preparation=%#v, want one bound preparation", calls, prepared)
				}
			case TerminalPrimaryResumeOnly, TerminalPrimaryForbidden:
				if calls != 0 || prepared.Candidate == nil || prepared.Candidate.Outcome != RunOutcomeBlocked {
					t.Fatalf("calls=%d preparation=%#v, want local blocked terminal", calls, prepared)
				}
			default:
				t.Fatalf("unexpected primary mode %q", policy.PrimaryMode)
			}
		})
	}
}

func TestSetLastRunResultDoesNotPublishUnpreparedDecisionSuccess(t *testing.T) {
	c := &Coordinator{}
	if err := c.ConfigureDecisionTerminal(DecisionTerminalConfig{
		LogicalRunID: "ldr_05050505050505050505050505050505", BranchID: "main", Generation: 1,
	}); err != nil {
		t.Fatal(err)
	}
	c.resetDecisionTerminalInvocation("run-set-last")
	c.SetLastRunResult(&RunResult{RunID: "run-set-last", Outcome: RunOutcomeCompleted, GoalSatisfied: true})
	got := c.LastRunResult()
	if got == nil || got.Outcome != RunOutcomeBlocked || got.GoalSatisfied || got.Reason != ReasonDecisionTerminalPreparationMissing {
		t.Fatalf("LastRunResult = %#v", got)
	}
}

func terminalTestBinding(t *testing.T, logicalID string) PrimaryBindingV1 {
	t.Helper()
	taskID, err := PrimaryDecisionTaskID("main", logicalID)
	if err != nil {
		t.Fatal(err)
	}
	decisionID, err := PrimaryDecisionID("main", logicalID, 1)
	if err != nil {
		t.Fatal(err)
	}
	ref := DecisionArtifactRef{ID: "artifact-1", SHA256: testDigestA, MediaType: "application/json", SizeBytes: 1}
	return PrimaryBindingV1{
		SchemaVersion: 1, LogicalRunID: logicalID, BranchID: "main", TaskID: taskID, Generation: 1,
		DecisionID: decisionID, RequirementDigest: testDigestA, SupportRevisionDigest: testDigestB,
		AdmissionRef: ref, BaseEvidenceRef: ref, RolePlanRef: ref, RecordRef: ref, SealedEvidenceHash: testDigestC,
	}
}
