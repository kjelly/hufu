package inspect

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// observabilityCostFixture records failures, overhead and unknown prices through
// the production event writer. Values are synthetic USD micros, not model costs.
func observabilityCostFixture(t *testing.T, mixed bool) runFixture {
	t.Helper()
	fixture := runFixture{workspace: t.TempDir(), runID: "run-observability", taskID: "task-observability"}
	store, err := team.NewEventStore(fixture.workspace, fixture.runID, "session-observability")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "synthetic fixture"})})
	appendEvent(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: fixture.taskID, Payload: jsonBytes(t, map[string]any{
		"id": fixture.taskID, "agent": "worker", "status": team.TaskPending,
	})})

	type invocation struct {
		mode    cost.BillingMode
		outcome cost.Outcome
		amount  int64
		open    bool
		task    bool
	}
	calls := []invocation{
		{mode: cost.BillingMetered, outcome: cost.OutcomeProviderError, amount: 7, task: true},
		{mode: cost.BillingMetered, outcome: cost.OutcomeSuccess, amount: 11, task: true},
		{mode: cost.BillingMetered, outcome: cost.OutcomeSuccess, amount: 3},
	}
	if mixed {
		calls = append(calls,
			invocation{mode: cost.BillingUnknown, outcome: cost.OutcomeSuccess, task: true},
			invocation{mode: cost.BillingLocal, outcome: cost.OutcomeSuccess, task: true},
			invocation{mode: cost.BillingSubscription, outcome: cost.OutcomeSuccess, task: true},
			invocation{mode: cost.BillingMetered, amount: 100, open: true, task: true},
		)
	}
	for index, call := range calls {
		target := fmt.Sprintf("ollama/fixture-%d", index)
		config := cost.PriceConfig{BillingMode: call.mode}
		if call.mode == cost.BillingMetered {
			config.InputUSDPerMillion, config.OutputUSDPerMillion = "1", "1"
		}
		var price cost.PriceSnapshot
		if call.mode == cost.BillingUnknown {
			price, err = cost.NewUnknownPriceSnapshot(target)
		} else {
			var catalog cost.Catalog
			catalog, err = cost.NewCatalog(map[string]cost.PriceConfig{target: config})
			if err == nil {
				price, _ = catalog.Resolve(target)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		identity := cost.InvocationIdentity{
			ProviderInvocationID: fmt.Sprintf("invocation-%d", index), RunID: fixture.runID,
			Agent: "coordinator", Role: "coordinator", Purpose: cost.PurposeCoordinator,
		}
		if call.task {
			identity.TaskID, identity.OccurrenceAttempt = fixture.taskID, 1
			identity.Agent, identity.Role, identity.Purpose = "worker", "worker", cost.PurposeWorker
		}
		source := map[cost.BillingMode]cost.EstimateSource{
			cost.BillingMetered: cost.EstimateAdmissionBound, cost.BillingUnknown: cost.EstimateUnknown,
			cost.BillingLocal: cost.EstimateNotMetered, cost.BillingSubscription: cost.EstimateSubscription,
		}[call.mode]
		var amount *int64
		if call.mode == cost.BillingMetered {
			amount = new(call.amount)
		}
		events := []cost.Event{
			{Kind: cost.EventPriceSnapshotResolved, Price: &cost.PriceSnapshotResolvedEvent{
				SchemaVersion: cost.EventSchemaVersion, RunID: fixture.runID, Price: price,
			}},
			{Kind: cost.EventReservationCommitted, Reservation: &cost.ReservationEvent{
				SchemaVersion: cost.EventSchemaVersion, InvocationIdentity: identity,
				ExecutionTarget: target, PriceSnapshotID: price.ID, ReservedMicros: amount,
				EstimatedInputTokens: call.amount,
				EstimateSource:       source, BillingMode: call.mode, ReservedAt: "2026-10-20T00:00:00Z",
			}},
		}
		if !call.open {
			if call.mode == cost.BillingMetered {
				source = cost.EstimateUsage
			}
			events = append(events, cost.Event{Kind: cost.EventSettled, Settlement: &cost.SettlementEvent{
				SchemaVersion: cost.EventSchemaVersion, InvocationIdentity: identity,
				ExecutionTarget: target, PriceSnapshotID: price.ID, FinalMicros: amount,
				Usage: &cost.TokenUsage{InputTokens: call.amount}, EstimateSource: source, BillingMode: call.mode,
				Outcome: call.outcome, SettledAt: "2026-10-20T00:00:01Z",
			}})
		}
		for _, event := range events {
			payload, encodeErr := cost.EncodeEvent(event)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			envelope := team.RunEvent{Type: string(event.Kind), Actor: identity.Agent, Payload: payload}
			if event.Kind != cost.EventPriceSnapshotResolved {
				envelope.TaskID, envelope.Attempt = identity.TaskID, identity.OccurrenceAttempt
			}
			appendEvent(envelope)
		}
	}
	// A done task without objective verification must never imply a verified task.
	appendEvent(team.RunEvent{Type: "task_completed", Actor: "worker", TaskID: fixture.taskID, Payload: jsonBytes(t, map[string]any{
		"id": fixture.taskID, "agent": "worker", "status": team.TaskDone,
	})})
	appendEvent(team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
		RunID: fixture.runID, Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
	})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestObservabilityCostFixturePreservesFailureAndOverhead(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			fixture := observabilityCostFixture(t, mixed)
			before := snapshotInspectWorkspace(t, fixture.workspace)
			query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
			first, err := InspectCost(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			second, err := InspectCost(t.Context(), query)
			if err != nil || !reflect.DeepEqual(first, second) {
				t.Fatalf("unstable replay: %v", err)
			}
			view := first.Data.(CostData)
			if view.UsageDerivedMicros == nil || *view.UsageDerivedMicros != 21 {
				t.Fatalf("failure + success + overhead must equal 7 + 11 + 3: %+v", view)
			}
			if mixed && (view.OpenReservationCount != 1 || view.UnknownInvocations != 1 ||
				view.LocalInvocations != 1 || view.SubscriptionInvocations != 1 ||
				view.OpenReservationMicros == nil || *view.OpenReservationMicros != 100) {
				t.Fatalf("non-usage amounts must remain distinct: %+v", view)
			}
			query.TaskID = fixture.taskID
			task, err := InspectCost(t.Context(), query)
			if err != nil {
				t.Fatal(err)
			}
			taskView := task.Data.(CostData)
			if taskView.UsageDerivedMicros == nil || *taskView.UsageDerivedMicros != 18 {
				t.Fatalf("task cost must exclude coordinator overhead: %+v", taskView)
			}
			if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
				t.Fatal("cost inspection changed fixture files")
			}
		})
	}
}

// observabilityEvidenceFixture supplies real CAS evidence and a persisted
// objective verification result. No verifier command is executed to build it.
func observabilityEvidenceFixture(t *testing.T, routes ...*team.ExecutionRouteBinding) runFixture {
	t.Helper()
	fixture := runFixture{workspace: t.TempDir(), runID: "run-evidence", taskID: "task-evidence"}
	store, err := team.NewEventStore(fixture.workspace, fixture.runID, "session-evidence")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := team.NewFileArtifactStore(fixture.workspace, fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	put, err := artifacts.Put(t.Context(), team.PutArtifactRequest{
		Kind: "task_transcript", Path: "transcript.txt", Content: []byte("synthetic transcript"),
		RunID: fixture.runID, TaskID: fixture.taskID, Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := execution.ExecutionTarget{Backend: "ollama", Model: "fixture"}
	receipt := team.ExecutionReceipt{
		RunID: fixture.runID, TaskID: fixture.taskID, Attempt: 1, OccurrenceAttempt: 1,
		ModelExecutionID: "model-evidence", ProducerID: "worker", TranscriptRef: put.ArtifactRef.ID,
		ExecutionTarget: target, Backend: target.Backend, ExitCode: new(0),
		StartedAt: workerEpoch, FinishedAt: workerEpoch.Add(time.Second),
		VerifyResult: &team.VerificationResult{Command: "fixture-verifier-must-not-run", ExitCode: 0, Fingerprint: "fixture-verification"},
	}
	var route *team.ExecutionRouteBinding
	var candidates []execution.ExecutionTarget
	if len(routes) > 0 {
		route = routes[0]
		candidates = route.Candidates
		receipt.CandidateIndex = new(0)
	}
	manifest := &team.EvidenceManifest{
		RunID: fixture.runID, Status: "accepted", ArtifactRefs: []team.ArtifactRef{put.ArtifactRef},
		EvidenceResults: []team.EvidenceResult{
			{RequirementID: "task:" + fixture.taskID, Status: "passed", Validator: "task-verification",
				ArtifactRefs: []team.ArtifactRef{put.ArtifactRef}, CheckedAt: workerEpoch,
				Binding: &team.EvidenceBinding{
					RunID: fixture.runID, TaskID: fixture.taskID, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID,
					ProducerID: receipt.ProducerID, TranscriptRef: receipt.TranscriptRef, ArtifactIDs: []string{put.ArtifactRef.ID},
				}},
			{RequirementID: "run:acceptance", Status: "passed", Validator: "acceptance-gate", CheckedAt: workerEpoch},
		},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(t.Context(), artifacts); err != nil {
		t.Fatal(err)
	}
	events := []team.RunEvent{
		{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "fixture"})},
		{Type: "task_created", Actor: "coordinator", TaskID: fixture.taskID, Payload: jsonBytes(t, map[string]any{
			"id": fixture.taskID, "agent": "worker", "status": team.TaskPending, "execution_target": target,
			"execution_topology": []execution.ExecutionTarget{target},
			"execution_route":    route, "execution_candidates": candidates,
		})},
		{Type: "task_started", Actor: "coordinator", TaskID: fixture.taskID, Attempt: 1, Payload: jsonBytes(t, map[string]any{
			"id": fixture.taskID, "agent": "worker", "status": team.TaskInProgress, "attempt": 1, "dispatch_attempt": 1,
		})},
		{Type: "task_completed", Actor: "worker", TaskID: fixture.taskID, Attempt: 1, Payload: jsonBytes(t, map[string]any{
			"id": fixture.taskID, "agent": "worker", "status": team.TaskDone, "execution_target": target,
			"execution_topology": []execution.ExecutionTarget{target},
			"execution_route":    route, "execution_candidates": candidates,
			"execution_receipts": []team.ExecutionReceipt{receipt},
		})},
		{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
			RunID: fixture.runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true, StopReason: team.StopReasonCompleted,
			Acceptance:       &team.AcceptanceResult{State: team.AcceptancePassed, Passed: true},
			EvidenceManifest: manifest, Stats: team.RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
		})},
	}
	for _, event := range events {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestObservabilityEvidenceFixtureAuditsOffline(t *testing.T) {
	fixture := observabilityEvidenceFixture(t)
	before := snapshotInspectWorkspace(t, fixture.workspace)
	result, err := auditverify.VerifyWorkspaceRun(t.Context(), fixture.workspace, fixture.runID, auditverify.VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != auditverify.AuditVerdictPass {
		t.Fatalf("fixture must have valid evidence: %+v", result)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatal("offline evidence audit changed fixture files")
	}
}
