package team

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/decisionrt/control"
)

// ControlDecisionSummary aggregates control_decision_observed events for one
// point and mode. It is the evidence for moving a point from shadow to
// active: how often the decision model agreed with the existing path, how
// often it would have abstained, and how long it took.
type ControlDecisionSummary struct {
	Point string `json:"point"`
	Mode  string `json:"mode"`
	// Backend keeps systemone and sidecar observations apart, since each
	// point is evaluated for activation per backend.
	Backend   string `json:"backend"`
	Calls     int    `json:"calls"`
	Decided   int    `json:"decided"`
	Abstained int    `json:"abstained"`
	Errors    int    `json:"errors"`
	// Compared counts decided calls whose existing outcome was a comparable
	// value; Agreed counts the ones that matched.
	Compared int `json:"compared"`
	Agreed   int `json:"agreed"`
	// BelowThreshold counts decided answers under the point's threshold:
	// calls that active mode answers with the safe outcome.
	BelowThreshold     int            `json:"below_threshold"`
	AppliedPrimitive   int            `json:"applied_primitive"`
	AppliedSafeDefault int            `json:"applied_safe_default"`
	AppliedLegacy      int            `json:"applied_legacy"`
	MeanConfidence     float64        `json:"mean_confidence"`
	P50MS              int64          `json:"p50_ms"`
	P95MS              int64          `json:"p95_ms"`
	ErrorCodes         map[string]int `json:"error_codes,omitempty"`
}

// SummarizeControlDecisions aggregates every valid observation in events,
// ordered by point and mode. Invalid observations are skipped.
func SummarizeControlDecisions(events []RunEvent) []ControlDecisionSummary {
	type key struct{ point, mode, backend string }
	type accumulator struct {
		summary    ControlDecisionSummary
		confidence float64
		durations  []int64
	}
	groups := make(map[key]*accumulator)
	for _, event := range events {
		if event.Type != string(EventControlDecisionObserved) || validateControlDecisionEvent(event) != nil {
			continue
		}
		var payload controlDecisionPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		backendName := payload.Backend
		if backendName == "" {
			backendName = control.BackendSystemOne
		}
		groupKey := key{payload.Point, payload.Mode, backendName}
		group := groups[groupKey]
		if group == nil {
			group = &accumulator{summary: ControlDecisionSummary{Point: payload.Point, Mode: payload.Mode, Backend: backendName}}
			groups[groupKey] = group
		}
		summary := &group.summary
		summary.Calls++
		group.durations = append(group.durations, payload.DurationMS)
		switch control.Status(payload.Status) {
		case control.StatusDecided:
			summary.Decided++
			group.confidence += payload.Confidence
			if !payload.Accepted {
				summary.BelowThreshold++
			}
		case control.StatusAbstained:
			summary.Abstained++
		default:
			summary.Errors++
			if summary.ErrorCodes == nil {
				summary.ErrorCodes = make(map[string]int)
			}
			summary.ErrorCodes[payload.ErrorCode]++
		}
		if payload.Agree != nil {
			summary.Compared++
			if *payload.Agree {
				summary.Agreed++
			}
		}
		switch payload.Applied {
		case controlAppliedPrimitive:
			summary.AppliedPrimitive++
		case controlAppliedSafeDefault:
			summary.AppliedSafeDefault++
		default:
			summary.AppliedLegacy++
		}
	}
	keys := slices.SortedFunc(maps.Keys(groups), func(a, b key) int {
		return cmp.Or(cmp.Compare(a.point, b.point), cmp.Compare(a.mode, b.mode), cmp.Compare(a.backend, b.backend))
	})
	summaries := make([]ControlDecisionSummary, 0, len(keys))
	for _, groupKey := range keys {
		group := groups[groupKey]
		if group.summary.Decided > 0 {
			group.summary.MeanConfidence = group.confidence / float64(group.summary.Decided)
		}
		slices.Sort(group.durations)
		group.summary.P50MS = percentile(group.durations, 50)
		group.summary.P95MS = percentile(group.durations, 95)
		summaries = append(summaries, group.summary)
	}
	return summaries
}

// percentile returns the nearest-rank percentile of sorted values.
func percentile(sorted []int64, rank int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := max((rank*len(sorted)+99)/100, 1)
	return sorted[index-1]
}

// ControlDecisionSummaries aggregates the active lineage's observations.
func (c *Coordinator) ControlDecisionSummaries(ctx context.Context) ([]ControlDecisionSummary, error) {
	if c == nil || !c.hasDurableEventJournal() {
		return nil, nil
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	return SummarizeControlDecisions(events), nil
}
