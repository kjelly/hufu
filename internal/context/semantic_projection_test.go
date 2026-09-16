package context

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/embedding"
)

func TestEmbeddingInventoryUsesOneCanonicalSnapshotAndStableDigest(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "project-1", "b", "a")

	inventory, err := repository.LoadEmbeddingInventory(t.Context(), "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Items) != 2 || inventory.Items[0].ItemID != "a" || inventory.Items[1].ItemID != "b" {
		t.Fatalf("inventory order = %#v", inventory.Items)
	}
	if inventory.Revision == 0 {
		t.Fatal("inventory revision was not captured")
	}

	hash := sha256.New()
	var length [binary.MaxVarintLen64]byte
	for _, item := range inventory.Items {
		n := binary.PutUvarint(length[:], uint64(len(item.ItemID)))
		_, _ = hash.Write(length[:n])
		_, _ = hash.Write([]byte(item.ItemID))
		n = binary.PutUvarint(length[:], uint64(len(item.ContentHash)))
		_, _ = hash.Write(length[:n])
		_, _ = hash.Write([]byte(item.ContentHash))
	}
	want := hex.EncodeToString(hash.Sum(nil))
	if inventory.Digest != want {
		t.Fatalf("inventory digest = %s, want %s", inventory.Digest, want)
	}
}

func TestEmbeddingGenerationLifecycleAndProjectionIsolation(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "project-1", "item-1", "item-2")
	inventory := loadProjectionInventory(t, repository, "project-1")
	revisionBefore := inventory.Revision
	model := projectionTestModel("model-a", "1")

	generation, err := repository.BeginGeneration(t.Context(), GenerationSpec{
		ProjectID: "project-1", Model: model, SourceRevision: inventory.Revision,
		ExpectedCount: len(inventory.Items), ContentDigest: inventory.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if generation.GenerationID == "" || generation.State != GenerationBuilding {
		t.Fatalf("unexpected building generation: %#v", generation)
	}
	if _, err = repository.ActiveGeneration(t.Context(), "project-1", model); !errors.Is(err, ErrNoActiveEmbeddingGeneration) {
		t.Fatalf("building generation became visible: %v", err)
	}
	if _, err = repository.LoadActiveGeneration(t.Context(), generation.GenerationID); err == nil {
		t.Fatal("building generation rows were readable")
	}

	if err = repository.AppendGenerationRows(t.Context(), generation.GenerationID, projectionRows(inventory, model.Dimensions)); err != nil {
		t.Fatal(err)
	}
	active, err := repository.ActivateGeneration(t.Context(), generation.GenerationID, inventory.Revision, inventory.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if active.GenerationID != generation.GenerationID || active.State != GenerationActive || active.ActivatedAt == nil {
		t.Fatalf("unexpected active generation: %#v", active)
	}
	loaded, err := repository.LoadActiveGeneration(t.Context(), active.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(inventory.Items) {
		t.Fatalf("loaded %d rows, want %d", len(loaded), len(inventory.Items))
	}
	revisionAfter, err := repository.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("projection writes advanced canonical revision: before=%d after=%d", revisionBefore, revisionAfter)
	}

	if _, err = repository.db.ExecContext(t.Context(), "DELETE FROM context_items WHERE id=?", inventory.Items[0].ItemID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err = repository.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM context_embeddings WHERE generation_id=?", active.GenerationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(inventory.Items)-1 {
		t.Fatalf("cascade left %d rows, want %d", rows, len(inventory.Items)-1)
	}
}

func TestEquivalentGenerationKeepsExistingActiveWinner(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "project-1", "item-1", "item-2")
	inventory := loadProjectionInventory(t, repository, "project-1")
	model := projectionTestModel("model-a", "1")

	first := beginProjectionGeneration(t, repository, "generation-first", inventory, model)
	appendProjectionRows(t, repository, first.GenerationID, inventory, model.Dimensions)
	first, err := repository.ActivateGeneration(t.Context(), first.GenerationID, inventory.Revision, inventory.Digest)
	if err != nil {
		t.Fatal(err)
	}

	second := beginProjectionGeneration(t, repository, "generation-second", inventory, model)
	if err = repository.AppendGenerationRows(t.Context(), second.GenerationID, projectionRows(inventory, model.Dimensions)[:1]); err != nil {
		t.Fatal(err)
	}
	visible, err := repository.ActiveGeneration(t.Context(), "project-1", model)
	if err != nil {
		t.Fatal(err)
	}
	if visible.GenerationID != first.GenerationID {
		t.Fatalf("partial rebuild replaced active generation: got %s want %s", visible.GenerationID, first.GenerationID)
	}
	if _, err = repository.LoadActiveGeneration(t.Context(), second.GenerationID); err == nil {
		t.Fatal("partially built generation was readable")
	}
	if err = repository.AppendGenerationRows(t.Context(), second.GenerationID, projectionRows(inventory, model.Dimensions)[1:]); err != nil {
		t.Fatal(err)
	}
	winner, err := repository.ActivateGeneration(t.Context(), second.GenerationID, inventory.Revision, inventory.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if winner.GenerationID != first.GenerationID {
		t.Fatalf("equivalent activation winner = %s, want %s", winner.GenerationID, first.GenerationID)
	}
	second, err = loadGeneration(t.Context(), repository.db, second.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != GenerationSuperseded {
		t.Fatalf("equivalent candidate state = %s, want superseded", second.State)
	}
}

func TestBeginGenerationRejectsImmutableIdentityConflict(t *testing.T) {
	repository := openProjectionTestRepository(t)
	inventory := loadProjectionInventory(t, repository, "project-1")
	model := projectionTestModel("model-a", "1")
	beginProjectionGeneration(t, repository, "generation-first", inventory, model)
	model.TableSHA256 = projectionHash("different table")
	_, err := repository.BeginGeneration(t.Context(), GenerationSpec{
		GenerationID: "generation-conflict", ProjectID: "project-1", Model: model,
		SourceRevision: inventory.Revision, ExpectedCount: 0, ContentDigest: inventory.Digest,
	})
	if !errors.Is(err, ErrEmbeddingIdentityConflict) {
		t.Fatalf("identity conflict error = %v", err)
	}
}

func TestActivateGenerationRejectsCanonicalDrift(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "project-1", "item-1")
	inventory := loadProjectionInventory(t, repository, "project-1")
	model := projectionTestModel("model-a", "1")
	generation := beginProjectionGeneration(t, repository, "generation-drift", inventory, model)
	appendProjectionRows(t, repository, generation.GenerationID, inventory, model.Dimensions)
	appendProjectionItems(t, repository, "project-1", "item-2")

	_, err := repository.ActivateGeneration(t.Context(), generation.GenerationID, inventory.Revision, inventory.Digest)
	if !errors.Is(err, ErrEmbeddingInventoryDrift) {
		t.Fatalf("activation drift error = %v", err)
	}
	generation, err = loadGeneration(t.Context(), repository.db, generation.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if generation.State != GenerationBuilding {
		t.Fatalf("drifted generation state = %s, want building", generation.State)
	}
}

func TestConcurrentRepositoryActivationChoosesOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	appendProjectionItems(t, first, "project-1", "item-1", "item-2")
	inventory := loadProjectionInventory(t, first, "project-1")
	model := projectionTestModel("model-a", "1")
	left := beginProjectionGeneration(t, first, "generation-left", inventory, model)
	right := beginProjectionGeneration(t, second, "generation-right", inventory, model)
	appendProjectionRows(t, first, left.GenerationID, inventory, model.Dimensions)
	appendProjectionRows(t, second, right.GenerationID, inventory, model.Dimensions)

	type activationResult struct {
		generation Generation
		err        error
	}
	results := make(chan activationResult, 2)
	var group sync.WaitGroup
	for repository, generationID := range map[*SQLiteRepository]string{
		first: left.GenerationID, second: right.GenerationID,
	} {
		group.Go(func() {
			generation, activateErr := repository.ActivateGeneration(t.Context(), generationID, inventory.Revision, inventory.Digest)
			results <- activationResult{generation: generation, err: activateErr}
		})
	}
	group.Wait()
	close(results)
	var winner string
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if winner == "" {
			winner = result.generation.GenerationID
		} else if result.generation.GenerationID != winner {
			t.Fatalf("activation returned different winners: %s and %s", winner, result.generation.GenerationID)
		}
	}
	var activeCount int
	if err = first.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM context_embedding_generations
		WHERE project_id='project-1' AND model_id='model-a' AND model_revision='1' AND state='active'`).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active generation count = %d, want 1", activeCount)
	}
}

func TestGenerationCleanupRetainsActiveAndNewestSuperseded(t *testing.T) {
	repository := openProjectionTestRepository(t)
	inventory := loadProjectionInventory(t, repository, "project-1")
	model := projectionTestModel("model-a", "1")
	now := time.Unix(1_800_000_000, 0).UTC()
	states := map[string]GenerationState{
		"superseded-newest": GenerationSuperseded,
		"superseded-middle": GenerationSuperseded,
		"superseded-oldest": GenerationSuperseded,
		"building-old":      GenerationBuilding,
		"failed-old":        GenerationFailed,
		"active-current":    GenerationActive,
	}
	index := 0
	for id, state := range states {
		beginProjectionGeneration(t, repository, id, inventory, model)
		created := now.Add(-time.Duration(index+1) * time.Hour)
		if state == GenerationBuilding || state == GenerationFailed {
			created = now.Add(-48 * time.Hour)
		}
		if _, err := repository.db.ExecContext(t.Context(),
			"UPDATE context_embedding_generations SET state=?,created_at=? WHERE generation_id=?",
			state, created.UnixMilli(), id,
		); err != nil {
			t.Fatal(err)
		}
		index++
	}
	// Make superseded ordering deterministic instead of depending on map order.
	for id, created := range map[string]time.Time{
		"superseded-newest": now.Add(-time.Hour),
		"superseded-middle": now.Add(-2 * time.Hour),
		"superseded-oldest": now.Add(-3 * time.Hour),
	} {
		if _, err := repository.db.ExecContext(t.Context(), "UPDATE context_embedding_generations SET created_at=? WHERE generation_id=?", created.UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := repository.CleanupInactiveGenerations(t.Context(), GenerationCleanupPolicy{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("cleanup deleted %d generations, want 3", deleted)
	}
	var active int
	if err = repository.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM context_embedding_generations WHERE generation_id='active-current' AND state='active'").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatal("cleanup deleted active generation")
	}
	if err = repository.MarkGenerationFailed(t.Context(), "active-current"); err == nil {
		t.Fatal("active generation was allowed to transition to failed")
	}
}

func TestAppendGenerationRowsRejectsMalformedVectors(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "project-1", "item-1")
	inventory := loadProjectionInventory(t, repository, "project-1")
	model := projectionTestModel("model-a", "1")
	generation := beginProjectionGeneration(t, repository, "generation-malformed", inventory, model)
	tests := []EmbeddingRow{
		{ItemID: "item-1", ContentHash: inventory.Items[0].ContentHash, Dimensions: 1, Vector: []float32{1}},
		{ItemID: "item-1", ContentHash: inventory.Items[0].ContentHash, Dimensions: 2, Vector: []float32{float32(math.NaN()), 0}},
	}
	for _, row := range tests {
		if err := repository.AppendGenerationRows(t.Context(), generation.GenerationID, []EmbeddingRow{row}); err == nil {
			t.Fatalf("malformed row was accepted: %#v", row)
		}
	}
}

func TestMigrationEightToNineCreatesBackupAndPreservesCanonicalData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repository, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	appendProjectionItems(t, repository, "project-1", "pre-migration")
	if _, err = repository.db.ExecContext(t.Context(), `DROP TABLE context_embeddings;
		DROP TABLE context_embedding_generations;
		DELETE FROM schema_migrations WHERE version=9;`); err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}

	repository, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("migration 8 to 9 did not create a recovery backup")
	}
	if _, err = repository.Get(t.Context(), "pre-migration"); err != nil {
		t.Fatalf("canonical data did not survive migration: %v", err)
	}
	var version int
	if err = repository.db.QueryRowContext(t.Context(), "SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 9 {
		t.Fatalf("schema version = %d, want 9", version)
	}
}

func openProjectionTestRepository(t *testing.T) *SQLiteRepository {
	t.Helper()
	repository, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func appendProjectionItems(t *testing.T, repository *SQLiteRepository, projectID string, ids ...string) {
	t.Helper()
	items := make([]ContextItem, len(ids))
	for i, id := range ids {
		items[i] = ContextItem{ID: id, Kind: ContextDecision, Content: "content for " + id, Scope: Scope{ProjectID: projectID}}
	}
	if err := repository.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}
}

func loadProjectionInventory(t *testing.T, repository *SQLiteRepository, projectID string) EmbeddingInventory {
	t.Helper()
	inventory, err := repository.LoadEmbeddingInventory(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

func beginProjectionGeneration(
	t *testing.T,
	repository *SQLiteRepository,
	id string,
	inventory EmbeddingInventory,
	model embedding.ModelIdentity,
) Generation {
	t.Helper()
	generation, err := repository.BeginGeneration(t.Context(), GenerationSpec{
		GenerationID: id, ProjectID: inventory.ProjectID, Model: model,
		SourceRevision: inventory.Revision, ExpectedCount: len(inventory.Items), ContentDigest: inventory.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func appendProjectionRows(
	t *testing.T,
	repository *SQLiteRepository,
	generationID string,
	inventory EmbeddingInventory,
	dimensions int,
) {
	t.Helper()
	if err := repository.AppendGenerationRows(t.Context(), generationID, projectionRows(inventory, dimensions)); err != nil {
		t.Fatal(err)
	}
}

func projectionRows(inventory EmbeddingInventory, dimensions int) []EmbeddingRow {
	rows := make([]EmbeddingRow, len(inventory.Items))
	for i, item := range inventory.Items {
		vector := make([]float32, dimensions)
		vector[i%dimensions] = 1
		rows[i] = EmbeddingRow{ItemID: item.ItemID, ContentHash: item.ContentHash, Dimensions: dimensions, Vector: vector}
	}
	return rows
}

func projectionTestModel(id, revision string) embedding.ModelIdentity {
	return embedding.ModelIdentity{
		ID: id, Revision: revision, Dimensions: 2,
		ManifestSHA256:  projectionHash(id + revision + " manifest"),
		TableSHA256:     projectionHash(id + revision + " table"),
		TokenizerSHA256: projectionHash(id + revision + " tokenizer"),
	}
}

func projectionHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
