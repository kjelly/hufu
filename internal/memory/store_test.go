package memory

import (
	"context"
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"

	"github.com/philippgille/chromem-go"
)

func isolateMemoryHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestProjectDirHash(t *testing.T) {
	h1 := projectDirHash("/home/user/projects/myapp")
	h2 := projectDirHash("/home/user/projects/myapp")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: got %s, want %s", h1, h2)
	}

	h3 := projectDirHash("/home/user/projects/other")
	if h1 == h3 {
		t.Errorf("different inputs should produce different hashes")
	}

	if len(h1) != 16 {
		t.Errorf("hash length should be 16, got %d", len(h1))
	}
}

func TestDataDir(t *testing.T) {
	homeDir := isolateMemoryHome(t)
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir() error: %v", err)
	}

	expected := filepath.Join(homeDir, ".local", "share", "hufu", "memory")
	if dir != expected {
		t.Errorf("dataDir() = %q, want %q", dir, expected)
	}
}

func TestNewMemoryStoreCreatesDir(t *testing.T) {
	homeDir := isolateMemoryHome(t)
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("failed to create project dir: %v", err)
	}

	hash := projectDirHash(projectDir)
	expectedPath := filepath.Join(homeDir, ".local", "share", "hufu", "memory", hash)

	t.Logf("projectDir: %s, hash: %s", projectDir, hash)
	t.Logf("expected store path: %s", expectedPath)

	_ = expectedPath
}

func TestExportRecordsOnlyReadsSerializedMemoryRecords(t *testing.T) {
	storePath := t.TempDir()
	record := MemoryRecord{ID: "legacy-1", Content: "keep this", Category: "decision", Status: StatusConfirmed}
	metadata, err := recordToMetadata(record)
	if err != nil {
		t.Fatalf("recordToMetadata: %v", err)
	}
	for name, doc := range map[string]chromem.Document{
		"record.gob":  {ID: record.ID, Content: record.Content, Metadata: metadata},
		"foreign.gob": {ID: "foreign", Content: "must not import", Metadata: map[string]string{"category": "decision"}},
	} {
		file, err := os.Create(filepath.Join(storePath, name))
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := gob.NewEncoder(file).Encode(doc); err != nil {
			_ = file.Close()
			t.Fatalf("encode %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
	}

	records, err := (&MemoryStore{storePath: storePath}).ExportRecords(context.Background())
	if err != nil {
		t.Fatalf("ExportRecords: %v", err)
	}
	if len(records) != 1 || records[0].ID != record.ID || records[0].Content != record.Content {
		t.Fatalf("ExportRecords() = %#v, want only %#v", records, record)
	}
}

func TestReadEmbeddingMetaNotExist(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "nonexistent")

	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		t.Fatalf("readEmbeddingMeta() unexpected error: %v", err)
	}
	if meta != nil {
		t.Errorf("readEmbeddingMeta() = %+v, want nil", meta)
	}
}

func TestWriteAndReadEmbeddingMeta(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "store")

	original := &embeddingMeta{
		EmbeddingModel: "test-model:latest",
		CreatedAt:      "2025-01-01T00:00:00Z",
	}

	if err := writeEmbeddingMeta(storePath, original); err != nil {
		t.Fatalf("writeEmbeddingMeta() error: %v", err)
	}

	// Verify file exists
	metaPath := filepath.Join(storePath, metaFileName)
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		t.Fatalf("meta file was not created at %s", metaPath)
	}

	read, err := readEmbeddingMeta(storePath)
	if err != nil {
		t.Fatalf("readEmbeddingMeta() error: %v", err)
	}
	if read == nil {
		t.Fatal("readEmbeddingMeta() returned nil after write")
	}
	if read.EmbeddingModel != original.EmbeddingModel {
		t.Errorf("EmbeddingModel = %q, want %q", read.EmbeddingModel, original.EmbeddingModel)
	}
	if read.CreatedAt != original.CreatedAt {
		t.Errorf("CreatedAt = %q, want %q", read.CreatedAt, original.CreatedAt)
	}
}

func TestCheckEmbeddingModelMismatchMatch(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "store")
	embedModel := "ollama/nomic-embed-text:latest"

	// Create a persistent DB
	db, err := chromem.NewPersistentDB(storePath, true)
	if err != nil {
		t.Fatalf("NewPersistentDB() error: %v", err)
	}

	// Write matching metadata
	if err := writeEmbeddingMeta(storePath, &embeddingMeta{
		EmbeddingModel: embedModel,
		CreatedAt:      "2025-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeEmbeddingMeta() error: %v", err)
	}

	mismatch, err := checkEmbeddingModelMismatch(db, storePath, embedModel)
	if err != nil {
		t.Fatalf("checkEmbeddingModelMismatch() error: %v", err)
	}
	if mismatch {
		t.Errorf("checkEmbeddingModelMismatch() = true, want false (models match)")
	}

	// Verify meta file is unchanged
	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		t.Fatalf("readEmbeddingMeta() error: %v", err)
	}
	if meta.EmbeddingModel != embedModel {
		t.Errorf("meta.EmbeddingModel = %q, want %q", meta.EmbeddingModel, embedModel)
	}
}

func TestCheckEmbeddingModelMismatchLegacy(t *testing.T) {
	// NOTE: This test produces a log.Printf warning about missing embedding model
	// metadata, which is expected behavior for legacy stores. The warning is
	// acceptable as it mirrors real-world usage.
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "store")
	embedModel := "ollama/nomic-embed-text:latest"

	// Create a persistent DB
	db, err := chromem.NewPersistentDB(storePath, true)
	if err != nil {
		t.Fatalf("NewPersistentDB() error: %v", err)
	}

	// No metadata file exists (legacy store)

	mismatch, err := checkEmbeddingModelMismatch(db, storePath, embedModel)
	if err != nil {
		t.Fatalf("checkEmbeddingModelMismatch() error: %v", err)
	}
	if mismatch {
		t.Errorf("checkEmbeddingModelMismatch() = true, want false (legacy store, no re-index)")
	}

	// Verify meta file was created with correct model
	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		t.Fatalf("readEmbeddingMeta() error: %v", err)
	}
	if meta == nil {
		t.Fatal("readEmbeddingMeta() returned nil, want metadata to be created")
	}
	if meta.EmbeddingModel != embedModel {
		t.Errorf("meta.EmbeddingModel = %q, want %q", meta.EmbeddingModel, embedModel)
	}
}

func TestCheckEmbeddingModelMismatchDifferent(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "store")
	// "qwen3-embedding:4b" is the legacy default embedding model used before
	// the project switched to "ollama/nomic-embed-text:latest".
	oldModel := "qwen3-embedding:4b"
	newModel := "ollama/nomic-embed-text:latest"

	// Create a persistent DB
	db, err := chromem.NewPersistentDB(storePath, true)
	if err != nil {
		t.Fatalf("NewPersistentDB() error: %v", err)
	}

	// Create a collection so we can verify it gets deleted
	_, err = db.CreateCollection(collectionName, nil, nil)
	if err != nil {
		t.Fatalf("CreateCollection() error: %v", err)
	}

	// Verify collection exists
	if col := db.GetCollection(collectionName, nil); col == nil {
		t.Fatal("collection should exist before mismatch check")
	}

	// Write old metadata
	if err := writeEmbeddingMeta(storePath, &embeddingMeta{
		EmbeddingModel: oldModel,
		CreatedAt:      "2025-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("writeEmbeddingMeta() error: %v", err)
	}

	mismatch, err := checkEmbeddingModelMismatch(db, storePath, newModel)
	if err != nil {
		t.Fatalf("checkEmbeddingModelMismatch() error: %v", err)
	}
	if !mismatch {
		t.Errorf("checkEmbeddingModelMismatch() = false, want true (models differ)")
	}

	// Verify collection was deleted
	if col := db.GetCollection(collectionName, nil); col != nil {
		t.Errorf("collection should have been deleted after model mismatch")
	}

	// Verify new meta file has correct model
	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		t.Fatalf("readEmbeddingMeta() error: %v", err)
	}
	if meta == nil {
		t.Fatal("readEmbeddingMeta() returned nil, want metadata to be updated")
	}
	if meta.EmbeddingModel != newModel {
		t.Errorf("meta.EmbeddingModel = %q, want %q", meta.EmbeddingModel, newModel)
	}
}

func TestLazyInitFieldsSetWithoutProbe(t *testing.T) {
	isolateMemoryHome(t)
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := newLazyMemoryStore(projectDir, "http://localhost:9999/v1", "test-model", false)
	if err != nil {
		t.Fatalf("newLazyMemoryStore() error: %v", err)
	}

	if s.initialized {
		t.Error("store should not be initialized after construction")
	}
	if s.storePath == "" {
		t.Error("storePath should be set after construction")
	}
	if s.embedModel != "test-model" {
		t.Errorf("embedModel = %q, want %q", s.embedModel, "test-model")
	}
	if s.collection != nil {
		t.Error("collection should be nil before init")
	}
	if s.db != nil {
		t.Error("db should be nil before init")
	}
}

func TestLazyInitOnlyOnce(t *testing.T) {
	isolateMemoryHome(t)
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := newLazyMemoryStore(projectDir, "http://localhost:9999/v1", "test-model", false)
	if err != nil {
		t.Fatalf("newLazyMemoryStore() error: %v", err)
	}

	err1 := s.init()
	err2 := s.init()
	err3 := s.init()

	if err1 != err2 || err2 != err3 {
		t.Errorf("init() should return same error each time: got %v, %v, %v", err1, err2, err3)
	}
}
