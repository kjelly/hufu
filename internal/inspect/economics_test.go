package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/team"
)

// The synthetic snapshot deliberately uses the production snapshot validator.
// It describes only the admitted LLM boundary, without initializing a provider.
func economicsFixtureEvents(t *testing.T, events []team.RunEvent) []team.RunEvent {
	t.Helper()
	catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{
		"ollama/fixture": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	price, _ := catalog.Resolve("ollama/fixture")
	policy, err := cost.NewPolicySnapshot(cost.RunPolicy{Configured: true}, []cost.PriceSnapshot{price})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := team.ExecutionPolicySnapshot{
		Version: 5, TeamMaxConcurrent: 1, DefaultLLMBackend: "ollama", Cost: policy,
		Backends:        []team.ExecutionBackendPolicySnapshot{{Backend: "ollama", Kind: "llm", IdentityHash: "fixture"}},
		ExecutionWorlds: []team.ExecutionWorldPolicySnapshot{},
	}
	snapshot.ConfigurationHash = fmt.Sprintf("%x", sha256.Sum256(jsonBytes(t, snapshot)))
	if err := team.ValidateExecutionPolicySnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	prefix := []team.RunEvent{{Type: string(team.EventExecutionPolicySnapshot), Payload: jsonBytes(t, snapshot)}}
	for i, amount := range []int64{7, 11, 3} {
		identity := cost.InvocationIdentity{
			ProviderInvocationID: fmt.Sprintf("invocation-%d", i), RunID: "run-evidence",
			TaskID: "task-evidence", OccurrenceAttempt: 1, Agent: "worker", Role: "worker", Purpose: cost.PurposeWorker,
		}
		outcome := cost.OutcomeSuccess
		if i == 0 {
			outcome = cost.OutcomeProviderError
		}
		if i == 2 {
			identity.TaskID, identity.OccurrenceAttempt = "", 0
			identity.Agent, identity.Role, identity.Purpose = "coordinator", "coordinator", cost.PurposeCoordinator
		}
		prefix = append(prefix, team.RunEvent{Type: string(team.EventModelProfileResolved),
			Payload: jsonBytes(t, modelprofile.TelemetryProjection{SchemaVersion: 1, InvocationID: identity.ProviderInvocationID,
				ModelID: "fixture", Provider: "ollama"})})
		costEvents := []cost.Event{
			{Kind: cost.EventPriceSnapshotResolved, Price: &cost.PriceSnapshotResolvedEvent{
				SchemaVersion: 1, RunID: identity.RunID, Price: price}},
			{Kind: cost.EventReservationCommitted, Reservation: &cost.ReservationEvent{
				SchemaVersion: 1, InvocationIdentity: identity, ExecutionTarget: price.ExecutionTarget,
				PriceSnapshotID: price.ID, BillingMode: cost.BillingMetered, EstimateSource: cost.EstimateAdmissionBound,
				ReservedMicros: new(amount), EstimatedInputTokens: amount, ReservedAt: "2026-09-24T00:00:00Z"}},
			{Kind: cost.EventSettled, Settlement: &cost.SettlementEvent{
				SchemaVersion: 1, InvocationIdentity: identity, ExecutionTarget: price.ExecutionTarget,
				PriceSnapshotID: price.ID, BillingMode: cost.BillingMetered, EstimateSource: cost.EstimateUsage,
				FinalMicros: new(amount), Usage: &cost.TokenUsage{InputTokens: amount}, Outcome: outcome,
				SettledAt: "2026-09-24T00:00:01Z"}},
		}
		for _, event := range costEvents {
			payload, err := cost.EncodeEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			envelope := team.RunEvent{Type: string(event.Kind), Payload: payload}
			if event.Kind != cost.EventPriceSnapshotResolved {
				envelope.TaskID, envelope.Attempt = identity.TaskID, identity.OccurrenceAttempt
			}
			prefix = append(prefix, envelope)
		}
	}
	// Policy precedes all task/provider execution. Cost events precede terminal.
	for i := range prefix {
		prefix[i].Actor = "coordinator"
	}
	return append(append(events[:1:1], prefix...), events[1:]...)
}

func TestInspectEconomicsCompleteAndReadOnly(t *testing.T) {
	fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
		return economicsFixtureEvents(t, events)
	})
	before := snapshotInspectWorkspace(t, fixture.workspace)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	first, err := InspectEconomics(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := first.Data.(EconomicsData)
	if data.CostPerVerifiedTaskMicros == nil || *data.CostPerVerifiedTaskMicros != 21 ||
		data.UsageCostMicros == nil || *data.UsageCostMicros != 21 ||
		data.InitialCostMicros == nil || *data.InitialCostMicros != 18 ||
		data.OverheadUsageCostMicros == nil || *data.OverheadUsageCostMicros != 3 ||
		data.VerifiedTaskRate == nil || *data.VerifiedTaskRate != 1 ||
		data.KnownCostCoverage == nil || *data.KnownCostCoverage != 1 {
		t.Fatalf("expected complete 7 + 11 + 3 with overhead excluded from route: %+v", data)
	}
	if len(data.Routes) != 1 || data.Routes[0].UsageCostMicros == nil || *data.Routes[0].UsageCostMicros != 18 ||
		data.Latency.SampleCount != 1 || data.Latency.P95Millis == nil || *data.Latency.P95Millis != 1000 {
		t.Fatalf("route/latency mismatch: %+v", data)
	}
	second, err := InspectEconomics(t.Context(), query)
	if err != nil || !reflect.DeepEqual(first, second) || !reflect.DeepEqual(before, snapshotInspectWorkspace(t, fixture.workspace)) {
		t.Fatalf("unstable or mutating inspection: %v", err)
	}
}

func TestInspectEconomicsPartialAccounting(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		reason string
	}{
		{"missing_policy", string(team.EventExecutionPolicySnapshot), "historical_coverage_unknown"},
		{"missing_profile", string(team.EventModelProfileResolved), "historical_coverage_unknown"},
		{"open_reservation", string(team.EventCostSettled), "open_reservation"},
		{"running", string(team.EventRunFinished), "run_in_progress"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
				return slices.DeleteFunc(economicsFixtureEvents(t, events), func(event team.RunEvent) bool {
					return event.Type == test.remove
				})
			})
			envelope, err := InspectEconomics(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
			if err != nil {
				t.Fatal(err)
			}
			data := envelope.Data.(EconomicsData)
			if data.CostPerVerifiedTaskMicros != nil || !slices.Contains(data.CostPerVerifiedTaskReasons, test.reason) {
				t.Fatalf("missing qualification: %+v", data)
			}
		})
	}
}

func TestInspectEconomicsUnknownAndLegacy(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		fixture := observabilityCostFixture(t, mixed)
		envelope, err := InspectEconomics(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
		if err != nil {
			t.Fatal(err)
		}
		data := envelope.Data.(EconomicsData)
		if data.TaskCount != 1 || data.VerifiedTaskCount != 0 || data.UsageCostMicros == nil || *data.UsageCostMicros != 21 ||
			data.CostPerVerifiedTaskMicros != nil || !slices.Contains(data.CostPerVerifiedTaskReasons, "historical_coverage_unknown") {
			t.Fatalf("legacy completeness invented: %+v", data)
		}
		if mixed && (data.RecordedInvocations != 7 || data.UsageInvocations != 3 || *data.KnownCostCoverage != 3.0/7 ||
			data.Cost.LocalInvocations != 1 || data.Cost.SubscriptionInvocations != 1 || data.Cost.UnknownInvocations != 1) {
			t.Fatalf("unknown amounts counted as zero: %+v", data)
		}
	}
	fixture := observabilityEvidenceFixture(t)
	envelope, err := InspectEconomics(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(EconomicsData)
	if data.UsageCostMicros != nil || data.KnownCostCoverage != nil || data.CostPerVerifiedTaskMicros != nil {
		t.Fatalf("missing ledger fabricated zero: %+v", data)
	}
}

func TestEconomicsLatencyAndOverflow(t *testing.T) {
	var attempts []RouteAttempt
	for i := 1; i <= 20; i++ {
		attempts = append(attempts, RouteAttempt{Counted: true, WorkerAttemptView: WorkerAttemptView{
			StartedAt: workerEpoch, FinishedAt: workerEpoch.Add(time.Duration(i) * time.Millisecond),
		}})
	}
	attempts = append(attempts, RouteAttempt{Counted: true}, RouteAttempt{Counted: true,
		WorkerAttemptView: WorkerAttemptView{StartedAt: workerEpoch, FinishedAt: workerEpoch.Add(-time.Second)}})
	latency := economicsLatency(attempts)
	if latency.SampleCount != 20 || latency.ExcludedCount != 2 || *latency.P50Millis != 10 || *latency.P95Millis != 19 {
		t.Fatalf("nearest rank mismatch: %+v", latency)
	}
	value := new(int64(math.MaxInt64))
	if err := addEconomicsMicros(&value, new(int64(1))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("overflow accepted: %v", err)
	}
	if err := addEconomicsMicros(&value, new(int64(-1))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("negative accepted: %v", err)
	}
}

func TestInspectEconomicsDuplicateSettlementAndScope(t *testing.T) {
	fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
		events = economicsFixtureEvents(t, events)
		for i, event := range events {
			if event.Type == string(team.EventCostSettled) {
				return slices.Insert(events, i+1, event)
			}
		}
		t.Fatal("missing settlement")
		return nil
	})
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	envelope, err := InspectEconomics(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(EconomicsData)
	if data.RecordedInvocations != 3 || *data.UsageCostMicros != 21 {
		t.Fatalf("duplicate settlement counted twice: %+v", data)
	}
	for _, modify := range []func(*InspectQuery){
		func(q *InspectQuery) { q.TaskID = fixture.taskID },
		func(q *InspectQuery) { q.Attempt = 1 },
		func(q *InspectQuery) { q.AgentID = "worker" },
		func(q *InspectQuery) { q.ProjectID = "project" },
		func(q *InspectQuery) { q.TeamID = "team" },
		func(q *InspectQuery) { q.AllBranches = true },
		func(q *InspectQuery) { q.RunID = "" },
	} {
		invalid := query
		modify(&invalid)
		if _, err := InspectEconomics(t.Context(), invalid); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid scope accepted: %+v: %v", invalid, err)
		}
	}
	store, err := team.NewEventStore(fixture.workspace, "run-later", "session-evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "later"})},
		{Type: "task_failed", Actor: "worker", TaskID: fixture.taskID, Payload: jsonBytes(t, map[string]any{
			"id": fixture.taskID, "status": team.TaskError,
		})},
		{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
			RunID: "run-later", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
		})},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	historical, err := InspectEconomics(t.Context(), query)
	if err != nil || !reflect.DeepEqual(historical, envelope) {
		t.Fatalf("later run changed historical economics: %v", err)
	}
	query.RunID = "run-later"
	current, err := InspectEconomics(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	later := current.Data.(EconomicsData)
	if later.TaskCount != 1 || later.UsageCostMicros != nil ||
		!slices.Contains(later.CostPerVerifiedTaskReasons, "cross_run_scope") {
		t.Fatalf("cross-run task reused old cost: %+v", later)
	}
}

func TestEconomicsAmbiguousFallbackKeepsOccurrenceSubtotal(t *testing.T) {
	invocation := cost.InvocationProjection{
		Reservation: cost.ReservationEvent{InvocationIdentity: cost.InvocationIdentity{
			RunID: "run", TaskID: "task", OccurrenceAttempt: 1, Purpose: cost.PurposeWorker,
		}, ExecutionTarget: "ollama/model"},
		Settlement: &cost.SettlementEvent{EstimateSource: cost.EstimateUsage, FinalMicros: new(int64(17))},
	}
	attempt := RouteAttempt{Counted: true, AttemptClass: "initial", RouteKind: "unbound",
		WorkerAttemptView: WorkerAttemptView{InvocationRunID: "run", TaskID: "task", OccurrenceAttempt: 1, ExecutionTarget: "ollama/model"}}
	fallback := attempt
	fallback.AttemptClass = "fallback"
	group := RouteGroup{RouteKind: "unbound", ExecutionTarget: "ollama/model"}
	ledger := cost.Projection{Invocations: map[string]cost.InvocationProjection{"invocation": invocation}}
	data := EconomicsData{RecordedInvocations: 1}
	if err := economicsAttribution(&data, RoutesData{Attempts: []RouteAttempt{attempt, fallback}, Groups: []RouteGroup{group}}, ledger); err != nil {
		t.Fatal(err)
	}
	if data.FallbackCostMicros != nil || data.InitialCostMicros != nil || data.UnassignedInvocations != 1 ||
		data.UnassignedUsageCostMicros == nil || *data.UnassignedUsageCostMicros != 17 ||
		len(data.Occurrences) != 1 || *data.Occurrences[0].UsageCostMicros != 17 ||
		data.Routes[0].UsageCostMicros != nil || data.BreakdownReason != "attribution_unavailable" {
		t.Fatalf("ambiguous fallback attributed: %+v", data)
	}
	for _, class := range []string{"initial", "retry", "fallback"} {
		attempt.AttemptClass = class
		data := EconomicsData{RecordedInvocations: 1}
		if err := economicsAttribution(&data, RoutesData{Attempts: []RouteAttempt{attempt}, Groups: []RouteGroup{group}}, ledger); err != nil {
			t.Fatal(err)
		}
		if data.InitialCostMicros == nil || data.RetryCostMicros == nil || data.FallbackCostMicros == nil ||
			*data.InitialCostMicros+*data.RetryCostMicros+*data.FallbackCostMicros != 17 {
			t.Fatalf("classification is not mutually exclusive for %s: %+v", class, data)
		}
	}
}

func TestInspectEconomicsEmptyRun(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "empty", "session")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "empty"})},
		{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
			RunID: "empty", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
		})},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectEconomics(t.Context(), InspectQuery{Workspace: workspace, RunID: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(EconomicsData)
	if data.TaskCount != 0 || data.VerifiedTaskRate != nil || data.KnownCostCoverage != nil ||
		data.Latency.SampleCount != 0 || data.Latency.P50Millis != nil || data.Latency.Reason != "no_samples" {
		t.Fatalf("empty run invented observations: %+v", data)
	}
}

func TestInspectEconomicsUnrecordedInvocationAndPriceOnly(t *testing.T) {
	for _, priceOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("price_only=%t", priceOnly), func(t *testing.T) {
			fixture := observabilityEvidenceFixtureWithEvents(t, func(events []team.RunEvent) []team.RunEvent {
				events = economicsFixtureEvents(t, events)
				return slices.DeleteFunc(events, func(event team.RunEvent) bool {
					if event.Type != string(team.EventCostReservationCommitted) && event.Type != string(team.EventCostSettled) {
						return false
					}
					decoded, err := cost.DecodeEvent(event.Type, event.Payload)
					if err != nil {
						t.Fatal(err)
					}
					id := ""
					if decoded.Reservation != nil {
						id = decoded.Reservation.ProviderInvocationID
					}
					if decoded.Settlement != nil {
						id = decoded.Settlement.ProviderInvocationID
					}
					return priceOnly || id == "invocation-0"
				})
			})
			envelope, err := InspectEconomics(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
			if err != nil {
				t.Fatal(err)
			}
			data := envelope.Data.(EconomicsData)
			if !data.Cost.Available || data.CostPerVerifiedTaskMicros != nil ||
				!slices.Contains(data.CostPerVerifiedTaskReasons, "historical_coverage_unknown") {
				t.Fatalf("settled ledger incorrectly proves historical completeness: %+v", data)
			}
			if priceOnly {
				if data.UsageCostMicros != nil || data.KnownCostCoverage != nil {
					t.Fatal("prices alone fabricated usage")
				}
			} else if data.KnownCostCoverage == nil || *data.KnownCostCoverage != 1 || *data.UsageCostMicros != 14 {
				t.Fatalf("known subtotal lost: %+v", data)
			}
		})
	}
}
