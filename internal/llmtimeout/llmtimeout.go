// Package llmtimeout holds the configured timeouts for model calls. The
// timeouts are grouped into four categories so a slow model or provider can be
// given more time without a setting per call site. A category left unset keeps
// each call's own default.
package llmtimeout

import (
	"fmt"
	"sync/atomic"
	"time"
)

// maxSetting bounds every configured value so a typo cannot leave a run
// waiting on one model call for days.
const maxSetting = time.Hour

// Settings is the hufu.yaml timeouts: block.
type Settings struct {
	// Decision is the largest timeout a decision-primitives entry or the
	// control-decisions block may request (jev/systemone calls).
	Decision time.Duration `yaml:"decision,omitempty"`
	// Sidecar covers short helper calls: duplicate-task similarity, failure
	// reflection, and skill naming and clustering.
	Sidecar time.Duration `yaml:"sidecar,omitempty"`
	// Review covers long reasoning calls: judge, skeptic, decision stages,
	// skill analysis, explain and fix.
	Review time.Duration `yaml:"review,omitempty"`
	// Provider covers provider queries: model listing and lookup, model
	// validation, context-window and embedding probes, catalog updates.
	Provider time.Duration `yaml:"provider,omitempty"`
}

// Merge returns s with each field that override sets replaced.
func (s Settings) Merge(override Settings) Settings {
	for _, field := range []struct{ dst, src *time.Duration }{
		{&s.Decision, &override.Decision}, {&s.Sidecar, &override.Sidecar},
		{&s.Review, &override.Review}, {&s.Provider, &override.Provider},
	} {
		if *field.src != 0 {
			*field.dst = *field.src
		}
	}
	return s
}

// Validate rejects negative values and values above one hour.
func (s Settings) Validate() error {
	for _, field := range []struct {
		name  string
		value time.Duration
	}{{"decision", s.Decision}, {"sidecar", s.Sidecar}, {"review", s.Review}, {"provider", s.Provider}} {
		if field.value < 0 || field.value > maxSetting {
			return fmt.Errorf("timeouts.%s must be within [0,%s], got %s", field.name, maxSetting, field.value)
		}
	}
	return nil
}

var current atomic.Pointer[Settings]

// Configure sets the process-wide timeouts. The CLI calls it once after
// loading hufu.yaml; invalid settings are rejected and leave the defaults.
func Configure(settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	current.Store(&settings)
	return nil
}

func pick(selected func(Settings) time.Duration, fallback time.Duration) time.Duration {
	if settings := current.Load(); settings != nil {
		if value := selected(*settings); value > 0 {
			return value
		}
	}
	return fallback
}

// DecisionMax returns the configured decision timeout limit, or fallback.
func DecisionMax(fallback time.Duration) time.Duration {
	return pick(func(s Settings) time.Duration { return s.Decision }, fallback)
}

// Sidecar returns the configured sidecar timeout, or fallback.
func Sidecar(fallback time.Duration) time.Duration {
	return pick(func(s Settings) time.Duration { return s.Sidecar }, fallback)
}

// Review returns the configured review timeout, or fallback.
func Review(fallback time.Duration) time.Duration {
	return pick(func(s Settings) time.Duration { return s.Review }, fallback)
}

// Provider returns the configured provider timeout, or fallback.
func Provider(fallback time.Duration) time.Duration {
	return pick(func(s Settings) time.Duration { return s.Provider }, fallback)
}
