package context

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/embedding"
)

const (
	maxEmbeddingRowsPerBatch     = 512
	maxStoredEmbeddingDimensions = 2048
)

var (
	ErrEmbeddingGenerationNotFound = errors.New("embedding generation not found")
	ErrNoActiveEmbeddingGeneration = errors.New("no active embedding generation")
	ErrEmbeddingInventoryDrift     = errors.New("canonical embedding inventory drifted")
	ErrEmbeddingIdentityConflict   = errors.New("embedding model identity conflicts with an existing generation")
)

type GenerationState string

const (
	GenerationBuilding   GenerationState = "building"
	GenerationActive     GenerationState = "active"
	GenerationSuperseded GenerationState = "superseded"
	GenerationFailed     GenerationState = "failed"
)

type EmbeddingInventoryItem struct {
	ItemID      string
	Content     string
	ContentHash string
}

type EmbeddingInventory struct {
	ProjectID string
	Revision  int64
	Digest    string
	Items     []EmbeddingInventoryItem
}

type GenerationSpec struct {
	GenerationID   string
	ProjectID      string
	Model          embedding.ModelIdentity
	SourceRevision int64
	ExpectedCount  int
	ContentDigest  string
}

type Generation struct {
	GenerationID   string
	ProjectID      string
	Model          embedding.ModelIdentity
	SourceRevision int64
	State          GenerationState
	ExpectedCount  int
	RowCount       int
	ContentDigest  string
	CreatedAt      time.Time
	ActivatedAt    *time.Time
}

type EmbeddingRow struct {
	ItemID      string
	ContentHash string
	Dimensions  int
	Vector      []float32
}

type GenerationCleanupPolicy struct {
	Now              time.Time
	InactiveTTL      time.Duration
	RetainSuperseded int
}

type CanonicalEmbeddingInventory interface {
	LoadEmbeddingInventory(context.Context, string) (EmbeddingInventory, error)
}

type SemanticProjectionRepository interface {
	BeginGeneration(context.Context, GenerationSpec) (Generation, error)
	AppendGenerationRows(context.Context, string, []EmbeddingRow) error
	ActiveGeneration(context.Context, string, embedding.ModelIdentity) (Generation, error)
	LoadActiveGeneration(context.Context, string) ([]EmbeddingRow, error)
	ActivateGeneration(context.Context, string, int64, string) (Generation, error)
	MarkGenerationFailed(context.Context, string) error
	CleanupInactiveGenerations(context.Context, GenerationCleanupPolicy) (int64, error)
}

func NewEmbeddingGenerationID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create embedding generation ID: %w", err)
	}
	return "sem-" + hex.EncodeToString(random), nil
}

func (r *SQLiteRepository) LoadEmbeddingInventory(ctx context.Context, projectID string) (EmbeddingInventory, error) {
	if strings.TrimSpace(projectID) == "" {
		return EmbeddingInventory{}, errors.New("embedding inventory project ID is required")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EmbeddingInventory{}, fmt.Errorf("begin embedding inventory snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	inventory, err := loadEmbeddingInventory(ctx, tx, projectID)
	if err != nil {
		return EmbeddingInventory{}, err
	}
	if err = tx.Commit(); err != nil {
		return EmbeddingInventory{}, fmt.Errorf("commit embedding inventory snapshot: %w", err)
	}
	return inventory, nil
}

func (r *SQLiteRepository) BeginGeneration(ctx context.Context, spec GenerationSpec) (Generation, error) {
	if err := validateGenerationSpec(&spec); err != nil {
		return Generation{}, err
	}
	var generation Generation
	err := r.withBusyRetry(ctx, func() error {
		tx, beginErr := r.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer func() { _ = tx.Rollback() }()

		var conflicts int
		loadErr := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_embedding_generations
			WHERE project_id=? AND model_id=? AND model_revision=? AND
			(model_hash!=? OR dimensions!=? OR table_hash!=? OR tokenizer_hash!=?)`,
			spec.ProjectID, spec.Model.ID, spec.Model.Revision, spec.Model.ManifestSHA256,
			spec.Model.Dimensions, spec.Model.TableSHA256, spec.Model.TokenizerSHA256,
		).Scan(&conflicts)
		if loadErr != nil {
			return loadErr
		}
		if conflicts != 0 {
			return fmt.Errorf("%w: %s@%s", ErrEmbeddingIdentityConflict, spec.Model.ID, spec.Model.Revision)
		}

		now := time.Now().UTC()
		_, insertErr := tx.ExecContext(ctx, `INSERT INTO context_embedding_generations(
			generation_id,project_id,model_id,model_revision,model_hash,dimensions,table_hash,tokenizer_hash,
			source_revision,state,expected_count,row_count,content_digest,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,'building',?,0,?,?)`,
			spec.GenerationID, spec.ProjectID, spec.Model.ID, spec.Model.Revision,
			spec.Model.ManifestSHA256, spec.Model.Dimensions, spec.Model.TableSHA256, spec.Model.TokenizerSHA256,
			spec.SourceRevision, spec.ExpectedCount, spec.ContentDigest, now.UnixMilli(),
		)
		if insertErr != nil {
			return fmt.Errorf("insert embedding generation: %w", insertErr)
		}
		generation = Generation{
			GenerationID: spec.GenerationID, ProjectID: spec.ProjectID, Model: spec.Model,
			SourceRevision: spec.SourceRevision, State: GenerationBuilding,
			ExpectedCount: spec.ExpectedCount, ContentDigest: spec.ContentDigest, CreatedAt: now,
		}
		return tx.Commit()
	})
	if err != nil {
		return Generation{}, err
	}
	return generation, nil
}

func (r *SQLiteRepository) AppendGenerationRows(ctx context.Context, generationID string, rows []EmbeddingRow) error {
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > maxEmbeddingRowsPerBatch {
		return fmt.Errorf("embedding row batch has %d rows, limit is %d", len(rows), maxEmbeddingRowsPerBatch)
	}
	if err := validateEmbeddingRows(rows); err != nil {
		return err
	}
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		generation, err := loadGeneration(ctx, tx, generationID)
		if err != nil {
			return generationLookupError(err)
		}
		if generation.State != GenerationBuilding {
			return fmt.Errorf("embedding generation %s is %s, not building", generationID, generation.State)
		}

		now := time.Now().UTC().UnixMilli()
		for _, row := range rows {
			if row.Dimensions != generation.Model.Dimensions {
				return fmt.Errorf("embedding row %s has %d dimensions, want %d", row.ItemID, row.Dimensions, generation.Model.Dimensions)
			}
			var canonicalHash string
			if err = tx.QueryRowContext(ctx,
				"SELECT content_hash FROM context_items WHERE id=? AND project_id=?",
				row.ItemID, generation.ProjectID,
			).Scan(&canonicalHash); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("embedding row %s is absent from project inventory", row.ItemID)
				}
				return err
			}
			if canonicalHash != row.ContentHash {
				return fmt.Errorf("embedding row %s content hash does not match canonical item", row.ItemID)
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO context_embeddings(
				generation_id,item_id,content_hash,dimensions,vector,updated_at
			) VALUES(?,?,?,?,?,?) ON CONFLICT(generation_id,item_id) DO UPDATE SET
				content_hash=excluded.content_hash,dimensions=excluded.dimensions,vector=excluded.vector,updated_at=excluded.updated_at`,
				generationID, row.ItemID, row.ContentHash, row.Dimensions, encodeEmbeddingVector(row.Vector), now,
			); err != nil {
				return fmt.Errorf("append embedding row %s: %w", row.ItemID, err)
			}
		}
		var rowCount int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_embeddings WHERE generation_id=?", generationID).Scan(&rowCount); err != nil {
			return err
		}
		if rowCount > generation.ExpectedCount {
			return fmt.Errorf("embedding generation %s has %d rows, expected at most %d", generationID, rowCount, generation.ExpectedCount)
		}
		if _, err = tx.ExecContext(ctx, "UPDATE context_embedding_generations SET row_count=? WHERE generation_id=? AND state='building'", rowCount, generationID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (r *SQLiteRepository) ActiveGeneration(ctx context.Context, projectID string, model embedding.ModelIdentity) (Generation, error) {
	if err := validateModelIdentity(model); err != nil {
		return Generation{}, err
	}
	generation, err := scanGeneration(r.db.QueryRowContext(ctx, generationSelect+`
		WHERE project_id=? AND model_id=? AND model_revision=? AND model_hash=?
		AND dimensions=? AND table_hash=? AND tokenizer_hash=? AND state='active'`,
		projectID, model.ID, model.Revision, model.ManifestSHA256, model.Dimensions, model.TableSHA256, model.TokenizerSHA256,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, ErrNoActiveEmbeddingGeneration
	}
	if err != nil {
		return Generation{}, fmt.Errorf("load active embedding generation: %w", err)
	}
	return generation, nil
}

func (r *SQLiteRepository) LoadActiveGeneration(ctx context.Context, generationID string) ([]EmbeddingRow, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	generation, err := loadGeneration(ctx, tx, generationID)
	if err != nil {
		return nil, generationLookupError(err)
	}
	if generation.State != GenerationActive {
		return nil, fmt.Errorf("embedding generation %s is %s, not active", generationID, generation.State)
	}
	rows, err := tx.QueryContext(ctx, `SELECT item_id,content_hash,dimensions,vector
		FROM context_embeddings WHERE generation_id=? ORDER BY item_id COLLATE BINARY`, generationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]EmbeddingRow, 0, generation.RowCount)
	for rows.Next() {
		row, scanErr := scanEmbeddingRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if row.Dimensions != generation.Model.Dimensions {
			return nil, fmt.Errorf("embedding row %s dimension mismatch", row.ItemID)
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != generation.RowCount || len(result) != generation.ExpectedCount {
		return nil, fmt.Errorf("active embedding generation %s row count mismatch", generationID)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *SQLiteRepository) ActivateGeneration(
	ctx context.Context,
	generationID string,
	expectedCanonicalRevision int64,
	expectedContentDigest string,
) (Generation, error) {
	var activated Generation
	err := r.withBusyRetry(ctx, func() error {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			}
		}()

		activated, err = activateGeneration(ctx, conn, generationID, expectedCanonicalRevision, expectedContentDigest)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if err != nil {
		return Generation{}, err
	}
	return activated, nil
}

func (r *SQLiteRepository) MarkGenerationFailed(ctx context.Context, generationID string) error {
	return r.withBusyRetry(ctx, func() error {
		result, err := r.db.ExecContext(ctx,
			"UPDATE context_embedding_generations SET state='failed' WHERE generation_id=? AND state='building'",
			generationID,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 1 {
			return nil
		}
		generation, err := loadGeneration(ctx, r.db, generationID)
		if err != nil {
			return generationLookupError(err)
		}
		if generation.State == GenerationFailed {
			return nil
		}
		return fmt.Errorf("refusing to fail embedding generation %s in state %s", generationID, generation.State)
	})
}

func (r *SQLiteRepository) CleanupInactiveGenerations(ctx context.Context, policy GenerationCleanupPolicy) (int64, error) {
	policy, err := normalizeCleanupPolicy(policy)
	if err != nil {
		return 0, err
	}
	var deleted int64
	err = r.withBusyRetry(ctx, func() error {
		var attemptDeleted int64
		tx, beginErr := r.db.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer func() { _ = tx.Rollback() }()

		rows, queryErr := tx.QueryContext(ctx, `SELECT generation_id,project_id,model_id,model_revision,state,created_at
			FROM context_embedding_generations WHERE state!='active'
			ORDER BY project_id,model_id,model_revision,created_at DESC,generation_id`)
		if queryErr != nil {
			return queryErr
		}
		type inactiveGeneration struct {
			id, projectID, modelID, revision string
			state                            GenerationState
			createdAt                        int64
		}
		var inactive []inactiveGeneration
		for rows.Next() {
			var item inactiveGeneration
			if queryErr = rows.Scan(&item.id, &item.projectID, &item.modelID, &item.revision, &item.state, &item.createdAt); queryErr != nil {
				_ = rows.Close()
				return queryErr
			}
			inactive = append(inactive, item)
		}
		if queryErr = rows.Err(); queryErr != nil {
			_ = rows.Close()
			return queryErr
		}
		_ = rows.Close()

		cutoff := policy.Now.Add(-policy.InactiveTTL).UnixMilli()
		supersededCounts := make(map[string]int)
		for _, item := range inactive {
			remove := false
			switch item.state {
			case GenerationBuilding, GenerationFailed:
				remove = item.createdAt < cutoff
			case GenerationSuperseded:
				key := item.projectID + "\x00" + item.modelID + "\x00" + item.revision
				supersededCounts[key]++
				remove = supersededCounts[key] > policy.RetainSuperseded
			}
			if !remove {
				continue
			}
			result, deleteErr := tx.ExecContext(ctx,
				"DELETE FROM context_embedding_generations WHERE generation_id=? AND state!='active'", item.id,
			)
			if deleteErr != nil {
				return deleteErr
			}
			count, countErr := result.RowsAffected()
			if countErr != nil {
				return countErr
			}
			attemptDeleted += count
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		deleted = attemptDeleted
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func activateGeneration(
	ctx context.Context,
	conn *sql.Conn,
	generationID string,
	expectedCanonicalRevision int64,
	expectedContentDigest string,
) (Generation, error) {
	candidate, err := loadGeneration(ctx, conn, generationID)
	if err != nil {
		return Generation{}, generationLookupError(err)
	}
	if candidate.SourceRevision != expectedCanonicalRevision || candidate.ContentDigest != expectedContentDigest {
		return Generation{}, errors.New("embedding activation expectation does not match generation")
	}
	if candidate.State == GenerationActive {
		return candidate, nil
	}
	if candidate.State != GenerationBuilding {
		return Generation{}, fmt.Errorf("embedding generation %s is %s, not building", generationID, candidate.State)
	}

	rowDigest, rowCount, err := validateStoredGenerationRows(ctx, conn, candidate)
	if err != nil {
		return Generation{}, err
	}
	if rowCount != candidate.RowCount || rowCount != candidate.ExpectedCount || rowDigest != candidate.ContentDigest {
		return Generation{}, fmt.Errorf("embedding generation %s is incomplete or inconsistent", generationID)
	}
	inventory, err := loadEmbeddingInventory(ctx, conn, candidate.ProjectID)
	if err != nil {
		return Generation{}, err
	}
	if inventory.Revision != expectedCanonicalRevision || inventory.Digest != expectedContentDigest || len(inventory.Items) != candidate.ExpectedCount {
		return Generation{}, ErrEmbeddingInventoryDrift
	}

	winner, err := activeGenerationForKey(ctx, conn, candidate.ProjectID, candidate.Model.ID, candidate.Model.Revision)
	if err == nil && generationsEquivalent(winner, candidate) {
		if _, err = conn.ExecContext(ctx,
			"UPDATE context_embedding_generations SET state='superseded' WHERE generation_id=? AND state='building'",
			candidate.GenerationID,
		); err != nil {
			return Generation{}, err
		}
		return winner, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Generation{}, err
	}
	if err == nil {
		if _, err = conn.ExecContext(ctx,
			"UPDATE context_embedding_generations SET state='superseded' WHERE generation_id=? AND state='active'",
			winner.GenerationID,
		); err != nil {
			return Generation{}, err
		}
	}
	now := time.Now().UTC()
	result, err := conn.ExecContext(ctx, `UPDATE context_embedding_generations
		SET state='active',activated_at=? WHERE generation_id=? AND state='building'`, now.UnixMilli(), generationID)
	if err != nil {
		return Generation{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Generation{}, err
	}
	if changed != 1 {
		return Generation{}, fmt.Errorf("embedding generation %s was not activated", generationID)
	}
	candidate.State = GenerationActive
	candidate.ActivatedAt = &now
	return candidate, nil
}

func validateStoredGenerationRows(ctx context.Context, query sqlQueryer, generation Generation) (string, int, error) {
	rows, err := query.QueryContext(ctx, `SELECT item_id,content_hash,dimensions,vector
		FROM context_embeddings WHERE generation_id=? ORDER BY item_id COLLATE BINARY`, generation.GenerationID)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = rows.Close() }()
	inventory := make([]EmbeddingInventoryItem, 0, generation.RowCount)
	for rows.Next() {
		row, scanErr := scanEmbeddingRow(rows)
		if scanErr != nil {
			return "", 0, scanErr
		}
		if row.Dimensions != generation.Model.Dimensions {
			return "", 0, fmt.Errorf("embedding row %s dimension mismatch", row.ItemID)
		}
		inventory = append(inventory, EmbeddingInventoryItem{ItemID: row.ItemID, ContentHash: row.ContentHash})
	}
	if err = rows.Err(); err != nil {
		return "", 0, err
	}
	return embeddingInventoryDigest(inventory), len(inventory), nil
}

func loadEmbeddingInventory(ctx context.Context, query sqlQueryer, projectID string) (EmbeddingInventory, error) {
	inventory := EmbeddingInventory{ProjectID: projectID}
	if err := query.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM context_events").Scan(&inventory.Revision); err != nil {
		return EmbeddingInventory{}, fmt.Errorf("read canonical revision: %w", err)
	}
	rows, err := query.QueryContext(ctx, `SELECT id,content,LOWER(content_hash)
		FROM context_items WHERE project_id=? ORDER BY id COLLATE BINARY`, projectID)
	if err != nil {
		return EmbeddingInventory{}, fmt.Errorf("read embedding inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item EmbeddingInventoryItem
		if err = rows.Scan(&item.ItemID, &item.Content, &item.ContentHash); err != nil {
			return EmbeddingInventory{}, err
		}
		if !validEmbeddingHash(item.ContentHash) {
			return EmbeddingInventory{}, fmt.Errorf("context item %s has invalid content hash", item.ItemID)
		}
		inventory.Items = append(inventory.Items, item)
	}
	if err = rows.Err(); err != nil {
		return EmbeddingInventory{}, err
	}
	inventory.Digest = embeddingInventoryDigest(inventory.Items)
	return inventory, nil
}

func embeddingInventoryDigest(items []EmbeddingInventoryItem) string {
	hash := sha256.New()
	var encoded [binary.MaxVarintLen64]byte
	for _, item := range items {
		length := binary.PutUvarint(encoded[:], uint64(len(item.ItemID)))
		_, _ = hash.Write(encoded[:length])
		_, _ = hash.Write([]byte(item.ItemID))
		contentHash := strings.ToLower(item.ContentHash)
		length = binary.PutUvarint(encoded[:], uint64(len(contentHash)))
		_, _ = hash.Write(encoded[:length])
		_, _ = hash.Write([]byte(contentHash))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validateGenerationSpec(spec *GenerationSpec) error {
	if strings.TrimSpace(spec.ProjectID) == "" {
		return errors.New("embedding generation project ID is required")
	}
	if spec.GenerationID == "" {
		generated, err := NewEmbeddingGenerationID()
		if err != nil {
			return err
		}
		spec.GenerationID = generated
	}
	if !validGenerationID(spec.GenerationID) {
		return errors.New("invalid embedding generation ID")
	}
	if err := validateModelIdentity(spec.Model); err != nil {
		return err
	}
	if spec.SourceRevision < 0 || spec.ExpectedCount < 0 || !validEmbeddingHash(spec.ContentDigest) {
		return errors.New("invalid embedding generation inventory metadata")
	}
	return nil
}

func validateModelIdentity(model embedding.ModelIdentity) error {
	if strings.TrimSpace(model.ID) == "" || strings.TrimSpace(model.Revision) == "" ||
		model.Dimensions <= 0 || model.Dimensions > maxStoredEmbeddingDimensions {
		return errors.New("invalid embedding model identity")
	}
	if !validEmbeddingHash(model.ManifestSHA256) || !validEmbeddingHash(model.TableSHA256) || !validEmbeddingHash(model.TokenizerSHA256) {
		return errors.New("invalid embedding model identity hashes")
	}
	return nil
}

func validateEmbeddingRows(rows []EmbeddingRow) error {
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.ItemID) == "" || !validEmbeddingHash(row.ContentHash) {
			return errors.New("embedding row identity and content hash are required")
		}
		if _, exists := seen[row.ItemID]; exists {
			return fmt.Errorf("embedding row %s is duplicated in batch", row.ItemID)
		}
		seen[row.ItemID] = struct{}{}
		if row.Dimensions <= 0 || len(row.Vector) != row.Dimensions {
			return fmt.Errorf("embedding row %s has invalid dimensions", row.ItemID)
		}
		for _, value := range row.Vector {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("embedding row %s contains non-finite value", row.ItemID)
			}
		}
	}
	return nil
}

func normalizeCleanupPolicy(policy GenerationCleanupPolicy) (GenerationCleanupPolicy, error) {
	if policy.Now.IsZero() {
		policy.Now = time.Now().UTC()
	} else {
		policy.Now = policy.Now.UTC()
	}
	if policy.InactiveTTL == 0 {
		policy.InactiveTTL = 24 * time.Hour
	}
	if policy.RetainSuperseded == 0 {
		policy.RetainSuperseded = 2
	}
	if policy.InactiveTTL < 0 || policy.RetainSuperseded < 0 {
		return GenerationCleanupPolicy{}, errors.New("invalid embedding generation cleanup policy")
	}
	return policy, nil
}

func validGenerationID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func validEmbeddingHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func encodeEmbeddingVector(vector []float32) []byte {
	encoded := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(encoded[i*4:i*4+4], math.Float32bits(value))
	}
	return encoded
}

func decodeEmbeddingVector(encoded []byte, dimensions int) ([]float32, error) {
	if dimensions <= 0 || dimensions > maxStoredEmbeddingDimensions || len(encoded) != dimensions*4 {
		return nil, errors.New("invalid stored embedding vector length")
	}
	vector := make([]float32, dimensions)
	for i := range dimensions {
		value := math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4 : i*4+4]))
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("stored embedding vector contains non-finite value")
		}
		vector[i] = value
	}
	return vector, nil
}

func scanEmbeddingRow(row interface{ Scan(...any) error }) (EmbeddingRow, error) {
	var result EmbeddingRow
	var encoded []byte
	if err := row.Scan(&result.ItemID, &result.ContentHash, &result.Dimensions, &encoded); err != nil {
		return EmbeddingRow{}, err
	}
	if !validEmbeddingHash(result.ContentHash) {
		return EmbeddingRow{}, fmt.Errorf("embedding row %s has invalid content hash", result.ItemID)
	}
	vector, err := decodeEmbeddingVector(encoded, result.Dimensions)
	if err != nil {
		return EmbeddingRow{}, fmt.Errorf("decode embedding row %s: %w", result.ItemID, err)
	}
	result.Vector = vector
	return result, nil
}

const generationSelect = `SELECT generation_id,project_id,model_id,model_revision,model_hash,
	dimensions,table_hash,tokenizer_hash,source_revision,state,expected_count,row_count,
	content_digest,created_at,activated_at FROM context_embedding_generations `

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var result Generation
	var createdAt int64
	var activatedAt sql.NullInt64
	err := row.Scan(
		&result.GenerationID, &result.ProjectID, &result.Model.ID, &result.Model.Revision,
		&result.Model.ManifestSHA256, &result.Model.Dimensions, &result.Model.TableSHA256,
		&result.Model.TokenizerSHA256, &result.SourceRevision, &result.State,
		&result.ExpectedCount, &result.RowCount, &result.ContentDigest, &createdAt, &activatedAt,
	)
	if err != nil {
		return Generation{}, err
	}
	result.CreatedAt = time.UnixMilli(createdAt).UTC()
	if activatedAt.Valid {
		activated := time.UnixMilli(activatedAt.Int64).UTC()
		result.ActivatedAt = &activated
	}
	return result, nil
}

func loadGeneration(ctx context.Context, query sqlQueryer, generationID string) (Generation, error) {
	return scanGeneration(query.QueryRowContext(ctx, generationSelect+"WHERE generation_id=?", generationID))
}

func activeGenerationForKey(ctx context.Context, query sqlQueryer, projectID, modelID, revision string) (Generation, error) {
	return scanGeneration(query.QueryRowContext(ctx, generationSelect+`
		WHERE project_id=? AND model_id=? AND model_revision=? AND state='active'`,
		projectID, modelID, revision,
	))
}

func generationLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEmbeddingGenerationNotFound
	}
	return err
}

func generationsEquivalent(left, right Generation) bool {
	return left.ProjectID == right.ProjectID && left.Model == right.Model &&
		left.SourceRevision == right.SourceRevision && left.ExpectedCount == right.ExpectedCount &&
		left.RowCount == right.RowCount && left.ContentDigest == right.ContentDigest
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var _ CanonicalEmbeddingInventory = (*SQLiteRepository)(nil)
var _ SemanticProjectionRepository = (*SQLiteRepository)(nil)
