package promotion

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type iterationOrderingEligibilityRepository struct {
	item      contextstore.ContextItem
	iterating bool
}

func (r *iterationOrderingEligibilityRepository) Iterate(_ context.Context, q contextstore.RepositoryQuery, visit func(contextstore.ContextItem) error) error {
	if q.Limit != 0 {
		return fmt.Errorf("unexpected iteration limit %d", q.Limit)
	}
	r.iterating = true
	defer func() { r.iterating = false }()
	return visit(r.item)
}

func (r *iterationOrderingEligibilityRepository) ExperienceAggregate(_ context.Context, id, _ string) (contextstore.ExperienceAggregate, error) {
	if r.iterating {
		return contextstore.ExperienceAggregate{}, fmt.Errorf("aggregate lookup while iterator is open")
	}
	if id != r.item.ID {
		return contextstore.ExperienceAggregate{}, sql.ErrNoRows
	}
	return contextstore.ExperienceAggregate{ContextItemID: id, AppliedCount: 2, VerifiedSupportCount: 2, IndependentTaskCount: 2}, nil
}

func (r *iterationOrderingEligibilityRepository) OpenConflictsForItems(context.Context, string, string, []string) (map[string][]string, error) {
	if r.iterating {
		return nil, fmt.Errorf("conflict lookup while iterator is open")
	}
	return map[string][]string{}, nil
}

func TestEligibleSourcesClosesIteratorBeforeAggregateLookups(t *testing.T) {
	repo := &iterationOrderingEligibilityRepository{item: contextstore.ContextItem{
		ID: "eligible", Kind: contextstore.ContextPattern, Content: "verified reusable practice",
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed,
		Metadata: map[string]string{"memory_lifetime": "persistent"},
	}}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.MinConfirmedSupport = 2
	policy.MinIndependentTasks = 2
	got, diagnostics, err := EligibleSources(t.Context(), repo, EligibilityOptions{ProjectID: "project", TeamID: "team", PolicyVersion: "v1"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Item.ID != "eligible" || len(diagnostics) != 0 {
		t.Fatalf("eligible=%v diagnostics=%v", got, diagnostics)
	}
}
