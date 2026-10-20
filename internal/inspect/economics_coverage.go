package inspect

import (
	"encoding/json/v2"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/team"
)

func economicsCompleteness(runID string, events []team.RunEvent, policy *team.ExecutionPolicySnapshot,
	ledger cost.Projection, routes RoutesData, data EconomicsData,
) []string {
	reasons := []string{}
	terminal := false
	for _, event := range events {
		if event.RunID == runID && event.Type == string(team.EventRunFinished) {
			terminal = true
		}
	}
	if !terminal {
		reasons = append(reasons, "run_in_progress")
	}
	if !economicsHistoricalCoverage(runID, events, policy, ledger, routes) {
		reasons = append(reasons, "historical_coverage_unknown")
	}
	if data.Cost.OpenReservationCount > 0 {
		reasons = append(reasons, "open_reservation")
	}
	if data.UsageCostMicros == nil || data.UsageInvocations != data.RecordedInvocations {
		reasons = append(reasons, "unknown_cost")
	}
	for _, task := range routes.Tasks {
		if task.CrossRunScope {
			reasons = append(reasons, "cross_run_scope")
			break
		}
	}
	if data.VerifiedTaskCount == 0 {
		reasons = append(reasons, "no_samples")
	}
	return reasons
}

// v5 freezes cost admission before execution. The universal LLM boundary
// commits a profile invocation before reservation; external-agent admission
// reserves before process launch. Check both recorded execution surfaces,
// rather than treating a settled ledger alone as proof of complete accounting.
func economicsHistoricalCoverage(runID string, events []team.RunEvent, policy *team.ExecutionPolicySnapshot,
	ledger cost.Projection, routes RoutesData,
) bool {
	if policy == nil || policy.Version != 5 || policy.Cost == nil {
		return false
	}
	profiles := map[string]bool{}
	admitted := false
	for _, event := range events {
		if event.Type == string(team.EventExecutionPolicySnapshot) {
			var snapshot team.ExecutionPolicySnapshot
			if json.Unmarshal(event.Payload, &snapshot) != nil {
				return false
			}
			admitted = snapshot.Version == 5 && snapshot.Cost != nil
		}
		if event.RunID != runID {
			continue
		}
		switch event.Type {
		case string(team.EventTaskStarted), string(team.EventCostReservationCommitted):
			if !admitted {
				return false
			}
		case string(team.EventModelProfileResolved):
			var profile modelprofile.TelemetryProjection
			if !admitted || json.Unmarshal(event.Payload, &profile) != nil || profile.SchemaVersion != 1 ||
				profile.InvocationID == "" || strings.HasPrefix(profile.InvocationID, "legacy-profile-") {
				return false
			}
			if _, ok := ledger.Invocation(runID, profile.InvocationID); !ok {
				// Denied admission is a proven no-send path.
				if _, denied := ledger.Denial(runID, profile.InvocationID); !denied {
					return false
				}
			}
			profiles[profile.InvocationID] = true
		}
	}
	for _, invocation := range ledger.Invocations {
		if !economicsAdmittedBackend(policy, invocation.Reservation) {
			return false
		}
		if invocation.Reservation.Purpose != cost.PurposeExternal && !profiles[invocation.Reservation.ProviderInvocationID] {
			return false
		}
	}
	for _, attempt := range routes.Attempts {
		if !attempt.Counted {
			return false
		}
		found := false
		for _, invocation := range ledger.Invocations {
			reservation := invocation.Reservation
			if workerCostPurpose(reservation.Purpose) && reservation.TaskID == attempt.TaskID &&
				reservation.OccurrenceAttempt == attempt.OccurrenceAttempt &&
				reservation.ExecutionTarget == attempt.ExecutionTarget {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return admitted
}

func economicsAdmittedBackend(policy *team.ExecutionPolicySnapshot, reservation cost.ReservationEvent) bool {
	backend, _, ok := strings.Cut(reservation.ExecutionTarget, "/")
	if !ok {
		return false
	}
	kind := "llm"
	if reservation.Purpose == cost.PurposeExternal {
		kind = "agent"
	}
	for _, bound := range policy.Backends {
		if bound.Backend == backend && bound.Kind == kind {
			return true
		}
	}
	return false
}
