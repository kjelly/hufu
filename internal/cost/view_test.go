package cost

import "testing"

func TestBuildViewSeparatesUsageBoundsOpenAndNonNumericModes(t *testing.T) {
	projection := NewProjection()
	projection.Available = true
	usageMicros, boundMicros, openMicros := int64(300_000), int64(100_000), int64(20_000)
	projection.Invocations = map[string]InvocationProjection{
		"usage": costViewInvocation("run-1", "task-1", EstimateUsage, BillingMetered, usageMicros, false),
		"bound": costViewInvocation("run-1", "task-1", EstimateAdmissionBound, BillingMetered, boundMicros, false),
		"open":  costViewInvocation("run-1", "task-2", EstimateAdmissionBound, BillingMetered, openMicros, true),
		"local": costViewNonNumericInvocation("run-1", "task-1", EstimateNotMetered, BillingLocal),
		"sub":   costViewNonNumericInvocation("run-1", "task-1", EstimateSubscription, BillingSubscription),
		"unk":   costViewNonNumericInvocation("run-1", "task-1", EstimateUnknown, BillingUnknown),
		"other": costViewInvocation("run-other", "task-1", EstimateUsage, BillingMetered, 900_000, false),
	}
	budget := int64(1_000_000)
	policy := &PolicySnapshot{MaxRunMicros: &budget, UnknownPricePolicy: UnknownPriceDeny}
	policy.PolicyHash, _ = policySnapshotHash(policy)

	view, err := BuildView(projection, policy, "run-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.KnownMicros == nil || *view.KnownMicros != 400_000 || view.UsageDerivedMicros == nil || *view.UsageDerivedMicros != usageMicros || view.AdmissionBoundMicros == nil || *view.AdmissionBoundMicros != boundMicros {
		t.Fatalf("task totals = %#v", view)
	}
	if view.OpenReservationMicros != nil || view.OpenReservationCount != 0 {
		t.Fatalf("task open totals = %#v", view)
	}
	if view.LocalInvocations != 1 || view.SubscriptionInvocations != 1 || view.UnknownInvocations != 1 {
		t.Fatalf("task modes = %#v", view)
	}
	if view.RemainingMicros == nil || *view.RemainingMicros != 580_000 {
		t.Fatalf("run-wide remaining = %#v", view.RemainingMicros)
	}
}

func TestBuildViewKeepsOldProjectionUnavailable(t *testing.T) {
	view, err := BuildView(NewProjection(), nil, "run-old", "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Available || view.KnownMicros != nil || view.RemainingMicros != nil || view.Integrity != "ok" {
		t.Fatalf("old workspace view = %#v", view)
	}
}

func costViewInvocation(runID, taskID string, source EstimateSource, mode BillingMode, micros int64, open bool) InvocationProjection {
	identity := InvocationIdentity{ProviderInvocationID: taskID + string(source), RunID: runID, TaskID: taskID, OccurrenceAttempt: 1, Agent: "worker", Role: "worker", Purpose: PurposeWorker}
	reservation := ReservationEvent{InvocationIdentity: identity, ExecutionTarget: "openai/gpt", ReservedMicros: new(micros), EstimateSource: EstimateAdmissionBound, BillingMode: mode}
	result := InvocationProjection{Reservation: reservation}
	if !open {
		result.Settlement = &SettlementEvent{InvocationIdentity: identity, ExecutionTarget: "openai/gpt", FinalMicros: new(micros), EstimateSource: source, BillingMode: mode}
	}
	return result
}

func costViewNonNumericInvocation(runID, taskID string, source EstimateSource, mode BillingMode) InvocationProjection {
	identity := InvocationIdentity{ProviderInvocationID: taskID + string(source), RunID: runID, TaskID: taskID, OccurrenceAttempt: 1, Agent: "worker", Role: "worker", Purpose: PurposeWorker}
	return InvocationProjection{
		Reservation: ReservationEvent{InvocationIdentity: identity, ExecutionTarget: "local/model", EstimateSource: source, BillingMode: mode},
		Settlement:  &SettlementEvent{InvocationIdentity: identity, ExecutionTarget: "local/model", EstimateSource: source, BillingMode: mode},
	}
}
