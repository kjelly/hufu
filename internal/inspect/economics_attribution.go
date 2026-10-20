package inspect

import (
	"context"
	"fmt"
	"math"

	"github.com/kjelly/hufu/internal/cost"
)

func economicsAttribution(data *EconomicsData, routes RoutesData, ledger cost.Projection) error {
	return economicsAttributionContext(context.Background(), data, routes, ledger)
}

func economicsAttributionContext(ctx context.Context, data *EconomicsData, routes RoutesData, ledger cost.Projection) error {
	for _, group := range routes.Groups {
		data.Routes = append(data.Routes, RouteEconomics{RouteGroup: group, ExcludesOverhead: true, Reason: "no_samples"})
	}
	totals := map[string]int64{"initial": 0, "retry": 0, "fallback": 0}
	workerInvocations := 0
	occurrences := map[struct {
		task    string
		attempt int
	}]int{}
	for _, invocation := range sortedInvocations(ledger) {
		if err := ctx.Err(); err != nil {
			return err
		}
		reservation, amount := invocation.Reservation, usageAmount(invocation)
		if !workerCostPurpose(reservation.Purpose) ||
			reservation.Purpose == cost.PurposeRepair && reservation.Role == "coordinator" && reservation.TaskID == "" {
			if err := addEconomicsMicros(&data.OverheadUsageCostMicros, amount); err != nil {
				return err
			}
			continue
		}
		workerInvocations++
		key := struct {
			task    string
			attempt int
		}{reservation.TaskID, reservation.OccurrenceAttempt}
		index, exists := occurrences[key]
		if !exists {
			index = len(data.Occurrences)
			occurrences[key] = index
			data.Occurrences = append(data.Occurrences, OccurrenceCost{TaskID: key.task, OccurrenceAttempt: key.attempt})
		}
		if err := addEconomicsMicros(&data.Occurrences[index].UsageCostMicros, amount); err != nil {
			return err
		}
		matches := invocationAttempts(invocation, routes.Attempts)
		if len(matches) != 1 || !matches[0].Counted || matches[0].AttemptClass == "unavailable" ||
			matches[0].ExecutionTarget == "" || matches[0].ExecutionTarget != reservation.ExecutionTarget {
			data.UnassignedInvocations++
			data.BreakdownReason = "attribution_unavailable"
			if err := addEconomicsMicros(&data.UnassignedUsageCostMicros, amount); err != nil {
				return err
			}
			// Unknown identity can affect every route for this occurrence.
			for i := range data.Routes {
				if err := ctx.Err(); err != nil {
					return err
				}
				for _, attempt := range matches {
					if sameEconomicsGroup(data.Routes[i].RouteGroup, attempt) {
						data.Routes[i].Reason = "attribution_unavailable"
					}
				}
			}
			continue
		}
		attempt := matches[0]
		if amount == nil {
			if data.BreakdownReason == "" {
				data.BreakdownReason = "unknown_cost"
			}
		} else {
			total := new(totals[attempt.AttemptClass])
			if err := addEconomicsMicros(&total, amount); err != nil {
				return err
			}
			totals[attempt.AttemptClass] = *total
		}
		for i := range data.Routes {
			group := &data.Routes[i]
			if !sameEconomicsGroup(group.RouteGroup, attempt) {
				continue
			}
			if amount == nil {
				group.Reason = "unknown_cost"
			} else {
				if group.Reason == "no_samples" {
					group.Reason = ""
				}
				if err := addEconomicsMicros(&group.UsageCostMicros, amount); err != nil {
					return err
				}
			}
		}
	}
	for i := range data.Routes {
		if data.Routes[i].Reason != "" {
			data.Routes[i].UsageCostMicros = nil
		}
	}
	if workerInvocations == 0 {
		data.BreakdownReason = "no_samples"
	}
	if data.BreakdownReason == "" {
		data.InitialCostMicros = new(totals["initial"])
		data.RetryCostMicros = new(totals["retry"])
		data.FallbackCostMicros = new(totals["fallback"])
	}
	return nil
}

func workerCostPurpose(purpose cost.Purpose) bool {
	switch purpose {
	case cost.PurposeWorker, cost.PurposeDirectAgent, cost.PurposeExternal, cost.PurposeRepair:
		return true
	default:
		return false
	}
}

// No dispatch/model execution ID exists in a cost event. An occurrence with
// multiple executions remains ambiguous even when target names happen to differ:
// the ledger cannot prove which retry produced a particular invocation.
func invocationAttempts(invocation cost.InvocationProjection, attempts []RouteAttempt) []RouteAttempt {
	var matches []RouteAttempt
	for _, attempt := range attempts {
		if attempt.InvocationRunID == invocation.Reservation.RunID &&
			attempt.TaskID == invocation.Reservation.TaskID &&
			attempt.OccurrenceAttempt == invocation.Reservation.OccurrenceAttempt {
			matches = append(matches, attempt)
		}
	}
	return matches
}

func sameEconomicsGroup(group RouteGroup, attempt RouteAttempt) bool {
	return group.RouteKind == attempt.RouteKind && group.RouteName == attempt.RouteName &&
		group.RouteDigest == attempt.RouteDigest && group.ExecutionTarget == attempt.ExecutionTarget
}

func addEconomicsMicros(destination **int64, amount *int64) error {
	if amount == nil {
		return nil
	}
	current := int64(0)
	if *destination != nil {
		current = **destination
	}
	if *amount < 0 || current < 0 || *amount > math.MaxInt64-current {
		return fmt.Errorf("%w: invalid or overflowing economics amount", ErrIntegrity)
	}
	*destination = new(current + *amount)
	return nil
}
