package team

import (
	"context"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// FilteredContextItem is a canonical record the compiler dropped before
// budgeting. It carries no content: the manifest records only why a routed
// item never reached the prompt.
type FilteredContextItem struct {
	ID     string
	Kind   string
	Source string
	Reason ContextDecisionReason
}

// contextEvaluationTime is the time the router judged the bundle's validity
// at. Compiling against the same instant keeps an item from being eligible at
// route time and expired at compile time.
func contextEvaluationTime(ctx context.Context, bundle *CanonicalContextBundle) time.Time {
	if bundle != nil && !bundle.EvaluatedAt.IsZero() {
		return bundle.EvaluatedAt.UTC()
	}
	return contextstore.EvaluationTime(ctx)
}

// canonicalRecordFilterReason reports why the compiler drops a canonical
// record, or "" when the record is compiled.
func canonicalRecordFilterReason(record contextstore.ContextItem, now time.Time, workerSTMOnly, includeCandidates bool) ContextDecisionReason {
	// Shared session STM may carry the current run's candidates (visible to
	// the run's own prompts); persistent LTM never does. The coordinator
	// already filters SharedSession to confirmed + current-run candidates, so
	// includeCandidates only relaxes the lifecycle guard for that list.
	if record.Lifecycle == contextstore.LifecycleCandidate {
		if !includeCandidates {
			return ContextOmittedLifecycle
		}
	} else if record.Lifecycle != "" && record.Lifecycle != contextstore.LifecycleConfirmed {
		return ContextOmittedLifecycle
	}
	switch {
	case record.SupersededBy != "":
		return ContextOmittedLifecycle
	case strings.TrimSpace(record.Content) == "":
		return ContextOmittedEmptyContent
	case record.ExpiresAt != nil && !now.Before(*record.ExpiresAt):
		return ContextOmittedExpired
	case workerSTMOnly && record.Kind == contextstore.ContextProgress:
		return ContextOmittedWorkerSTMOnly
	}
	return ""
}

func canonicalFilteredItems(records []contextstore.ContextItem, source string, workerSTMOnly, includeCandidates bool, now time.Time) []FilteredContextItem {
	var filtered []FilteredContextItem
	for _, record := range records {
		if reason := canonicalRecordFilterReason(record, now, workerSTMOnly, includeCandidates); reason != "" {
			filtered = append(filtered, FilteredContextItem{ID: "context:" + record.ID, Kind: string(record.Kind), Source: source, Reason: reason})
		}
	}
	return filtered
}

// canonicalBundleFilteredItems mirrors the compiler's canonical inputs: the
// shared session list admits current-run candidates, and only a worker drops
// session progress records.
func canonicalBundleFilteredItems(bundle *CanonicalContextBundle, workerSTMOnly bool, now time.Time) []FilteredContextItem {
	if bundle == nil {
		return nil
	}
	filtered := canonicalFilteredItems(bundle.SharedSession, "shared_session", workerSTMOnly, true, now)
	return append(filtered, canonicalFilteredItems(bundle.SharedPersistent, "shared_persistent", false, false, now)...)
}
