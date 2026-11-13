package inspect

import (
	"fmt"
	"maps"
	"math"
	"slices"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// EconomicsComparedRun retains the source and completeness of one frozen run.
type EconomicsComparedRun struct {
	Selection   EconomicsRunSelection     `json:"selection"`
	Scope       operatorpkg.ResolvedScope `json:"scope"`
	Economics   EconomicsData             `json:"economics"`
	Terminal    bool                      `json:"terminal"`
	Snapshots   EconomicsSnapshotRefs     `json:"snapshots"`
	CostReasons []string                  `json:"cost_completeness_reasons"`
}

type EconomicsComparisonGroup struct {
	Group                     string         `json:"group"`
	RunCount                  int            `json:"run_count"`
	TerminalRunCount          int            `json:"terminal_run_count"`
	NonterminalRunCount       int            `json:"nonterminal_run_count"`
	AcceptedRunCount          int            `json:"accepted_run_count"`
	AcceptedRate              *float64       `json:"accepted_rate"`
	OutcomeCounts             map[string]int `json:"outcome_counts"`
	AcceptanceCounts          map[string]int `json:"acceptance_counts"`
	TaskCount                 int            `json:"task_count"`
	VerifiedTaskCount         int            `json:"verified_task_count"`
	VerifiedTaskRate          *float64       `json:"verified_task_rate"`
	KnownSubtotalMicros       *int64         `json:"known_subtotal_micros"`
	TotalCostMicros           *int64         `json:"total_cost_micros"`
	CostPerAcceptedRunMicros  *int64         `json:"cost_per_accepted_run_micros"`
	IncompleteCostRunCount    int            `json:"incomplete_cost_run_count"`
	CostUnavailableReasons    map[string]int `json:"cost_unavailable_reasons"`
	CostPerAcceptedRunReasons []string       `json:"cost_per_accepted_run_reasons"`
}

func aggregateEconomicsComparisons(runs []EconomicsComparedRun) ([]EconomicsComparisonGroup, error) {
	groups := map[string]*EconomicsComparisonGroup{}
	for _, run := range runs {
		label := run.Selection.Group
		group := groups[label]
		if group == nil {
			group = &EconomicsComparisonGroup{Group: label, OutcomeCounts: map[string]int{}, AcceptanceCounts: map[string]int{}, CostUnavailableReasons: map[string]int{}, CostPerAcceptedRunReasons: []string{}}
			groups[label] = group
		}
		data := run.Economics
		if data.TaskCount < 0 || data.VerifiedTaskCount < 0 || data.VerifiedTaskCount > data.TaskCount {
			return nil, fmt.Errorf("%w: invalid comparison task counts", ErrIntegrity)
		}
		group.RunCount++
		if run.Terminal {
			group.TerminalRunCount++
		} else {
			group.NonterminalRunCount++
		}
		outcome, acceptance := data.RunOutcome, data.Acceptance
		if outcome == "" {
			outcome = "unknown"
		}
		if acceptance == "" {
			acceptance = "unknown"
		}
		group.OutcomeCounts[outcome]++
		group.AcceptanceCounts[acceptance]++
		if run.Terminal && outcome == string(team.RunOutcomeCompleted) && acceptance == string(team.AcceptancePassed) {
			group.AcceptedRunCount++
		}
		if group.TaskCount > math.MaxInt-data.TaskCount || group.VerifiedTaskCount > math.MaxInt-data.VerifiedTaskCount {
			return nil, fmt.Errorf("%w: comparison task count overflow", ErrIntegrity)
		}
		group.TaskCount += data.TaskCount
		group.VerifiedTaskCount += data.VerifiedTaskCount
		if err := addEconomicsMicros(&group.KnownSubtotalMicros, data.UsageCostMicros); err != nil {
			return nil, err
		}

		reasons := slices.Clone(run.CostReasons)
		if data.UsageCostMicros == nil && len(reasons) == 0 {
			reasons = append(reasons, "unknown_cost")
		}
		if len(reasons) > 0 {
			group.IncompleteCostRunCount++
			slices.Sort(reasons)
			for _, reason := range slices.Compact(reasons) {
				group.CostUnavailableReasons[reason]++
			}
		}
	}
	result := make([]EconomicsComparisonGroup, 0, len(groups))
	for _, label := range slices.Sorted(maps.Keys(groups)) {
		group := groups[label]
		group.AcceptedRate = new(float64(group.AcceptedRunCount) / float64(group.RunCount))
		if group.TaskCount > 0 {
			group.VerifiedTaskRate = new(float64(group.VerifiedTaskCount) / float64(group.TaskCount))
		}
		if group.IncompleteCostRunCount == 0 && group.KnownSubtotalMicros != nil {
			total := *group.KnownSubtotalMicros
			group.TotalCostMicros = new(total)
			if group.AcceptedRunCount > 0 {
				denominator := int64(group.AcceptedRunCount)
				rounded := total / denominator
				if total%denominator != 0 {
					rounded++
				}
				group.CostPerAcceptedRunMicros = new(rounded)
			}
		}
		group.CostPerAcceptedRunReasons = slices.Sorted(maps.Keys(group.CostUnavailableReasons))
		if group.AcceptedRunCount == 0 {
			group.CostPerAcceptedRunReasons = append(group.CostPerAcceptedRunReasons, "no_accepted_runs")
		}
		result = append(result, *group)
	}
	return result, nil
}
