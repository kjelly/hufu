package agent

import "fmt"

// ContextRoutingPolicy controls optional context retrieval. Its zero value
// retains legacy behavior; active must be explicitly requested by the team.
type ContextRoutingPolicy struct {
	QueryMode string `yaml:"query-mode" json:"query_mode,omitempty"`
}

func (p ContextRoutingPolicy) Validate() error {
	switch p.QueryMode {
	case "", "legacy", "active":
		return nil
	default:
		return fmt.Errorf("context-routing.query-mode must be legacy or active, got %q", p.QueryMode)
	}
}
