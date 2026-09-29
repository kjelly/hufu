package cost

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

const ViewSchemaVersion = 1

// View is the single presentation-safe generation-cost projection consumed by
// inspect, operator snapshots, reports, summaries, and the TUI. Monetary
// values are integer USD micros; unknown and non-metered calls stay separate.
type View struct {
	SchemaVersion           int       `json:"schema_version"`
	Coverage                string    `json:"coverage"`
	Available               bool      `json:"available"`
	RunID                   string    `json:"run_id"`
	TaskID                  string    `json:"task_id,omitempty"`
	KnownMicros             *int64    `json:"known_micros,omitempty"`
	UsageDerivedMicros      *int64    `json:"usage_derived_micros,omitempty"`
	AdmissionBoundMicros    *int64    `json:"admission_bound_micros,omitempty"`
	OpenReservationMicros   *int64    `json:"open_reservation_micros,omitempty"`
	OpenReservationCount    int       `json:"open_reservation_count,omitzero"`
	UnknownInvocations      int       `json:"unknown_invocations,omitzero"`
	LocalInvocations        int       `json:"local_invocations,omitzero"`
	SubscriptionInvocations int       `json:"subscription_invocations,omitzero"`
	BudgetMicros            *int64    `json:"budget_micros,omitempty"`
	RemainingMicros         *int64    `json:"remaining_micros,omitempty"`
	Integrity               string    `json:"integrity"`
	Freshness               Freshness `json:"freshness"`
}

type Freshness struct {
	EventID   string `json:"event_id,omitempty"`
	EventHash string `json:"event_hash,omitempty"`
}

// BuildView projects one run, optionally narrowed to one task. Remaining is a
// run-wide budget fact even for a task-filtered view.
func BuildView(projection Projection, policy *PolicySnapshot, runID, taskID string) (View, error) {
	runID = strings.TrimSpace(runID)
	taskID = strings.TrimSpace(taskID)
	if runID == "" {
		return View{}, fmt.Errorf("cost view run ID is required")
	}
	view := View{
		SchemaVersion: ViewSchemaVersion, Coverage: "generation_only", Available: projection.Available,
		RunID: runID, TaskID: taskID, Integrity: "ok",
	}
	if policy != nil {
		if err := ValidatePolicySnapshot(policy); err != nil {
			return View{}, fmt.Errorf("cost view policy: %w", err)
		}
		view.BudgetMicros = cloneInt64(policy.MaxRunMicros)
	}
	if !projection.Available {
		if err := ValidateView(view); err != nil {
			return View{}, err
		}
		return view, nil
	}

	var runKnown int64
	for _, key := range slices.Sorted(maps.Keys(projection.Invocations)) {
		invocation := projection.Invocations[key]
		if invocation.Reservation.RunID != runID {
			continue
		}
		estimate, open := invocationViewEstimate(invocation)
		if estimate.Micros != nil {
			var err error
			runKnown, err = addMicros(runKnown, *estimate.Micros)
			if err != nil {
				return View{}, fmt.Errorf("cost view run total: %w", err)
			}
		}
		if taskID != "" && invocation.Reservation.TaskID != taskID {
			continue
		}
		if err := addInvocationToView(&view, estimate, open); err != nil {
			return View{}, err
		}
	}
	if view.BudgetMicros != nil {
		remaining := *view.BudgetMicros - runKnown
		if remaining < 0 {
			remaining = 0
		}
		view.RemainingMicros = new(remaining)
	}
	if err := ValidateView(view); err != nil {
		return View{}, err
	}
	return view, nil
}

func (v View) Clone() View {
	v.KnownMicros = cloneInt64(v.KnownMicros)
	v.UsageDerivedMicros = cloneInt64(v.UsageDerivedMicros)
	v.AdmissionBoundMicros = cloneInt64(v.AdmissionBoundMicros)
	v.OpenReservationMicros = cloneInt64(v.OpenReservationMicros)
	v.BudgetMicros = cloneInt64(v.BudgetMicros)
	v.RemainingMicros = cloneInt64(v.RemainingMicros)
	return v
}

func ValidateView(view View) error {
	if view.SchemaVersion != ViewSchemaVersion {
		return fmt.Errorf("cost view schema version %d is unsupported", view.SchemaVersion)
	}
	if view.Coverage != "generation_only" {
		return fmt.Errorf("cost view coverage %q is unsupported", view.Coverage)
	}
	if strings.TrimSpace(view.RunID) == "" {
		return fmt.Errorf("cost view run ID is required")
	}
	if view.Integrity != "ok" && view.Integrity != "degraded" && view.Integrity != "invalid" {
		return fmt.Errorf("cost view integrity %q is unsupported", view.Integrity)
	}
	for name, value := range map[string]*int64{
		"known": view.KnownMicros, "usage-derived": view.UsageDerivedMicros,
		"admission-bound": view.AdmissionBoundMicros, "open-reservation": view.OpenReservationMicros,
		"budget": view.BudgetMicros, "remaining": view.RemainingMicros,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("cost view %s micros must not be negative", name)
		}
	}
	if view.OpenReservationCount < 0 || view.UnknownInvocations < 0 || view.LocalInvocations < 0 || view.SubscriptionInvocations < 0 {
		return fmt.Errorf("cost view counters must not be negative")
	}
	if view.RemainingMicros != nil && (view.BudgetMicros == nil || view.Integrity != "ok") {
		return fmt.Errorf("cost view remaining requires a hard budget and good integrity")
	}
	return nil
}

func addInvocationToView(view *View, estimate Estimate, open bool) error {
	if estimate.Micros != nil {
		if err := addAggregateMicros(&view.KnownMicros, *estimate.Micros); err != nil {
			return fmt.Errorf("cost view known total: %w", err)
		}
	}
	if open {
		view.OpenReservationCount++
		if estimate.Micros != nil {
			if err := addAggregateMicros(&view.OpenReservationMicros, *estimate.Micros); err != nil {
				return fmt.Errorf("cost view open reservations: %w", err)
			}
		}
	} else {
		switch estimate.Source {
		case EstimateUsage:
			if estimate.Micros == nil {
				return fmt.Errorf("cost view usage estimate has no amount")
			}
			if err := addAggregateMicros(&view.UsageDerivedMicros, *estimate.Micros); err != nil {
				return fmt.Errorf("cost view usage total: %w", err)
			}
		case EstimateAdmissionBound:
			if estimate.Micros == nil {
				return fmt.Errorf("cost view admission estimate has no amount")
			}
			if err := addAggregateMicros(&view.AdmissionBoundMicros, *estimate.Micros); err != nil {
				return fmt.Errorf("cost view admission total: %w", err)
			}
		}
	}
	switch estimate.Source {
	case EstimateNotMetered:
		view.LocalInvocations++
	case EstimateSubscription:
		view.SubscriptionInvocations++
	case EstimateUnknown:
		view.UnknownInvocations++
	case EstimateUsage, EstimateAdmissionBound:
	default:
		return fmt.Errorf("cost view estimate source %q is unsupported", estimate.Source)
	}
	return nil
}

func invocationViewEstimate(invocation InvocationProjection) (Estimate, bool) {
	if invocation.Settlement == nil {
		return reservationEstimate(invocation.Reservation), true
	}
	return settlementEstimate(*invocation.Settlement), false
}
