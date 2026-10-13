package context

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/embedding"
)

// SemanticBuildRepository is the canonical and projection surface the
// builder needs; SQLiteRepository implements it.
type SemanticBuildRepository interface {
	CanonicalEmbeddingInventory
	SemanticProjectionRepository
}

type SemanticBuilderOptions struct {
	// InventoryPolicy defaults to InventoryPolicyRetrievable.
	InventoryPolicy string
	// BatchSize bounds documents per embedding request (default 8).
	BatchSize int
	// MaxBatchBytes bounds the content bytes per request (default 256 KiB).
	MaxBatchBytes int
}

// SemanticBuilder refreshes a project's semantic generation incrementally:
// vectors whose (item ID, content hash) is unchanged are copied forward and
// only new or edited items are embedded. A refresh never modifies the active
// generation in place and never deletes it before a replacement activates.
type SemanticBuilder struct {
	repo          SemanticBuildRepository
	embedder      embedding.Embedder
	model         embedding.ModelIdentity
	policy        string
	batchSize     int
	maxBatchBytes int
}

func NewSemanticBuilder(repo SemanticBuildRepository, embedder embedding.Embedder, options SemanticBuilderOptions) (*SemanticBuilder, error) {
	if repo == nil || embedder == nil {
		return nil, errors.New("semantic builder dependencies are required")
	}
	model := embedder.Identity()
	model.Scheme = model.EffectiveScheme()
	if err := validateModelIdentity(model); err != nil {
		return nil, err
	}
	policy := options.InventoryPolicy
	if policy == "" {
		policy = InventoryPolicyRetrievable
	}
	if !ValidInventoryPolicy(policy) {
		return nil, fmt.Errorf("unknown embedding inventory policy %q", policy)
	}
	batch := options.BatchSize
	if batch <= 0 {
		batch = 8
	}
	maxBytes := options.MaxBatchBytes
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	return &SemanticBuilder{repo: repo, embedder: embedder, model: model, policy: policy, batchSize: batch, maxBatchBytes: maxBytes}, nil
}

// SemanticRefreshPlan is the difference between the current inventory and
// the active generation. Computing it never contacts the encoder.
type SemanticRefreshPlan struct {
	Inventory EmbeddingInventory
	Active    *Generation
	// Reuse holds items the active generation already covers (vector or
	// exclusion) at their current hash.
	Reuse []EmbeddingInventoryItem
	// Embed holds new or edited items.
	Embed []EmbeddingInventoryItem
	// Dropped counts active entries no longer in the inventory.
	Dropped int
	// Excluded counts reused items covered by an exclusion.
	Excluded int
}

// Unchanged reports whether the active generation already matches.
func (p SemanticRefreshPlan) Unchanged() bool {
	return p.Active != nil && len(p.Embed) == 0 && p.Dropped == 0
}

func (b *SemanticBuilder) Plan(ctx context.Context, projectID string) (SemanticRefreshPlan, error) {
	inventory, err := b.repo.LoadEmbeddingInventoryForPolicy(ctx, projectID, b.policy)
	if err != nil {
		return SemanticRefreshPlan{}, fmt.Errorf("load semantic inventory: %w", err)
	}
	plan := SemanticRefreshPlan{Inventory: inventory}
	active, err := b.repo.ActiveGeneration(ctx, projectID, b.model, b.policy)
	switch {
	case errors.Is(err, ErrNoActiveEmbeddingGeneration):
		plan.Embed = inventory.Items
		return plan, nil
	case err != nil:
		return SemanticRefreshPlan{}, fmt.Errorf("load active semantic generation: %w", err)
	}
	plan.Active = &active
	if err = diffSemanticGeneration(ctx, b.repo, &plan); err != nil {
		return SemanticRefreshPlan{}, err
	}
	return plan, nil
}

// diffSemanticGeneration fills Reuse, Embed, Excluded and Dropped of a plan
// whose Inventory and Active are set.
func diffSemanticGeneration(ctx context.Context, repo SemanticProjectionRepository, plan *SemanticRefreshPlan) error {
	vectors, exclusions, err := repo.LoadGenerationEntryHashes(ctx, plan.Active.GenerationID)
	if err != nil {
		return fmt.Errorf("load semantic generation entries: %w", err)
	}
	matched := 0
	for _, item := range plan.Inventory.Items {
		if hash, ok := vectors[item.ItemID]; ok && hash == item.ContentHash {
			plan.Reuse = append(plan.Reuse, item)
			matched++
			continue
		}
		if hash, ok := exclusions[item.ItemID]; ok && hash == item.ContentHash {
			plan.Reuse = append(plan.Reuse, item)
			plan.Excluded++
			matched++
			continue
		}
		plan.Embed = append(plan.Embed, item)
	}
	plan.Dropped = len(vectors) + len(exclusions) - matched
	return nil
}

// PlanAgainstGeneration diffs the policy's current inventory against a
// stored generation without an encoder; operator status and dry runs use it.
// A nil generation means every item would be embedded.
func PlanAgainstGeneration(ctx context.Context, repo SemanticBuildRepository, projectID, policy string, generation *Generation) (SemanticRefreshPlan, error) {
	inventory, err := repo.LoadEmbeddingInventoryForPolicy(ctx, projectID, policy)
	if err != nil {
		return SemanticRefreshPlan{}, fmt.Errorf("load semantic inventory: %w", err)
	}
	plan := SemanticRefreshPlan{Inventory: inventory, Active: generation}
	if generation == nil {
		plan.Embed = inventory.Items
		return plan, nil
	}
	if err = diffSemanticGeneration(ctx, repo, &plan); err != nil {
		return SemanticRefreshPlan{}, err
	}
	return plan, nil
}

// SemanticRefreshResult summarizes one refresh.
type SemanticRefreshResult struct {
	GenerationID         string
	PreviousGenerationID string
	SourceRevision       int64
	Unchanged            bool
	// Activated is false when nothing changed or a newer generation won.
	Activated bool
	Inventory int
	Reused    int
	Embedded  int
	Excluded  int
}

// Refresh brings the project's generation up to date with its inventory.
// Embedding happens before the generation row exists, so a canceled or
// failed refresh leaves the active generation untouched.
func (b *SemanticBuilder) Refresh(ctx context.Context, projectID string) (SemanticRefreshResult, error) {
	plan, err := b.Plan(ctx, projectID)
	if err != nil {
		return SemanticRefreshResult{}, err
	}
	result := SemanticRefreshResult{SourceRevision: plan.Inventory.Revision, Inventory: len(plan.Inventory.Items), Reused: len(plan.Reuse)}
	if plan.Active != nil {
		result.PreviousGenerationID = plan.Active.GenerationID
	}
	if plan.Unchanged() {
		result.GenerationID, result.Unchanged = plan.Active.GenerationID, true
		return result, nil
	}
	rows, exclusions, err := b.embedItems(ctx, plan.Embed)
	if err != nil {
		return result, err
	}
	result.Embedded, result.Excluded = len(rows), len(exclusions)+plan.Excluded

	generation, err := b.repo.BeginGeneration(ctx, GenerationSpec{
		ProjectID: projectID, Model: b.model, InventoryPolicy: b.policy, SourceRevision: plan.Inventory.Revision,
		ExpectedCount: len(plan.Inventory.Items), ContentDigest: plan.Inventory.Digest,
	})
	if err != nil {
		return result, fmt.Errorf("begin semantic generation: %w", err)
	}
	fail := func(cause error) (SemanticRefreshResult, error) {
		if markErr := b.repo.MarkGenerationFailed(context.WithoutCancel(ctx), generation.GenerationID); markErr != nil {
			return result, errors.Join(cause, fmt.Errorf("mark semantic generation failed: %w", markErr))
		}
		return result, cause
	}
	if plan.Active != nil && len(plan.Reuse) > 0 {
		if err = b.repo.CopyGenerationEntries(ctx, plan.Active.GenerationID, generation.GenerationID, plan.Reuse); err != nil {
			return fail(fmt.Errorf("copy semantic entries: %w", err))
		}
	}
	for start := 0; start < len(rows); start += maxEmbeddingRowsPerBatch {
		if err = b.repo.AppendGenerationRows(ctx, generation.GenerationID, rows[start:min(start+maxEmbeddingRowsPerBatch, len(rows))]); err != nil {
			return fail(fmt.Errorf("append semantic rows: %w", err))
		}
	}
	for start := 0; start < len(exclusions); start += maxEmbeddingRowsPerBatch {
		if err = b.repo.AppendGenerationExclusions(ctx, generation.GenerationID, exclusions[start:min(start+maxEmbeddingRowsPerBatch, len(exclusions))]); err != nil {
			return fail(fmt.Errorf("append semantic exclusions: %w", err))
		}
	}
	active, err := b.repo.ActivateGeneration(ctx, generation.GenerationID, plan.Inventory.Revision, plan.Inventory.Digest)
	if err != nil {
		return fail(fmt.Errorf("activate semantic generation: %w", err))
	}
	result.GenerationID, result.Activated = active.GenerationID, active.GenerationID == generation.GenerationID
	// Bound growth from repeated refreshes; the active generation is never
	// a cleanup candidate.
	_, _ = b.repo.CleanupInactiveGenerations(context.WithoutCancel(ctx), GenerationCleanupPolicy{})
	return result, nil
}

// embedItems embeds items in bounded batches. An oversize batch is retried
// one document at a time so only the oversize documents are excluded.
func (b *SemanticBuilder) embedItems(ctx context.Context, items []EmbeddingInventoryItem) ([]EmbeddingRow, []EmbeddingExclusion, error) {
	var rows []EmbeddingRow
	var exclusions []EmbeddingExclusion
	var batch []EmbeddingInventoryItem
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		embedded, excluded, err := b.embedBatch(ctx, batch)
		if err != nil {
			return err
		}
		rows = append(rows, embedded...)
		exclusions = append(exclusions, excluded...)
		batch, batchBytes = batch[:0], 0
		return nil
	}
	for _, item := range items {
		if strings.TrimSpace(item.Content) == "" {
			exclusions = append(exclusions, EmbeddingExclusion{ItemID: item.ItemID, ContentHash: item.ContentHash, Reason: ExclusionEmpty})
			continue
		}
		if len(batch) == b.batchSize || (len(batch) > 0 && batchBytes+len(item.Content) > b.maxBatchBytes) {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
		batch = append(batch, item)
		batchBytes += len(item.Content)
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return rows, exclusions, nil
}

func (b *SemanticBuilder) embedBatch(ctx context.Context, batch []EmbeddingInventoryItem) ([]EmbeddingRow, []EmbeddingExclusion, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	texts := make([]string, len(batch))
	for i, item := range batch {
		texts[i] = item.Content
	}
	vectors, err := b.embedder.EmbedDocuments(ctx, texts)
	if err == nil {
		rows := make([]EmbeddingRow, len(batch))
		for i, item := range batch {
			rows[i] = EmbeddingRow{ItemID: item.ItemID, ContentHash: item.ContentHash, Dimensions: len(vectors[i]), Vector: vectors[i]}
		}
		return rows, nil, nil
	}
	if !errors.Is(err, embedding.ErrEmbeddingInputTooLong) {
		return nil, nil, fmt.Errorf("embed semantic documents: %w", err)
	}
	var rows []EmbeddingRow
	var exclusions []EmbeddingExclusion
	for _, item := range batch {
		vector, itemErr := b.embedder.EmbedDocument(ctx, item.Content)
		switch {
		case errors.Is(itemErr, embedding.ErrEmbeddingInputTooLong):
			exclusions = append(exclusions, EmbeddingExclusion{ItemID: item.ItemID, ContentHash: item.ContentHash, Reason: ExclusionOversize})
		case itemErr != nil:
			return nil, nil, fmt.Errorf("embed semantic document: %w", itemErr)
		default:
			rows = append(rows, EmbeddingRow{ItemID: item.ItemID, ContentHash: item.ContentHash, Dimensions: len(vector), Vector: vector})
		}
	}
	return rows, exclusions, nil
}

// ProjectIDs lists the distinct project IDs with canonical items.
func (r *SQLiteRepository) ProjectIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT DISTINCT project_id FROM context_items ORDER BY project_id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SchemaVersion is the newest applied migration of the open store.
func (r *SQLiteRepository) SchemaVersion() int { return r.schemaVersion }

// SemanticSchemaVersion is the first schema with inventory policies.
const SemanticSchemaVersion = 13

// VerifyActiveGeneration loads the active generation exactly as queries do,
// rejecting wrong dimensions, non-normalized vectors and count mismatches.
func VerifyActiveGeneration(ctx context.Context, repo SemanticProjectionRepository, active Generation) error {
	rows, err := repo.LoadActiveGeneration(ctx, active.GenerationID)
	if err != nil {
		return fmt.Errorf("load semantic generation: %w", err)
	}
	exclusions, err := repo.LoadGenerationExclusions(ctx, active.GenerationID)
	if err != nil {
		return fmt.Errorf("load semantic exclusions: %w", err)
	}
	_, err = buildSemanticSnapshot(active, rows, exclusions)
	return err
}
