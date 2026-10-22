package operator

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
)

// ObservationSummary is presentation data from a single validated event head.
// The inspector owns qualification and arithmetic; renderers only display it.
type ObservationSummary struct {
	Status            string          `json:"status"`
	SourceHead        cost.Freshness  `json:"source_head"`
	ComparisonScope   string          `json:"comparison_scope"`
	TaskCount         *int            `json:"task_count"`
	VerifiedTasks     *int            `json:"verified_tasks"`
	UnavailableTasks  *int            `json:"unavailable_tasks"`
	UsageCostMicros   *int64          `json:"usage_cost_micros"`
	KnownCostCoverage *float64        `json:"known_cost_coverage"`
	ReasonCodes       []string        `json:"reason_codes"`
	Routes            []ObservedRoute `json:"routes"`
}

type ObservedRoute struct {
	Kind          string `json:"kind"`
	Name          string `json:"name,omitempty"`
	Digest        string `json:"digest,omitempty"`
	Target        string `json:"target"`
	Attempts      int    `json:"attempts"`
	VerifiedTasks int    `json:"verified_tasks"`
}

func (summary *ObservationSummary) Clone() *ObservationSummary {
	if summary == nil {
		return nil
	}
	result := *summary
	result.TaskCount = clonePointer(summary.TaskCount)
	result.VerifiedTasks = clonePointer(summary.VerifiedTasks)
	result.UnavailableTasks = clonePointer(summary.UnavailableTasks)
	result.UsageCostMicros = clonePointer(summary.UsageCostMicros)
	result.KnownCostCoverage = clonePointer(summary.KnownCostCoverage)
	result.ReasonCodes = cloneSorted(summary.ReasonCodes)
	result.Routes = slices.Clone(summary.Routes)
	return &result
}

func validateObservations(summary *ObservationSummary, head FreshnessView) error {
	if summary == nil {
		return nil
	}
	if summary.Status != "available" && summary.Status != "unavailable" {
		return fmt.Errorf("operator observations status is invalid")
	}
	if summary.ComparisonScope != "single_run_observed" {
		return fmt.Errorf("operator observations comparison scope is invalid")
	}
	if summary.SourceHead.EventID == "" || summary.SourceHead.EventHash == "" ||
		summary.SourceHead.EventID != head.EventID || summary.SourceHead.EventHash != head.EventHash {
		return fmt.Errorf("operator observations event head does not match snapshot")
	}
	for _, count := range []*int{summary.TaskCount, summary.VerifiedTasks, summary.UnavailableTasks} {
		if count != nil && *count < 0 || summary.Status == "available" && count == nil {
			return fmt.Errorf("operator observations counts are invalid")
		}
	}
	if summary.Status == "unavailable" && (summary.TaskCount != nil || summary.VerifiedTasks != nil ||
		summary.UnavailableTasks != nil || summary.UsageCostMicros != nil || summary.KnownCostCoverage != nil || len(summary.Routes) > 0) {
		return fmt.Errorf("unavailable operator observations carry numbers")
	}
	if summary.UsageCostMicros != nil && *summary.UsageCostMicros < 0 {
		return fmt.Errorf("operator observations cost is negative")
	}
	if summary.Status == "available" && (*summary.VerifiedTasks > *summary.TaskCount ||
		*summary.UnavailableTasks > *summary.TaskCount-*summary.VerifiedTasks) {
		return fmt.Errorf("operator observations evidence counts exceed task count")
	}
	if coverage := summary.KnownCostCoverage; coverage != nil && (math.IsNaN(*coverage) || math.IsInf(*coverage, 0) || *coverage < 0 || *coverage > 1) {
		return fmt.Errorf("operator observations coverage is invalid")
	}
	for _, route := range summary.Routes {
		if route.Attempts < 0 || route.VerifiedTasks < 0 {
			return fmt.Errorf("operator observations route counts are negative")
		}
	}
	return nil
}

// RenderObservationSummary is shared by plain CLI and the existing TUI panel.
// It neither loads data nor recalculates any projection.
func RenderObservationSummary(summary *ObservationSummary) string {
	if summary == nil {
		return "Observations: unavailable\n"
	}
	var output strings.Builder
	fmt.Fprintf(&output, "Observations: %s\nEvent head: %s\n",
		SafeDisplayText(summary.Status, 40), SafeDisplayText(summary.SourceHead.EventID, 120))
	if summary.Status == "available" {
		number := func(value *int) string {
			if value == nil {
				return "unknown"
			}
			return fmt.Sprint(*value)
		}
		fmt.Fprintf(&output, "Tasks: %s\nVerified: %s\nEvidence unavailable: %s\n",
			number(summary.TaskCount), number(summary.VerifiedTasks), number(summary.UnavailableTasks))
		usage, coverage := "unknown", "unknown"
		if summary.UsageCostMicros != nil {
			usage = fmt.Sprint(*summary.UsageCostMicros)
		}
		if summary.KnownCostCoverage != nil {
			coverage = fmt.Sprintf("%.4g", *summary.KnownCostCoverage)
		}
		fmt.Fprintf(&output, "Usage subtotal (USD micros): %s\nRecorded invocation coverage: %s\n", usage, coverage)
		for _, route := range summary.Routes {
			fmt.Fprintf(&output, "Route: %s (%s)\nTarget: %s\nAttempts: %d; verified: %d\n",
				SafeDisplayText(valueOrUnknown(route.Name), 120), SafeDisplayText(route.Kind, 40),
				SafeDisplayText(valueOrUnknown(route.Target), 160), route.Attempts, route.VerifiedTasks)
		}
	}
	if len(summary.ReasonCodes) > 0 {
		fmt.Fprintf(&output, "Data gaps: %s\n", SafeDisplayText(strings.Join(summary.ReasonCodes, ", "), 500))
	}
	output.WriteString("Details: inspect routes / inspect economics\n")
	return output.String()
}
