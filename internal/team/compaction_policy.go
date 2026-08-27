package team

import "github.com/kjelly/hufu/internal/agent"

// CompactionPolicy is exposed from the team package because team loading and
// coordinator behavior are the policy owners. The underlying type remains in
// agent so TeamConfig does not introduce an import cycle.
type CompactionPolicy = agent.CompactionPolicy

// DefaultCompactionPolicy returns the built-in team safety limits.
func DefaultCompactionPolicy() CompactionPolicy {
	return agent.DefaultCompactionPolicy()
}

func defaultCompactionPolicy() CompactionPolicy {
	return DefaultCompactionPolicy()
}

func (c *Coordinator) compactionPolicy() CompactionPolicy {
	if c != nil && c.session != nil {
		policy := c.session.Config.Compaction
		if policy.Validate() == nil {
			return policy
		}
	}
	return defaultCompactionPolicy()
}
