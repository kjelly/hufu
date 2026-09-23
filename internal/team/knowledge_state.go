package team

import (
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// KnowledgeState is the deterministic attribution attached to an injected
// context item. Conflicting marks a historical item with an open memory
// conflict recorded by hufu context conflicts; the classification is
// deterministic over those persisted judgments.
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
func classifyKnowledgeState(authority ContextAuthority, aggregate *contextstore.ExperienceAggregate, now time.Time, policy agent.MemoryLearningPolicy, conflicting bool) (KnowledgeState, bool) {
	switch authority {
	case ContextAuthorityNormative:
		return KnowledgeKnown, true
	case ContextAuthorityExample:
		return "", false
	case ContextAuthorityHistorical:
	default:
		return "", false
	}
	if conflicting {
		return KnowledgeConflicting, true
	}
	if aggregate == nil || aggregate.VerifiedSupportCount < policy.MinConfirmedSupport || aggregate.IndependentTaskCount < policy.MinIndependentTasks {
		return KnowledgeAssumed, true
	}
	if policy.StaleAfter > 0 && now.Sub(aggregate.LastObservedAt) > policy.StaleAfter {
		return KnowledgeStale, true
	}
	return KnowledgeKnown, true
}

type InvariantCoverageSignal struct {
	TouchedPathCount         int `json:"touched_path_count"`
	ApplicableInvariantCount int `json:"applicable_invariant_count"`
	// UncoveredPathCount is the structural unknown signal: touched paths that
	// match no repository invariant.
	UncoveredPathCount int `json:"uncovered_path_count"`
}

type OutcomeCoverageSignal struct {
	IncludedItemCount int `json:"included_item_count"`
	KnownCount        int `json:"known_count"`
	AssumedCount      int `json:"assumed_count"`
	StaleCount        int `json:"stale_count"`
	// ConflictingCount is omitted when zero so coverage JSON without
	// conflicts keeps its existing shape.
	ConflictingCount int `json:"conflicting_count,omitempty"`
}

type TaskKnowledgeCoverage struct {
	InvariantCoverage InvariantCoverageSignal `json:"invariant_coverage"`
	OutcomeCoverage   OutcomeCoverageSignal   `json:"outcome_coverage"`
}

// ComputeTaskKnowledgeCoverage is a pure diagnostic projection over already
// persisted context attribution and the repository invariant catalog.
func ComputeTaskKnowledgeCoverage(manifest *ContextInjectionManifest, catalog []InvariantDefinition, touchedPaths []string) TaskKnowledgeCoverage {
	var coverage TaskKnowledgeCoverage
	if manifest != nil {
		for _, item := range manifest.Items {
			if !item.Included || item.KnowledgeState == "" {
				continue
			}
			coverage.OutcomeCoverage.IncludedItemCount++
			switch item.KnowledgeState {
			case KnowledgeKnown:
				coverage.OutcomeCoverage.KnownCount++
			case KnowledgeAssumed:
				coverage.OutcomeCoverage.AssumedCount++
			case KnowledgeStale:
				coverage.OutcomeCoverage.StaleCount++
			case KnowledgeConflicting:
				coverage.OutcomeCoverage.ConflictingCount++
			}
		}
	}
	if len(touchedPaths) == 0 {
		return coverage
	}
	coverage.InvariantCoverage.TouchedPathCount = len(touchedPaths)
	for _, definition := range catalog {
		if invariantAppliesToTouchedPaths(definition, touchedPaths) {
			coverage.InvariantCoverage.ApplicableInvariantCount++
		}
	}
	for _, path := range touchedPaths {
		covered := false
		for _, definition := range catalog {
			if invariantAppliesToTouchedPaths(definition, []string{path}) {
				covered = true
				break
			}
		}
		if !covered {
			coverage.InvariantCoverage.UncoveredPathCount++
		}
	}
	return coverage
}

// attachTaskKnowledgeCoverage adds best-effort report-only attribution to a
// runtime result. Missing, ambiguous, legacy, or corrupt manifests leave the
// field nil and never affect result acceptance.
func (c *Coordinator) attachTaskKnowledgeCoverage(todoID string, attempt int, modelExecutionID string, result *TaskResult) {
	if c == nil || c.session == nil || result == nil {
		return
	}
	todo := c.todoItemByID(todoID)
	manifest, err := c.invariantContextManifest(todo, attempt, modelExecutionID)
	if err != nil {
		return
	}
	var touchedPaths []string
	if todo.WorksetBinding != nil {
		touchedPaths = todo.WorksetBinding.TouchedPaths
	}
	coverage := ComputeTaskKnowledgeCoverage(manifest, c.session.InvariantCatalog, touchedPaths)
	result.KnowledgeCoverage = new(coverage)
}
