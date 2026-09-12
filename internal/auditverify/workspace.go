package auditverify

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// canonicalLineage streams and validates the workspace's event log without
// constructing an EventStore, then returns the active branch's exact lineage.
// It is deliberately the one lineage computation audit verification performs
// per run; every verification phase operates on the returned slice.
func canonicalLineage(ctx context.Context, workspace string) ([]team.RunEvent, error) {
	var events []team.RunEvent
	if err := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("stream event store: %w", err)
	}

	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return nil, fmt.Errorf("load session tree: %w", err)
	}
	activeBranch := tree.ActiveBranch
	if strings.TrimSpace(activeBranch) == "" {
		activeBranch = "main"
	}
	lineage, err := team.ProjectValidatedEventsForBranch(events, tree, activeBranch)
	if err != nil {
		return nil, fmt.Errorf("project active branch lineage: %w", err)
	}
	return lineage, nil
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
