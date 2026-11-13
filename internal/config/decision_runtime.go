package config

import (
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

// ResolveDecisionRuntime refuses invalid layers even if a later file overrides them.
func (c *Config) ResolveDecisionRuntime() (settings.Config, error) {
	if c.decisionRuntimeError != nil {
		return settings.Config{}, c.decisionRuntimeError
	}
	return control.NormalizeTransport(c.DecisionRuntime, c.ControlDecisions, "global")
}
