package team

import "context"

func (r *coordinatorBeliefs) suggestPolicies(ctx context.Context) {
	hasPolicy := false
	for _, view := range r.views {
		if len(view.Policies) > 0 {
			hasPolicy = true
			break
		}
	}
	if !hasPolicy {
		return
	}
	events, err := r.journal.events(ctx)
	if err != nil {
		r.unavailable(ctx, "", "", "replay_unavailable")
		return
	}
	changed := false
	for _, view := range r.views {
		if len(view.Policies) == 0 {
			continue
		}
		reducer, err := reduceBeliefRun(ctx, events, r.journal.artifacts, view.RunID)
		if err != nil {
			r.unavailable(ctx, "", "", "replay_unavailable")
			return
		}
		last := map[string]BeliefPolicySuggestion{}
		for _, suggestion := range view.Suggestions {
			last[suggestion.Record.EpochID] = suggestion.Record
		}
		for _, epoch := range view.Epochs {
			if epoch.Epoch.Scope.BranchID != r.branchID {
				continue
			}
			input, err := reducer.policyContext(epoch.Epoch.ID)
			if err != nil {
				r.unavailable(ctx, epoch.Epoch.Scope.TaskID, "", "bookkeeping_unavailable")
				return
			}
			digest := beliefPolicySnapshotDigest(epoch, input.actor)
			if last[epoch.Epoch.ID].SnapshotDigest == digest {
				continue
			}
			writer, err := NewBeliefJournal(r.journal.journal, r.journal.artifacts, r.journal.tree, r.branchID, view.RunID, input.cursor.SessionID)
			if err != nil {
				r.unavailable(ctx, epoch.Epoch.Scope.TaskID, input.cursor.ID, "bookkeeping_unavailable")
				return
			}
			if _, err := writer.SuggestPolicy(ctx, epoch.Epoch.ID, digest, last[epoch.Epoch.ID].DecisionGeneration); err != nil {
				r.diagnostic(ctx, writer, epoch.Epoch.Scope.TaskID, input.cursor.ID, "bookkeeping_unavailable")
			}
			changed = true
		}
	}
	if changed {
		r.rebuild(ctx)
	}
}
