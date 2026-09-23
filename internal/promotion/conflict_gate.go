package promotion

import (
	"context"
	"fmt"
)

// validateNoOpenConflicts refuses a proposal whose sources have an open
// memory conflict. It is a blocking check, not a stale transition: the
// proposal identity cannot change after a dismiss or supersede, so marking it
// stale would be permanent. Once the conflict is resolved, apply succeeds
// without a new analyze.
func (s Service) validateNoOpenConflicts(ctx context.Context, p Proposal) error {
	ids := make([]string, 0, len(p.Sources))
	for _, source := range p.Sources {
		ids = append(ids, source.ContextItemID)
	}
	conflicts, err := s.Repo.OpenConflictsForItems(ctx, p.ProjectID, p.TeamID, ids)
	if err != nil {
		return fmt.Errorf("check memory conflicts for promotion %s: %w", p.ID, err)
	}
	for _, id := range ids {
		if found := conflicts[id]; len(found) > 0 {
			return fmt.Errorf("promotion source %s has an unresolved memory conflict %s; supersede or dismiss it, then apply again", id, found[0])
		}
	}
	return nil
}
