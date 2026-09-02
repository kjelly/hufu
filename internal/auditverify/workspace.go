package auditverify

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// canonicalLineage opens the workspace's EventStore, validates it can be read
// at all (team.EventStore already fails closed on a broken chain at open
// time), and returns the active branch's lineage exactly the way
// team.LoadCanonicalRunFinishedSnapshot and the coordinator's own event-first
// projections do (OpenEventStore -> ReadEvents -> LoadSessionTree ->
// FilterEventsForBranch). This is deliberately the one lineage computation
// audit verification performs per run (spec.md §46's "single scan"
// requirement); every phase below operates on the slice returned here.
func canonicalLineage(workspace string) ([]team.RunEvent, error) {
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		return nil, fmt.Errorf("open event store: %w", err)
	}
	defer func() { _ = store.Close() }()

	events, err := store.ReadEvents()
	if err != nil {
		return nil, fmt.Errorf("read event store: %w", err)
	}

	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return nil, fmt.Errorf("load session tree: %w", err)
	}
	activeBranch := tree.ActiveBranch
	if strings.TrimSpace(activeBranch) == "" {
		activeBranch = "main"
	}
	return team.FilterEventsForBranch(events, tree, activeBranch), nil
}

// runTerminalEvents returns every run_finished event in lineage for runID,
// preserving lineage order. runEventExists reports whether runID appears in
// lineage at all (via any event type), which distinguishes an unknown run id
// from a known run that never reached a terminal event.
func runTerminalEvents(lineage []team.RunEvent, runID string) (terminals []team.RunEvent, runEventExists bool) {
	for _, event := range lineage {
		if event.RunID != runID {
			continue
		}
		runEventExists = true
		if event.Type == "run_finished" {
			terminals = append(terminals, event)
		}
	}
	return terminals, runEventExists
}

// terminalConflict reports whether two or more run_finished events for the
// same run carry different hashes (spec.md §11 step 8). A second append of
// byte-identical content is not a conflict: EventStore's own idempotency-key
// dedup means duplicates can only arise via out-of-band tampering of the
// durable log, and even then the records would agree.
func terminalConflict(terminals []team.RunEvent) bool {
	for i := 1; i < len(terminals); i++ {
		if terminals[i].Hash != terminals[0].Hash {
			return true
		}
	}
	return false
}
