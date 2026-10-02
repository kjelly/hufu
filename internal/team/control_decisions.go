package team

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/tools"
)

// EventControlDecisionObserved records one shadow or active runtime control
// decision (docs/architecture/decision-primitive.md §59). Its payload is
// content-free: no question, command, path, arguments, options, or goal.
const EventControlDecisionObserved EventType = "control_decision_observed"

// Where a control decision's applied outcome came from.
const (
	controlAppliedPrimitive   = "primitive"
	controlAppliedSafeDefault = "safe_default"
	controlAppliedLegacy      = "legacy"
)

// Encodings of the existing path's outcome besides a value.
const (
	controlLegacyError   = "error"
	controlLegacyAbstain = "abstain"
	controlLegacyInvalid = "invalid"
)

var (
	controlValuePattern  = regexp.MustCompile(`^(true|false|[0-9]{1,2})$`)
	controlLegacyPattern = regexp.MustCompile(`^(true|false|[0-9]{1,2}|error|abstain|invalid)$`)
	controlCodePattern   = regexp.MustCompile(`^[a-z][a-z_]{0,63}$`)
)

type controlDecisionPayload struct {
	Version    int     `json:"version"`
	Point      string  `json:"point"`
	Mode       string  `json:"mode"`
	Applied    string  `json:"applied"`
	Status     string  `json:"status"`
	Value      string  `json:"value,omitempty"`
	Confidence float64 `json:"confidence,omitzero"`
	Accepted   bool    `json:"accepted,omitzero"`
	Threshold  float64 `json:"threshold"`
	ReasonCode string  `json:"reason_code,omitempty"`
	ErrorCode  string  `json:"error_code,omitempty"`
	Model      string  `json:"model,omitempty"`
	DurationMS int64   `json:"duration_ms"`
	Candidates int     `json:"candidates,omitzero"`
	// Legacy is the existing path's outcome in the same encoding as Value,
	// or empty when the existing path did not run.
	Legacy   string `json:"legacy,omitempty"`
	LegacyMS int64  `json:"legacy_ms,omitzero"`
	// Agree is set only when both sides produced a comparable value.
	Agree *bool `json:"agree,omitempty"`
	// Backend is the decision backend; an observation without it predates
	// backend selection and used systemone.
	Backend string `json:"backend,omitempty"`
}

func validateControlDecisionEvent(event RunEvent) error {
	var payload controlDecisionPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return fmt.Errorf("invalid control decision payload: %w", err)
	}
	if payload.Version != 1 || !slices.Contains(control.Points(), control.Point(payload.Point)) {
		return fmt.Errorf("invalid control decision point")
	}
	if payload.Mode != string(control.ModeShadow) && payload.Mode != string(control.ModeActive) {
		return fmt.Errorf("invalid control decision mode")
	}
	if !slices.Contains([]string{controlAppliedPrimitive, controlAppliedSafeDefault, controlAppliedLegacy}, payload.Applied) ||
		payload.Mode == string(control.ModeShadow) && payload.Applied != controlAppliedLegacy {
		return fmt.Errorf("invalid control decision application")
	}
	decided := payload.Status == string(control.StatusDecided)
	switch {
	case !slices.Contains([]string{string(control.StatusDecided), string(control.StatusAbstained), string(control.StatusError)}, payload.Status),
		decided != (payload.Value != "") || payload.Value != "" && !controlValuePattern.MatchString(payload.Value),
		payload.Accepted && !decided,
		!controlFraction(payload.Confidence) || !controlFraction(payload.Threshold),
		payload.ReasonCode != "" && !controlCodePattern.MatchString(payload.ReasonCode),
		payload.ErrorCode != "" && !controlCodePattern.MatchString(payload.ErrorCode),
		payload.Status == string(control.StatusError) && payload.ErrorCode == "",
		len(payload.Model) > 256, payload.DurationMS < 0, payload.LegacyMS < 0,
		payload.Candidates < 0 || payload.Candidates > 21,
		payload.Legacy != "" && !controlLegacyPattern.MatchString(payload.Legacy),
		payload.Agree != nil && (!decided || !controlValuePattern.MatchString(payload.Legacy)),
		payload.Applied == controlAppliedLegacy && payload.Legacy == "":
		return fmt.Errorf("invalid control decision outcome")
	}
	if event.Actor == "" {
		return fmt.Errorf("invalid control decision identity")
	}
	return nil
}

func controlFraction(value float64) bool {
	return !math.IsNaN(value) && value >= 0 && value <= 1
}

// controlDecisionRecord is one invocation in progress.
type controlDecisionRecord struct {
	point      control.Point
	mode       control.Mode
	candidates int
	outcome    control.Outcome
	applied    string
	legacy     string
	legacyTime time.Duration
}

// recordControlDecision appends the invocation's content-free event. It is
// best effort: an observation must never change the decision it describes,
// so a journal failure only warns.
func (c *Coordinator) recordControlDecision(ctx context.Context, record controlDecisionRecord) {
	if c == nil || !c.hasDurableEventJournal() {
		return
	}
	payload := controlDecisionPayload{
		Version: 1, Point: string(record.point), Mode: string(record.mode), Applied: record.applied,
		Status: string(record.outcome.Status), Value: record.outcome.Value, Confidence: record.outcome.Confidence,
		Accepted: record.outcome.Accepted, Threshold: c.controlDecisions.MinConfidence(record.point),
		ReasonCode: record.outcome.ReasonCode, ErrorCode: record.outcome.ErrorCode, Model: record.outcome.Model,
		Backend:    c.controlDecisions.Backend(),
		DurationMS: record.outcome.Duration.Milliseconds(), Candidates: record.candidates,
		Legacy: record.legacy, LegacyMS: record.legacyTime.Milliseconds(),
	}
	if record.outcome.Status == control.StatusDecided && controlValuePattern.MatchString(record.legacy) {
		payload.Agree = new(record.outcome.Value == record.legacy)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: encode control decision observation: %v\n", err)
		return
	}
	event := RunEvent{Type: string(EventControlDecisionObserved), Actor: "hufu", Payload: encoded}
	if metadata, ok := invocationMetadataFromContext(ctx); ok {
		event.TaskID, event.Attempt = metadata.TaskID, metadata.Attempt
		if metadata.AgentName != "" {
			event.Actor = metadata.AgentName
		}
	} else if agentName, _ := ctx.Value(tools.AgentNameKey).(string); agentName != "" {
		event.Actor = agentName
	}
	if event.TaskID == "" {
		event.TaskID, _ = ctx.Value(todoIDKey{}).(string)
	}
	if err := validateControlDecisionEvent(event); err != nil {
		fmt.Fprintf(os.Stderr, "warning: control decision observation rejected: %v\n", err)
		return
	}
	if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), event); err != nil {
		fmt.Fprintf(os.Stderr, "warning: persist control decision observation: %v\n", err)
	}
}

// controlLeg is the existing path's result and its outcome encoding.
type controlLeg[T any] struct {
	value T
	err   error
	code  string
}

// controlPlan describes one control decision invocation. legacy runs the
// existing path; primitive maps an accepted decision; safe is the point's
// outcome when the decision model answers without enough confidence.
type controlPlan[T any] struct {
	point      control.Point
	request    decisionrt.Request
	candidates int
	legacy     func() controlLeg[T]
	primitive  func(control.Outcome) (T, bool)
	safe       func() (T, error)
}

// runControlPlan applies the point's mode (§59): off runs the existing path
// only; shadow runs both concurrently and returns the existing result; active
// applies an accepted decision, takes the safe outcome on low confidence, and
// runs the existing path when the decision model fails.
func runControlPlan[T any](c *Coordinator, ctx context.Context, plan controlPlan[T]) (T, error) {
	mode := c.controlDecisions.Mode(plan.point)
	record := controlDecisionRecord{point: plan.point, mode: mode, candidates: plan.candidates}
	runLegacy := func() controlLeg[T] {
		started := time.Now()
		leg := plan.legacy()
		record.applied, record.legacy, record.legacyTime = controlAppliedLegacy, leg.code, time.Since(started)
		return leg
	}
	switch mode {
	case control.ModeShadow:
		done := make(chan control.Outcome, 1)
		go func() { done <- c.controlDecisions.Decide(ctx, plan.point, plan.request) }()
		leg := runLegacy()
		record.outcome = <-done
		c.recordControlDecision(ctx, record)
		return leg.value, leg.err
	case control.ModeActive:
		record.outcome = c.controlDecisions.Decide(ctx, plan.point, plan.request)
		if record.outcome.Accepted {
			if value, ok := plan.primitive(record.outcome); ok {
				record.applied = controlAppliedPrimitive
				c.recordControlDecision(ctx, record)
				return value, nil
			}
		} else if record.outcome.Status != control.StatusError {
			record.applied = controlAppliedSafeDefault
			c.recordControlDecision(ctx, record)
			return plan.safe()
		}
		leg := runLegacy()
		c.recordControlDecision(ctx, record)
		return leg.value, leg.err
	default:
		leg := plan.legacy()
		return leg.value, leg.err
	}
}
