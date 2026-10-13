package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Generation entries are vector rows plus exclusions. These helpers copy,
// list and validate them; semantic_projection.go owns generation lifecycle.

// buildingGeneration loads generationID inside tx and requires it to be
// building.
func buildingGeneration(ctx context.Context, tx *sql.Tx, generationID string) (Generation, error) {
	generation, err := loadGeneration(ctx, tx, generationID)
	if err != nil {
		return Generation{}, generationLookupError(err)
	}
	if generation.State != GenerationBuilding {
		return Generation{}, fmt.Errorf("embedding generation %s is %s, not building", generationID, generation.State)
	}
	return generation, nil
}

// requireCanonicalHash fails with ErrEmbeddingInventoryDrift when the item
// left the project or its content changed since the inventory snapshot.
func requireCanonicalHash(ctx context.Context, tx *sql.Tx, projectID, itemID, contentHash string) error {
	var canonicalHash string
	err := tx.QueryRowContext(ctx, "SELECT LOWER(content_hash) FROM context_items WHERE id=? AND project_id=?", itemID, projectID).Scan(&canonicalHash)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: embedding entry %s is absent from project inventory", ErrEmbeddingInventoryDrift, itemID)
	}
	if err != nil {
		return err
	}
	if canonicalHash != contentHash {
		return fmt.Errorf("%w: embedding entry %s content hash does not match canonical item", ErrEmbeddingInventoryDrift, itemID)
	}
	return nil
}

// refreshEntryCount updates row_count and enforces rows+exclusions <= expected.
func refreshEntryCount(ctx context.Context, tx *sql.Tx, generation Generation) error {
	var rows, exclusions int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_embeddings WHERE generation_id=?", generation.GenerationID).Scan(&rows); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_embedding_exclusions WHERE generation_id=?", generation.GenerationID).Scan(&exclusions); err != nil {
		return err
	}
	if rows+exclusions > generation.ExpectedCount {
		return fmt.Errorf("embedding generation %s has %d entries, expected at most %d", generation.GenerationID, rows+exclusions, generation.ExpectedCount)
	}
	_, err := tx.ExecContext(ctx, "UPDATE context_embedding_generations SET row_count=? WHERE generation_id=? AND state='building'", rows, generation.GenerationID)
	return err
}

// AppendGenerationExclusions records items the generation leaves without a
// vector. They still count toward the generation's expected entries.
func (r *SQLiteRepository) AppendGenerationExclusions(ctx context.Context, generationID string, exclusions []EmbeddingExclusion) error {
	if len(exclusions) == 0 {
		return nil
	}
	if len(exclusions) > maxEmbeddingRowsPerBatch {
		return fmt.Errorf("embedding exclusion batch has %d entries, limit is %d", len(exclusions), maxEmbeddingRowsPerBatch)
	}
	for _, exclusion := range exclusions {
		if strings.TrimSpace(exclusion.ItemID) == "" || !validEmbeddingHash(exclusion.ContentHash) ||
			(exclusion.Reason != ExclusionOversize && exclusion.Reason != ExclusionEmpty) {
			return fmt.Errorf("invalid embedding exclusion for %q", exclusion.ItemID)
		}
	}
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		generation, err := buildingGeneration(ctx, tx, generationID)
		if err != nil {
			return err
		}
		for _, exclusion := range exclusions {
			if err = requireCanonicalHash(ctx, tx, generation.ProjectID, exclusion.ItemID, exclusion.ContentHash); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO context_embedding_exclusions(generation_id,item_id,content_hash,reason)
				VALUES(?,?,?,?) ON CONFLICT(generation_id,item_id) DO UPDATE SET content_hash=excluded.content_hash,reason=excluded.reason`,
				generationID, exclusion.ItemID, exclusion.ContentHash, exclusion.Reason); err != nil {
				return fmt.Errorf("append embedding exclusion %s: %w", exclusion.ItemID, err)
			}
		}
		if err = refreshEntryCount(ctx, tx, generation); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// CopyGenerationEntries carries unchanged vectors and exclusions forward from
// an earlier generation of the same project, encoder and inventory policy.
// Every item must still match its canonical hash and exist in the source.
func (r *SQLiteRepository) CopyGenerationEntries(ctx context.Context, fromGenerationID, toGenerationID string, items []EmbeddingInventoryItem) error {
	for start := 0; start < len(items); start += maxEmbeddingRowsPerBatch {
		chunk := items[start:min(start+maxEmbeddingRowsPerBatch, len(items))]
		err := r.withBusyRetry(ctx, func() error {
			tx, err := r.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			target, err := buildingGeneration(ctx, tx, toGenerationID)
			if err != nil {
				return err
			}
			source, err := loadGeneration(ctx, tx, fromGenerationID)
			if err != nil {
				return generationLookupError(err)
			}
			if source.ProjectID != target.ProjectID || source.Model != target.Model || source.InventoryPolicy != target.InventoryPolicy {
				return errors.New("embedding entries can only be copied between generations of one project, encoder and inventory policy")
			}
			now := time.Now().UTC().UnixMilli()
			for _, item := range chunk {
				if err = requireCanonicalHash(ctx, tx, target.ProjectID, item.ItemID, item.ContentHash); err != nil {
					return err
				}
				result, execErr := tx.ExecContext(ctx, `INSERT INTO context_embeddings(generation_id,item_id,content_hash,dimensions,vector,updated_at)
					SELECT ?,item_id,content_hash,dimensions,vector,? FROM context_embeddings WHERE generation_id=? AND item_id=? AND content_hash=?`,
					toGenerationID, now, fromGenerationID, item.ItemID, item.ContentHash)
				if execErr != nil {
					return fmt.Errorf("copy embedding row %s: %w", item.ItemID, execErr)
				}
				if copied, _ := result.RowsAffected(); copied == 1 {
					continue
				}
				result, execErr = tx.ExecContext(ctx, `INSERT INTO context_embedding_exclusions(generation_id,item_id,content_hash,reason)
					SELECT ?,item_id,content_hash,reason FROM context_embedding_exclusions WHERE generation_id=? AND item_id=? AND content_hash=?`,
					toGenerationID, fromGenerationID, item.ItemID, item.ContentHash)
				if execErr != nil {
					return fmt.Errorf("copy embedding exclusion %s: %w", item.ItemID, execErr)
				}
				if copied, _ := result.RowsAffected(); copied != 1 {
					return fmt.Errorf("embedding generation %s has no entry for %s", fromGenerationID, item.ItemID)
				}
			}
			if err = refreshEntryCount(ctx, tx, target); err != nil {
				return err
			}
			return tx.Commit()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// LoadGenerationExclusions returns the generation's exclusions in item ID
// order.
func (r *SQLiteRepository) LoadGenerationExclusions(ctx context.Context, generationID string) ([]EmbeddingExclusion, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT item_id,content_hash,reason FROM context_embedding_exclusions
		WHERE generation_id=? ORDER BY item_id COLLATE BINARY`, generationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []EmbeddingExclusion
	for rows.Next() {
		var exclusion EmbeddingExclusion
		if err = rows.Scan(&exclusion.ItemID, &exclusion.ContentHash, &exclusion.Reason); err != nil {
			return nil, err
		}
		result = append(result, exclusion)
	}
	return result, rows.Err()
}

// LoadGenerationEntryHashes returns item ID → content hash for the vector
// rows and the exclusions without decoding vectors.
func (r *SQLiteRepository) LoadGenerationEntryHashes(ctx context.Context, generationID string) (map[string]string, map[string]string, error) {
	load := func(table string) (map[string]string, error) {
		rows, err := r.db.QueryContext(ctx, "SELECT item_id,content_hash FROM "+table+" WHERE generation_id=?", generationID)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		result := map[string]string{}
		for rows.Next() {
			var id, hash string
			if err = rows.Scan(&id, &hash); err != nil {
				return nil, err
			}
			result[id] = hash
		}
		return result, rows.Err()
	}
	vectors, err := load("context_embeddings")
	if err != nil {
		return nil, nil, err
	}
	exclusions, err := load("context_embedding_exclusions")
	if err != nil {
		return nil, nil, err
	}
	return vectors, exclusions, nil
}

// ListGenerations returns the project's generations, newest first.
func (r *SQLiteRepository) ListGenerations(ctx context.Context, projectID string) ([]Generation, error) {
	rows, err := r.db.QueryContext(ctx, generationSelect+"WHERE project_id=? ORDER BY created_at DESC, generation_id", projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []Generation
	for rows.Next() {
		generation, scanErr := scanGeneration(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, generation)
	}
	return result, rows.Err()
}

// validateStoredGenerationEntries decodes every vector (rejecting bad
// dimensions or non-finite values) and digests the combined row and
// exclusion entries in item ID order.
func validateStoredGenerationEntries(ctx context.Context, query sqlQueryer, generation Generation) (string, int, int, error) {
	rows, err := query.QueryContext(ctx, `SELECT item_id,content_hash,dimensions,vector
		FROM context_embeddings WHERE generation_id=? ORDER BY item_id COLLATE BINARY`, generation.GenerationID)
	if err != nil {
		return "", 0, 0, err
	}
	entries := make([]EmbeddingInventoryItem, 0, generation.ExpectedCount)
	for rows.Next() {
		row, scanErr := scanEmbeddingRow(rows)
		if scanErr != nil {
			_ = rows.Close()
			return "", 0, 0, scanErr
		}
		if row.Dimensions != generation.Model.Dimensions {
			_ = rows.Close()
			return "", 0, 0, fmt.Errorf("embedding row %s dimension mismatch", row.ItemID)
		}
		entries = append(entries, EmbeddingInventoryItem{ItemID: row.ItemID, ContentHash: row.ContentHash})
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return "", 0, 0, err
	}
	_ = rows.Close()
	rowCount := len(entries)
	exclusions, err := query.QueryContext(ctx, `SELECT item_id,content_hash FROM context_embedding_exclusions
		WHERE generation_id=? ORDER BY item_id COLLATE BINARY`, generation.GenerationID)
	if err != nil {
		return "", 0, 0, err
	}
	defer func() { _ = exclusions.Close() }()
	for exclusions.Next() {
		var entry EmbeddingInventoryItem
		if err = exclusions.Scan(&entry.ItemID, &entry.ContentHash); err != nil {
			return "", 0, 0, err
		}
		entries = append(entries, entry)
	}
	if err = exclusions.Err(); err != nil {
		return "", 0, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ItemID < entries[j].ItemID })
	for i := 1; i < len(entries); i++ {
		if entries[i].ItemID == entries[i-1].ItemID {
			return "", 0, 0, fmt.Errorf("embedding item %s has both a vector and an exclusion", entries[i].ItemID)
		}
	}
	return embeddingInventoryDigest(entries), rowCount, len(entries) - rowCount, nil
}
