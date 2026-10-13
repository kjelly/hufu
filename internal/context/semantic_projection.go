package context

import (
	"context"
	"crypto/rand"
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

// Exclusion reasons for items a generation deliberately leaves without a
// vector. Coverage treats an excluded item with a matching hash as covered.
const (
	ExclusionOversize = "oversize"
	ExclusionEmpty    = "empty"
)

// GenerationSpec describes a generation before rows are written.
// ExpectedCount counts vector rows plus exclusions, and ContentDigest is the
// digest of their (item ID, content hash) pairs in item ID order.
type GenerationSpec struct {
	GenerationID    string
	ProjectID       string
	Model           embedding.ModelIdentity
	InventoryPolicy string
	SourceRevision  int64
	ExpectedCount   int
	ContentDigest   string
}

type Generation struct {
	GenerationID    string
	ProjectID       string
	Model           embedding.ModelIdentity
	InventoryPolicy string
	SourceRevision  int64
	State           GenerationState
	ExpectedCount   int
	// RowCount counts vector rows only; exclusions make up the rest of
	// ExpectedCount.
	RowCount      int
	ContentDigest string
	CreatedAt     time.Time
	ActivatedAt   *time.Time
}

type EmbeddingRow struct {
	ItemID      string
	ContentHash string
	Dimensions  int
	Vector      []float32
}

type EmbeddingExclusion struct {
	ItemID      string
	ContentHash string
	Reason      string
}

type GenerationCleanupPolicy struct {
	Now              time.Time
	InactiveTTL      time.Duration
	RetainSuperseded int
}

type SemanticProjectionRepository interface {
	BeginGeneration(context.Context, GenerationSpec) (Generation, error)
	AppendGenerationRows(context.Context, string, []EmbeddingRow) error
	AppendGenerationExclusions(context.Context, string, []EmbeddingExclusion) error
	CopyGenerationEntries(context.Context, string, string, []EmbeddingInventoryItem) error
	ActiveGeneration(context.Context, string, embedding.ModelIdentity, string) (Generation, error)
	LoadActiveGeneration(context.Context, string) ([]EmbeddingRow, error)
	LoadGenerationExclusions(context.Context, string) ([]EmbeddingExclusion, error)
	LoadGenerationEntryHashes(context.Context, string) (map[string]string, map[string]string, error)
	ActivateGeneration(context.Context, string, int64, string) (Generation, error)
	MarkGenerationFailed(context.Context, string) error
	CleanupInactiveGenerations(context.Context, GenerationCleanupPolicy) (int64, error)
	ListGenerations(context.Context, string) ([]Generation, error)
}

func NewEmbeddingGenerationID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create embedding generation ID: %w", err)
	}
	return "sem-" + hex.EncodeToString(random), nil
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
			(identity_scheme!=? OR model_hash!=? OR dimensions!=? OR table_hash!=? OR tokenizer_hash!=?)`,
			spec.ProjectID, spec.Model.ID, spec.Model.Revision, spec.Model.Scheme, spec.Model.ManifestSHA256,
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
			generation_id,project_id,identity_scheme,model_id,model_revision,model_hash,dimensions,table_hash,tokenizer_hash,
			inventory_policy,source_revision,state,expected_count,row_count,content_digest,created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,'building',?,0,?,?)`,
			spec.GenerationID, spec.ProjectID, spec.Model.Scheme, spec.Model.ID, spec.Model.Revision,
			spec.Model.ManifestSHA256, spec.Model.Dimensions, spec.Model.TableSHA256, spec.Model.TokenizerSHA256,
			spec.InventoryPolicy, spec.SourceRevision, spec.ExpectedCount, spec.ContentDigest, now.UnixMilli(),
		)
		if insertErr != nil {
			return fmt.Errorf("insert embedding generation: %w", insertErr)
		}
		generation = Generation{
			GenerationID: spec.GenerationID, ProjectID: spec.ProjectID, Model: spec.Model,
			InventoryPolicy: spec.InventoryPolicy, SourceRevision: spec.SourceRevision, State: GenerationBuilding,
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

		generation, err := buildingGeneration(ctx, tx, generationID)
		if err != nil {
			return err
		}
		now := time.Now().UTC().UnixMilli()
		for _, row := range rows {
			if row.Dimensions != generation.Model.Dimensions {
				return fmt.Errorf("embedding row %s has %d dimensions, want %d", row.ItemID, row.Dimensions, generation.Model.Dimensions)
			}
			if err = requireCanonicalHash(ctx, tx, generation.ProjectID, row.ItemID, row.ContentHash); err != nil {
				return err
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
		if err = refreshEntryCount(ctx, tx, generation); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (r *SQLiteRepository) ActiveGeneration(ctx context.Context, projectID string, model embedding.ModelIdentity, inventoryPolicy string) (Generation, error) {
	model.Scheme = model.EffectiveScheme()
	if err := validateModelIdentity(model); err != nil {
		return Generation{}, err
	}
	if !ValidInventoryPolicy(inventoryPolicy) {
		return Generation{}, fmt.Errorf("unknown embedding inventory policy %q", inventoryPolicy)
	}
	generation, err := scanGeneration(r.db.QueryRowContext(ctx, generationSelect+`
		WHERE project_id=? AND identity_scheme=? AND model_id=? AND model_revision=? AND model_hash=?
		AND dimensions=? AND table_hash=? AND tokenizer_hash=? AND inventory_policy=? AND state='active'`,
		projectID, model.Scheme, model.ID, model.Revision, model.ManifestSHA256, model.Dimensions,
		model.TableSHA256, model.TokenizerSHA256, inventoryPolicy,
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
	if len(result) != generation.RowCount {
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

// ActivateGeneration activates a complete building generation. Freshness is
// checked per item at query time, so activation does not require the global
// canonical revision to be unchanged; it only refuses to replace an active
// generation built from a newer inventory snapshot (the candidate is then
// superseded and the newer winner returned).
func (r *SQLiteRepository) ActivateGeneration(
	ctx context.Context,
	generationID string,
	expectedSourceRevision int64,
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

		activated, err = activateGeneration(ctx, conn, generationID, expectedSourceRevision, expectedContentDigest)
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

		rows, queryErr := tx.QueryContext(ctx, `SELECT generation_id,project_id,model_id,model_revision,inventory_policy,state,created_at
			FROM context_embedding_generations WHERE state!='active'
			ORDER BY project_id,model_id,model_revision,inventory_policy,created_at DESC,generation_id`)
		if queryErr != nil {
			return queryErr
		}
		type inactiveGeneration struct {
			id, projectID, modelID, revision, policy string
			state                                    GenerationState
			createdAt                                int64
		}
		var inactive []inactiveGeneration
		for rows.Next() {
			var item inactiveGeneration
			if queryErr = rows.Scan(&item.id, &item.projectID, &item.modelID, &item.revision, &item.policy, &item.state, &item.createdAt); queryErr != nil {
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
				key := item.projectID + "\x00" + item.modelID + "\x00" + item.revision + "\x00" + item.policy
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
	expectedSourceRevision int64,
	expectedContentDigest string,
) (Generation, error) {
	candidate, err := loadGeneration(ctx, conn, generationID)
	if err != nil {
		return Generation{}, generationLookupError(err)
	}
	if candidate.SourceRevision != expectedSourceRevision || candidate.ContentDigest != expectedContentDigest {
		return Generation{}, errors.New("embedding activation expectation does not match generation")
	}
	if candidate.State == GenerationActive {
		return candidate, nil
	}
	if candidate.State != GenerationBuilding {
		return Generation{}, fmt.Errorf("embedding generation %s is %s, not building", generationID, candidate.State)
	}

	entryDigest, rowCount, exclusionCount, err := validateStoredGenerationEntries(ctx, conn, candidate)
	if err != nil {
		return Generation{}, err
	}
	if rowCount != candidate.RowCount || rowCount+exclusionCount != candidate.ExpectedCount || entryDigest != candidate.ContentDigest {
		return Generation{}, fmt.Errorf("embedding generation %s is incomplete or inconsistent", generationID)
	}

	supersede := func(id, state string) error {
		_, execErr := conn.ExecContext(ctx, "UPDATE context_embedding_generations SET state='superseded' WHERE generation_id=? AND state=?", id, state)
		return execErr
	}
	winner, err := activeGenerationForKey(ctx, conn, candidate.ProjectID, candidate.Model.ID, candidate.Model.Revision, candidate.InventoryPolicy)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Generation{}, err
	}
	if err == nil {
		// An equivalent or newer active generation wins; the candidate is
		// obsolete and the caller receives the winner.
		if generationsEquivalent(winner, candidate) || winner.SourceRevision > candidate.SourceRevision {
			if err = supersede(candidate.GenerationID, string(GenerationBuilding)); err != nil {
				return Generation{}, err
			}
			return winner, nil
		}
		if err = supersede(winner.GenerationID, string(GenerationActive)); err != nil {
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
	spec.Model.Scheme = spec.Model.EffectiveScheme()
	if err := validateModelIdentity(spec.Model); err != nil {
		return err
	}
	if spec.InventoryPolicy == "" {
		spec.InventoryPolicy = InventoryPolicyProjectAll
	}
	if !ValidInventoryPolicy(spec.InventoryPolicy) {
		return fmt.Errorf("unknown embedding inventory policy %q", spec.InventoryPolicy)
	}
	if spec.SourceRevision < 0 || spec.ExpectedCount < 0 || !validEmbeddingHash(spec.ContentDigest) {
		return errors.New("invalid embedding generation inventory metadata")
	}
	return nil
}

// validateModelIdentity enforces what each scheme's hash fields attest. An
// Ollama identity carries no table or tokenizer hash; a static identity
// must carry all three.
func validateModelIdentity(model embedding.ModelIdentity) error {
	if strings.TrimSpace(model.ID) == "" || strings.TrimSpace(model.Revision) == "" ||
		model.Dimensions <= 0 || model.Dimensions > maxStoredEmbeddingDimensions {
		return errors.New("invalid embedding model identity")
	}
	switch model.EffectiveScheme() {
	case embedding.IdentitySchemeStaticTable:
		if !validEmbeddingHash(model.ManifestSHA256) || !validEmbeddingHash(model.TableSHA256) || !validEmbeddingHash(model.TokenizerSHA256) {
			return errors.New("invalid embedding model identity hashes")
		}
	case embedding.IdentitySchemeOllamaEncoder:
		if !strings.HasPrefix(model.ID, "ollama/") || !validEmbeddingHash(model.Revision) || !validEmbeddingHash(model.ManifestSHA256) ||
			model.TableSHA256 != "" || model.TokenizerSHA256 != "" {
			return errors.New("invalid ollama encoder identity")
		}
	default:
		return fmt.Errorf("unknown embedding identity scheme %q", model.Scheme)
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
	if len(value) != 64 || strings.ToLower(value) != value {
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

const generationSelect = `SELECT generation_id,project_id,identity_scheme,model_id,model_revision,model_hash,
	dimensions,table_hash,tokenizer_hash,inventory_policy,source_revision,state,expected_count,row_count,
	content_digest,created_at,activated_at FROM context_embedding_generations `

func scanGeneration(row interface{ Scan(...any) error }) (Generation, error) {
	var result Generation
	var createdAt int64
	var activatedAt sql.NullInt64
	err := row.Scan(
		&result.GenerationID, &result.ProjectID, &result.Model.Scheme, &result.Model.ID, &result.Model.Revision,
		&result.Model.ManifestSHA256, &result.Model.Dimensions, &result.Model.TableSHA256,
		&result.Model.TokenizerSHA256, &result.InventoryPolicy, &result.SourceRevision, &result.State,
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

func activeGenerationForKey(ctx context.Context, query sqlQueryer, projectID, modelID, revision, inventoryPolicy string) (Generation, error) {
	return scanGeneration(query.QueryRowContext(ctx, generationSelect+`
		WHERE project_id=? AND model_id=? AND model_revision=? AND inventory_policy=? AND state='active'`,
		projectID, modelID, revision, inventoryPolicy,
	))
}

func generationLookupError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEmbeddingGenerationNotFound
	}
	return err
}

func generationsEquivalent(left, right Generation) bool {
	return left.ProjectID == right.ProjectID && left.Model == right.Model && left.InventoryPolicy == right.InventoryPolicy &&
		left.SourceRevision == right.SourceRevision && left.ExpectedCount == right.ExpectedCount &&
		left.RowCount == right.RowCount && left.ContentDigest == right.ContentDigest
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var _ SemanticProjectionRepository = (*SQLiteRepository)(nil)
