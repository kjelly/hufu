package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

const (
	executionRouteInvalidCode  = "execution_route_invalid"
	executionRouteConflictCode = "execution_route_conflict"
	// executionRouteMaxCandidates bounds a route's ordered targets.
	executionRouteMaxCandidates = 4
)

// ExecutionRouteDefinition is one compiled hufu.yaml execution route: an
// ordered list of canonical LLM targets. Candidates[0] is the primary.
type ExecutionRouteDefinition struct {
	Name       string
	Candidates []execution.ExecutionTarget
	FallbackOn []ProviderFailureClass
	// Digest is the sha256 of the canonical JSON of {name, candidates,
	// fallback_on}; the policy snapshot pins it.
	Digest string
}

// ExecutionRouteBinding is the route frozen into a task occurrence at
// admission. Candidates[0] always equals the occurrence's ExecutionTarget;
// ExecutionTopology stays [primary], so fan-out semantics are unchanged.
type ExecutionRouteBinding struct {
	Name       string                      `json:"name"`
	Digest     string                      `json:"digest"`
	Candidates []execution.ExecutionTarget `json:"candidates"`
	FallbackOn []ProviderFailureClass      `json:"fallback_on,omitempty"`
}

func (b *ExecutionRouteBinding) clone() *ExecutionRouteBinding {
	if b == nil {
		return nil
	}
	clone := *b
	clone.Candidates = slices.Clone(b.Candidates)
	clone.FallbackOn = slices.Clone(b.FallbackOn)
	return &clone
}

func (d *ExecutionRouteDefinition) binding() *ExecutionRouteBinding {
	if d == nil {
		return nil
	}
	return &ExecutionRouteBinding{
		Name: d.Name, Digest: d.Digest,
		Candidates: slices.Clone(d.Candidates), FallbackOn: slices.Clone(d.FallbackOn),
	}
}

func executionRouteDigest(name string, candidates []execution.ExecutionTarget, fallbackOn []ProviderFailureClass) string {
	encoded, _ := json.Marshal(struct {
		Name       string                      `json:"name"`
		Candidates []execution.ExecutionTarget `json:"candidates"`
		FallbackOn []ProviderFailureClass      `json:"fallback_on"`
	}{Name: name, Candidates: candidates, FallbackOn: fallbackOn})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// executionRouteFor returns the route bound to a worker, or nil. Bindings are
// resolved once at team load, after CLI overrides: an agent a -m or
// --worker-model override targets is never route-bound.
func (c *Coordinator) executionRouteFor(def *agent.AgentDef) *ExecutionRouteDefinition {
	if c == nil || c.session == nil || def == nil {
		return nil
	}
	return c.session.executionRouteFor(def)
}

func (s *TeamSession) executionRouteFor(def *agent.AgentDef) *ExecutionRouteDefinition {
	if s == nil || def == nil {
		return nil
	}
	name, ok := s.AgentExecutionRoutes[strings.ToLower(strings.TrimSpace(def.Name))]
	if !ok {
		return nil
	}
	return s.ExecutionRoutes[name]
}

// teamRouteTarget returns the first candidate of the team-level execution
// route: the worker default for a worker the team did not bind, such as one
// resolved by name at dispatch time. It is "" when the team has no route.
func (s *TeamSession) teamRouteTarget() string {
	if s == nil || s.Config.ExecutionRoute == "" {
		return ""
	}
	route := s.ExecutionRoutes[s.Config.ExecutionRoute]
	if route == nil || len(route.Candidates) == 0 {
		return ""
	}
	return route.Candidates[0].String()
}

// ExecutionRoutePolicySnapshot pins one worker's bound route in the
// execution policy snapshot.
type ExecutionRoutePolicySnapshot struct {
	Owner  string `json:"owner"`
	Route  string `json:"route"`
	Digest string `json:"digest"`
}

// executionPolicyExecutionRoutes lists every route binding in a deterministic
// order. A changed route or binding changes the policy snapshot and fails
// resume closed; a team without routes contributes nothing.
func executionPolicyExecutionRoutes(session *TeamSession) []ExecutionRoutePolicySnapshot {
	if session == nil {
		return nil
	}
	var bindings []ExecutionRoutePolicySnapshot
	for owner, name := range session.AgentExecutionRoutes {
		route := session.ExecutionRoutes[name]
		if route == nil {
			continue
		}
		bindings = append(bindings, ExecutionRoutePolicySnapshot{Owner: "agent:" + owner, Route: route.Name, Digest: route.Digest})
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Owner < bindings[j].Owner })
	return bindings
}
