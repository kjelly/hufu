package taskdistill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// SaveKnowledge is an idempotent, all-or-nothing context-store write. The
// operator's application of the reviewed plan is the confirmation boundary;
// model-generated text alone never confers confirmed status.
func SaveKnowledge(ctx context.Context, repo *contextstore.SQLiteRepository, scope contextstore.Scope, planID string, sources []Source, draft Draft) error {
	if err := Validate(draft, sources); err != nil {
		return err
	}
	if scope.ProjectID == "" || scope.TeamID == "" || planID == "" {
		return errors.New("distillation knowledge requires project, team and plan identity")
	}
	sourceHashes := make(map[string]string, len(sources))
	for _, source := range sources {
		sourceHashes[source.ID] = Digest(source)
	}
	var items []contextstore.ContextItem
	for index, knowledge := range draft.Knowledge {
		item := contextstore.ContextItem{
			ID: fmt.Sprintf("distilled-%s-%d", planID, index), Kind: contextstore.ContextPattern,
			Content: contextstore.NormalizeContent(strings.TrimSpace(knowledge.Text)), Scope: scope,
			Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
			Priority: contextstore.PriorityNormal, Confidence: 1, Lifecycle: contextstore.LifecycleConfirmed,
			Source:   contextstore.SourceRef{Type: "task_distillation", Ref: planID},
			Metadata: map[string]string{"run_id": "distillation:" + planID, "task_id": fmt.Sprint(index), "attempt": "1", "distillation_plan": planID},
		}
		for _, id := range knowledge.SourceIDs {
			item.Evidence = append(item.Evidence, contextstore.EvidenceRef{Type: "distilled_task_snapshot", Ref: id + "@sha256:" + sourceHashes[id]})
		}
		existing, err := repo.Get(ctx, item.ID)
		if err == nil {
			if existing.Content != item.Content || existing.Scope != item.Scope || existing.Lifecycle != item.Lifecycle || existing.Source != item.Source || Digest(existing.Evidence) != Digest(item.Evidence) || existing.SupersededBy != "" || existing.ExpiresAt != nil {
				return errors.New("saved distillation knowledge changed; refusing cleanup")
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		items = append(items, item)
	}
	if len(items) > 0 {
		return repo.AppendReducer(ctx, items...)
	}
	return nil
}
