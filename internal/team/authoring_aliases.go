package team

import "github.com/kjelly/hufu/internal/agent"

// ergonomicDecisionAliases is authoring syntax only. Runtime execution uses
// the materialized profile reference and never branches on these display names.
var ergonomicDecisionAliases = map[string]string{
	"light":       agent.DecisionProfileBuiltinLightV1,
	"standard":    agent.DecisionProfileBuiltinStandardV1,
	"high-stakes": agent.DecisionProfileBuiltinHighStakesV1,
}
