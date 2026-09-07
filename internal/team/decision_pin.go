package team

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Pinned binding (spec.md v2 §34).
//
// A pin forces a role to one named agent instead of ranking candidates — but
// it can only replace *which* already-qualified candidate is chosen, never
// skip the authorization or required-capability checks a ranked resolution
// would also apply. A profile must separately opt in via
// discipline.routing.allow-pinned-binding before any pin is honored at all
// (enforced at config-validation time, agent.requirePinPermitted).

// resolvePinnedCandidate returns the pinned agent for a role, if pin is set,
// after confirming it is still authorized (delegation.allowed-workers) and
// still satisfies required. ok is false when pin is nil, so callers fall
// through to normal ranking; ok is never true alongside a non-nil error.
func resolvePinnedCandidate(ctx context.Context, c *Coordinator, roleName string, required, preferred []string, pin *agent.RoutingPin) (agentID string, def *agent.AgentDef, ok bool, err error) {
	if pin == nil {
		return "", nil, false, nil
	}
	if c == nil || c.session == nil {
		return "", nil, false, fmt.Errorf("%s role routing requires an active session", roleName)
	}
	pinned := strings.TrimSpace(pin.Agent)
	if !slices.Contains(c.eligibleWorkerIDs(), pinned) {
		return "", nil, false, fmt.Errorf("%s role routing: pinned agent %q is not authorized", roleName, pinned)
	}
	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{Required: required, Preferred: preferred})
	if err != nil {
		return "", nil, false, fmt.Errorf("%s role routing: %w", roleName, err)
	}
	qualifies := false
	for _, candidate := range candidates {
		if candidate.AgentID == pinned && candidate.Score > 0 {
			qualifies = true
			break
		}
	}
	if !qualifies {
		return "", nil, false, fmt.Errorf("%s role routing: pinned agent %q does not satisfy required capabilities %v", roleName, pinned, required)
	}
	pinnedDef := c.session.Agents[pinned]
	if pinnedDef == nil {
		return "", nil, false, fmt.Errorf("%s role routing: pinned agent %q is not a configured agent", roleName, pinned)
	}
	return pinned, pinnedDef, true, nil
}
