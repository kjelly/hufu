package execution

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var backendNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ExecutionSelector is an untrusted user or configuration input. A selector
// may be bare and therefore is not suitable for durable state.
type ExecutionSelector struct {
	Raw     string
	Backend string
	Model   string
}

// ExecutionTarget is the fully-qualified, canonical worker identity. Its
// default JSON/YAML struct encoding is intentionally the durable wire format.
type ExecutionTarget struct {
	Backend string `json:"backend" yaml:"backend"`
	Model   string `json:"model" yaml:"model"`
}

// ParseExecutionSelector parses one selector without resolving a bare model
// to a configured default. Model remainders may contain additional slashes.
func ParseExecutionSelector(raw string) (ExecutionSelector, error) {
	if raw == "" {
		return ExecutionSelector{}, fmt.Errorf("execution selector is required")
	}
	if strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return ExecutionSelector{}, fmt.Errorf("execution selector %q contains whitespace", raw)
	}

	backend, model, qualified := strings.Cut(raw, "/")
	if !qualified {
		return ExecutionSelector{Raw: raw, Model: raw}, nil
	}
	if backend == "" {
		return ExecutionSelector{}, fmt.Errorf("execution selector %q has no backend", raw)
	}
	if model == "" {
		return ExecutionSelector{}, fmt.Errorf("execution selector %q has no model", raw)
	}
	backend = CanonicalBackendName(backend)
	if !backendNamePattern.MatchString(backend) {
		return ExecutionSelector{}, fmt.Errorf("invalid execution backend %q", backend)
	}
	return ExecutionSelector{Raw: raw, Backend: backend, Model: model}, nil
}

// CanonicalBackendName returns the stable backend identity used in durable
// targets. Callers that accept a configured backend must still validate that it
// exists in their registry.
func CanonicalBackendName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "ollama" {
		return "local"
	}
	return name
}

// String returns the canonical human-facing representation.
func (t ExecutionTarget) String() string {
	if t.Backend == "" {
		return t.Model
	}
	return t.Backend + "/" + t.Model
}

// IsZero reports whether neither identity component is set.
func (t ExecutionTarget) IsZero() bool {
	return t.Backend == "" && t.Model == ""
}

// Validate verifies that a target is fully qualified and safe to use as a
// canonical identity. Backend registration and capability checks are owned by
// the team runtime, not this dependency-light package.
func (t ExecutionTarget) Validate() error {
	if t.Backend == "" {
		return fmt.Errorf("execution target backend is required")
	}
	if t.Model == "" {
		return fmt.Errorf("execution target model is required")
	}
	if strings.TrimSpace(t.Backend) != t.Backend || !backendNamePattern.MatchString(t.Backend) {
		return fmt.Errorf("invalid execution backend %q", t.Backend)
	}
	if strings.TrimSpace(t.Model) != t.Model || strings.IndexFunc(t.Model, unicode.IsSpace) >= 0 {
		return fmt.Errorf("invalid execution target model %q", t.Model)
	}
	if CanonicalBackendName(t.Backend) != t.Backend {
		return fmt.Errorf("execution target backend %q is not canonical", t.Backend)
	}
	return nil
}
