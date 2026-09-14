package team

import (
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// KnowledgeState is the deterministic attribution attached to an injected
// context item. Conflicting is reserved for a future contradiction model and
// is never produced by classifyKnowledgeState in this version.
type KnowledgeState string

const (
	KnowledgeKnown       KnowledgeState = "known"
	KnowledgeAssumed     KnowledgeState = "assumed"
	KnowledgeStale       KnowledgeState = "stale"
	KnowledgeConflicting KnowledgeState = "conflicting"
)

func validKnowledgeState(state KnowledgeState) bool {
	switch state {
	case KnowledgeKnown, KnowledgeAssumed, KnowledgeStale, KnowledgeConflicting:
		return true
	default:
		return false
	}
}

// classifyKnowledgeState is pure. Unknown authorities and example content are
// deliberately left unclassified instead of being assigned a guessed state.
func classifyKnowledgeState(authority ContextAuthority, aggregate *contextstore.ExperienceAggregate, now time.Time, policy agent.MemoryLearningPolicy) (KnowledgeState, bool) {
	switch authority {
	case ContextAuthorityNormative:
		return KnowledgeKnown, true
	case ContextAuthorityExample:
		return "", false
	case ContextAuthorityHistorical:
	default:
		return "", false
	}
	if aggregate == nil || aggregate.VerifiedSupportCount < policy.MinConfirmedSupport || aggregate.IndependentTaskCount < policy.MinIndependentTasks {
		return KnowledgeAssumed, true
	}
	if policy.StaleAfter > 0 && now.Sub(aggregate.LastObservedAt) > policy.StaleAfter {
		return KnowledgeStale, true
	}
	return KnowledgeKnown, true
}
