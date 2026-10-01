// Package control defines hufu's fixed runtime control decisions and the
// configuration that selects how each one uses the systemone decision
// backend (docs/architecture/decision-primitive.md §59).
//
// The questions, option mappings, context keys, and safe outcomes are owned
// by this package; configuration only chooses a mode, a confidence threshold,
// and the transport. Modes, durability, and reporting belong to the caller.
package control

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
)

// Mode selects how a point uses the decision model.
type Mode string

const (
	// ModeOff never calls the decision model. It is the default.
	ModeOff Mode = "off"
	// ModeShadow runs the decision model next to the existing path and only
	// records the comparison; the existing outcome is always applied.
	ModeShadow Mode = "shadow"
	// ModeActive applies an accepted decision, takes the point's safe
	// outcome on a low-confidence answer, and runs the existing path when
	// the decision model fails.
	ModeActive Mode = "active"
)

// Point names one fixed runtime decision.
type Point string

const (
	AgentMatcher  Point = "agent-matcher"
	AskUser       Point = "ask-user"
	PathReviewer  Point = "path-reviewer"
	GuardReviewer Point = "guard-reviewer"
)

// defaultMinConfidence is the built-in threshold per point. The matchers keep
// the sidecar's existing 0.60 cut-off; the reviewers gate authorization, so a
// decision that relaxes a check must be more certain.
var defaultMinConfidence = map[Point]float64{
	AgentMatcher:  0.60,
	AskUser:       0.60,
	PathReviewer:  0.90,
	GuardReviewer: 0.90,
}

// Points returns every point in a stable order.
func Points() []Point {
	return []Point{AgentMatcher, AskUser, PathReviewer, GuardReviewer}
}

// PointConfig overrides the block-level mode or threshold for one point.
type PointConfig struct {
	Mode          Mode     `yaml:"mode,omitempty" json:"mode,omitempty"`
	MinConfidence *float64 `yaml:"min-confidence,omitempty" json:"min_confidence,omitempty"`
}

// Config is the control-decisions block accepted in both hufu.yaml and
// team.yaml. Every field is optional; an empty block leaves every point off.
type Config struct {
	Endpoint      string                `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Model         string                `yaml:"model,omitempty" json:"model,omitempty"`
	APIKeyEnv     string                `yaml:"api-key-env,omitempty" json:"api_key_env,omitempty"`
	Timeout       time.Duration         `yaml:"timeout,omitempty" json:"timeout,omitzero"`
	Mode          Mode                  `yaml:"mode,omitempty" json:"mode,omitempty"`
	MinConfidence *float64              `yaml:"min-confidence,omitempty" json:"min_confidence,omitempty"`
	Points        map[Point]PointConfig `yaml:"points,omitempty" json:"points,omitempty"`
}

// Merge resolves each field to override's value when it is set and to base's
// value otherwise. Per-point fields resolve the same way. Neither input is
// modified.
func Merge(base, override Config) Config {
	merged := base.clone()
	if override.Endpoint != "" {
		merged.Endpoint = override.Endpoint
	}
	if override.Model != "" {
		merged.Model = override.Model
	}
	if override.APIKeyEnv != "" {
		merged.APIKeyEnv = override.APIKeyEnv
	}
	if override.Timeout != 0 {
		merged.Timeout = override.Timeout
	}
	if override.Mode != "" {
		merged.Mode = override.Mode
	}
	if override.MinConfidence != nil {
		merged.MinConfidence = new(*override.MinConfidence)
	}
	for _, point := range slices.Sorted(maps.Keys(override.Points)) {
		if merged.Points == nil {
			merged.Points = make(map[Point]PointConfig)
		}
		current := merged.Points[point]
		next := override.Points[point]
		if next.Mode != "" {
			current.Mode = next.Mode
		}
		if next.MinConfidence != nil {
			current.MinConfidence = new(*next.MinConfidence)
		}
		merged.Points[point] = current
	}
	return merged
}

func (c Config) clone() Config {
	cloned := c
	if c.MinConfidence != nil {
		cloned.MinConfidence = new(*c.MinConfidence)
	}
	if c.Points != nil {
		cloned.Points = make(map[Point]PointConfig, len(c.Points))
		for point, pointConfig := range c.Points {
			if pointConfig.MinConfidence != nil {
				pointConfig.MinConfidence = new(*pointConfig.MinConfidence)
			}
			cloned.Points[point] = pointConfig
		}
	}
	return cloned
}

// Clone returns a deep copy of the configuration.
func (c Config) Clone() Config { return c.clone() }

// Validate checks one block on its own: modes, point names, thresholds, the
// timeout range, and the api-key-env name. It does not require a model or
// read the credential, because another file may supply them; New validates
// the merged block completely.
func (c Config) Validate() error {
	if _, err := c.resolvePoints(); err != nil {
		return err
	}
	if c.Timeout != 0 {
		if _, err := validateTimeout(c.Timeout); err != nil {
			return err
		}
	}
	if c.APIKeyEnv != "" && !envName.MatchString(c.APIKeyEnv) {
		return fmt.Errorf("control-decisions.api-key-env: invalid variable name")
	}
	return nil
}

// resolvedPoint is the effective policy of one point.
type resolvedPoint struct {
	Mode          Mode    `json:"mode"`
	MinConfidence float64 `json:"min_confidence"`
}

// resolvePoints validates modes, point names, and thresholds and returns the
// effective policy of every point. It does not look at the transport.
func (c Config) resolvePoints() (map[Point]resolvedPoint, error) {
	for point := range c.Points {
		if _, known := defaultMinConfidence[point]; !known {
			return nil, fmt.Errorf("control-decisions.points: unknown point %q", point)
		}
	}
	if err := validateMode(c.Mode); err != nil {
		return nil, fmt.Errorf("control-decisions.mode: %w", err)
	}
	if err := validateThreshold(c.MinConfidence); err != nil {
		return nil, fmt.Errorf("control-decisions.min-confidence: %w", err)
	}
	resolved := make(map[Point]resolvedPoint, len(defaultMinConfidence))
	for _, point := range Points() {
		pointConfig := c.Points[point]
		if err := validateMode(pointConfig.Mode); err != nil {
			return nil, fmt.Errorf("control-decisions.points.%s.mode: %w", point, err)
		}
		if err := validateThreshold(pointConfig.MinConfidence); err != nil {
			return nil, fmt.Errorf("control-decisions.points.%s.min-confidence: %w", point, err)
		}
		effective := resolvedPoint{Mode: ModeOff, MinConfidence: defaultMinConfidence[point]}
		if c.Mode != "" {
			effective.Mode = c.Mode
		}
		if pointConfig.Mode != "" {
			effective.Mode = pointConfig.Mode
		}
		if c.MinConfidence != nil {
			effective.MinConfidence = *c.MinConfidence
		}
		if pointConfig.MinConfidence != nil {
			effective.MinConfidence = *pointConfig.MinConfidence
		}
		resolved[point] = effective
	}
	return resolved, nil
}

func validateMode(mode Mode) error {
	switch mode {
	case "", ModeOff, ModeShadow, ModeActive:
		return nil
	default:
		return fmt.Errorf("mode must be off, shadow, or active")
	}
}

func validateThreshold(threshold *float64) error {
	if threshold == nil {
		return nil
	}
	if math.IsNaN(*threshold) || *threshold < 0 || *threshold > 1 {
		return fmt.Errorf("must be within [0,1]")
	}
	return nil
}

func validateTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout == 0 {
		return decisionrt.DefaultTimeout, nil
	}
	if timeout < 0 || timeout > decisionrt.MaxTimeout {
		return 0, fmt.Errorf("control-decisions.timeout must be within (0,%s]", decisionrt.MaxTimeout)
	}
	return timeout, nil
}
