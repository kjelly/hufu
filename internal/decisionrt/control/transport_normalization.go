package control

import (
	"fmt"

	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

// HasTransport reports explicit legacy connection settings, independently of
// whether any control point is active.
func (c Config) HasTransport() bool {
	return c.Backend != "" || c.Model != "" || c.Endpoint != "" || c.APIKeyEnv != "" || c.Timeout != 0
}

// PolicyOnly leaves mode and confidence policy under the control service's
// ownership. Shared transport is resolved before global/team layers merge.
func (c Config) PolicyOnly() Config {
	c = c.Clone()
	c.Backend, c.Model, c.Endpoint, c.APIKeyEnv, c.Timeout = "", "", "", "", 0
	return c
}

// NormalizeTransport resolves aliases within one configuration layer. It does
// not apply defaults: absent transport must preserve entrypoint defaults.
func NormalizeTransport(c settings.Config, legacy Config, source string) (settings.Config, error) {
	c = c.Clone()
	if err := c.Validate(); err != nil {
		return settings.Config{}, fmt.Errorf("%s: %w", source, err)
	}
	if err := legacy.Validate(); err != nil {
		return settings.Config{}, fmt.Errorf("%s: %w", source, err)
	}
	if c.Sources == nil {
		c.Sources = make(map[string]string)
	}
	for _, key := range []string{"backend", "systemone.model", "systemone.endpoint", "systemone.api-key-env", "systemone.timeout", "sidecar.timeout"} {
		if c.Sources[key] == "" {
			c.Sources[key] = source
		}
	}
	if !legacy.HasTransport() {
		return c, nil
	}
	backend, err := legacy.backend()
	if err != nil {
		return settings.Config{}, fmt.Errorf("%s: %w", source, err)
	}
	type transportField struct {
		name  string
		key   string
		dst   *string
		value string
	}
	fields := []transportField{
		{"backend", "backend", &c.Backend, backend},
	}
	if backend == BackendSystemOne {
		fields = append(fields,
			transportField{"model", "systemone.model", &c.SystemOne.Model, legacy.Model},
			transportField{"endpoint", "systemone.endpoint", &c.SystemOne.Endpoint, legacy.Endpoint},
			transportField{"api-key-env", "systemone.api-key-env", &c.SystemOne.APIKeyEnv, legacy.APIKeyEnv},
		)
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		if *field.dst != "" && *field.dst != field.value {
			return settings.Config{}, fmt.Errorf("%s: conflicting decision-runtime.%s and control-decisions.%s", source, field.key, field.name)
		}
		if *field.dst == "" {
			*field.dst = field.value
			c.Sources[field.key] = source + ":control-decisions." + field.name
		}
	}
	timeout, key := &c.SystemOne.Timeout, "systemone.timeout"
	if backend == BackendSidecar {
		timeout, key = &c.Sidecar.Timeout, "sidecar.timeout"
	}
	if legacy.Timeout != 0 {
		if *timeout != 0 && *timeout != legacy.Timeout {
			return settings.Config{}, fmt.Errorf("%s: conflicting decision-runtime.%s and control-decisions.timeout", source, key)
		}
		if *timeout == 0 {
			*timeout = legacy.Timeout
			c.Sources[key] = source + ":control-decisions.timeout"
		}
	}
	if err := c.Validate(); err != nil {
		return settings.Config{}, fmt.Errorf("%s: %w", source, err)
	}
	return c, nil
}
