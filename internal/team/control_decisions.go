package team

import (
	"fmt"
	"math"
	"regexp"
	"slices"

	"github.com/kjelly/hufu/internal/decisionrt/control"
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
