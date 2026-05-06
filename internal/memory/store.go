package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/config"
	"github.com/philippgille/chromem-go"
)

const collectionName = "memory"
const metaFileName = "embedding_meta.json"

type Result struct {
	ID         string
	Content    string
	Similarity float32
	Metadata   map[string]string
}

type MemoryStore struct {
	db         *chromem.DB
	collection *chromem.Collection
	embedFunc  chromem.EmbeddingFunc
	mu         sync.RWMutex
	basePath   string

	// lazy init fields
	initOnce    sync.Once
	initErr     error
	storePath   string
	embedModel  string
	ollamaURL   string
	initialized bool
}

type embeddingMeta struct {
	EmbeddingModel string `json:"embedding_model"`
	CreatedAt      string `json:"created_at"`
}

func projectDirHash(projectDir string) string {
	h := sha256.Sum256([]byte(projectDir))
	return fmt.Sprintf("%x", h)[:16]
}

func dataDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "hufu", "memory"), nil
}

func NewMemoryStore(projectDir, ollamaURL, embedModel string) (*MemoryStore, error) {
	return newLazyMemoryStore(projectDir, ollamaURL, embedModel, false)
}

func NewGlobalMemoryStore(ollamaURL, embedModel string) (*MemoryStore, error) {
	return newLazyMemoryStore("", ollamaURL, embedModel, true)
}

func newLazyMemoryStore(projectDir, ollamaURL, embedModel string, isGlobal bool) (*MemoryStore, error) {
	if ollamaURL == "" {
		ollamaURL = config.DefaultOllamaAPIURL
	}
	if embedModel == "" {
		embedModel = config.DefaultEmbeddingModel
	}

	embedFunc := chromem.NewEmbeddingFuncOllama(embedModel, ollamaURL)

	basePath, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("failed to determine memory data directory: %w", err)
	}

	var storePath string
	if isGlobal {
		storePath = filepath.Join(basePath, "_global")
	} else {
		hash := projectDirHash(projectDir)
		storePath = filepath.Join(basePath, hash)
	}

	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create memory store directory %s: %w", storePath, err)
	}

	return &MemoryStore{
		embedFunc:  embedFunc,
		basePath:   basePath,
		storePath:  storePath,
		embedModel: embedModel,
		ollamaURL:  ollamaURL,
	}, nil
}

func (s *MemoryStore) init() error {
	s.initOnce.Do(func() {
		s.initErr = s.doInit()
	})
	return s.initErr
}

func (s *MemoryStore) doInit() error {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()
	if err := probeEmbeddingModel(probeCtx, s.embedFunc); err != nil {
		return fmt.Errorf("embedding model %q is not available: %w", s.embedModel, err)
	}

	db, err := chromem.NewPersistentDB(s.storePath, true)
	if err != nil {
		return fmt.Errorf("failed to create persistent memory database: %w", err)
	}

	mismatch, err := checkEmbeddingModelMismatch(db, s.storePath, s.embedModel)
	if err != nil {
		log.Printf("warning: failed to check embedding model mismatch: %v", err)
	}
	if mismatch {
		log.Printf("embedding model changed; previous collection was deleted and will be recreated")
	}

	collection, err := db.GetOrCreateCollection(collectionName, map[string]string{"embedding_model": s.embedModel}, s.embedFunc)
	if err != nil {
		return fmt.Errorf("failed to create memory collection: %w", err)
	}

	s.db = db
	s.collection = collection
	s.initialized = true
	return nil
}

func (s *MemoryStore) Save(ctx context.Context, id, content string, metadata map[string]string) error {
	if err := s.init(); err != nil {
		return err
	}
	if metadata == nil {
		metadata = make(map[string]string)
	}

	cloned := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		cloned[k] = v
	}
	cloned["saved_at"] = time.Now().Format(time.RFC3339)

	doc := chromem.Document{
		ID:       id,
		Content:  content,
		Metadata: cloned,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.AddDocuments(ctx, []chromem.Document{doc}, runtime.NumCPU())
}

func (s *MemoryStore) Query(ctx context.Context, query string, n int, filter map[string]string) ([]Result, error) {
	if err := s.init(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	results, err := s.collection.Query(ctx, query, n, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("memory query failed: %w", err)
	}

	out := make([]Result, 0, len(results))
	for _, r := range results {
		out = append(out, Result{
			ID:         r.ID,
			Content:    r.Content,
			Similarity: r.Similarity,
			Metadata:   r.Metadata,
		})
	}
	return out, nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := s.init(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.Delete(ctx, nil, nil, id)
}

func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	if err := s.init(); err != nil {
		return fmt.Errorf("failed to initialize memory store for close: %w", err)
	}
	s.db = nil
	s.collection = nil
	s.initialized = false
	return nil
}

// readEmbeddingMeta reads the embedding metadata sidecar file.
// Returns nil, nil if the file does not exist.
func readEmbeddingMeta(storePath string) (*embeddingMeta, error) {
	metaPath := filepath.Join(storePath, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read embedding meta: %w", err)
	}
	var meta embeddingMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse embedding meta: %w", err)
	}
	return &meta, nil
}

// writeEmbeddingMeta writes the embedding metadata sidecar file.
func writeEmbeddingMeta(storePath string, meta *embeddingMeta) error {
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}
	metaPath := filepath.Join(storePath, metaFileName)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal embedding meta: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write embedding meta: %w", err)
	}
	return nil
}

// probeEmbeddingModel verifies the embedding model is available by embedding a probe string.
func probeEmbeddingModel(ctx context.Context, embedFunc chromem.EmbeddingFunc) error {
	_, err := embedFunc(ctx, "probe")
	if err != nil {
		return fmt.Errorf("embedding probe failed: %w", err)
	}
	return nil
}

// checkEmbeddingModelMismatch checks if the stored embedding model differs from the
// current one. If so, it deletes the collection and writes new metadata.
// Returns true if a mismatch was detected and the collection was deleted.
func checkEmbeddingModelMismatch(db *chromem.DB, storePath, embedModel string) (bool, error) {
	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		return false, err
	}

	// If no metadata exists, write it and return.
	if meta == nil {
		log.Printf("Warning: existing memory store has no embedding model metadata; assuming %q matches. If results seem wrong, re-index with: rm -rf %s", embedModel, storePath)
		newMeta := &embeddingMeta{
			EmbeddingModel: embedModel,
			CreatedAt:      time.Now().Format(time.RFC3339),
		}
		if err := writeEmbeddingMeta(storePath, newMeta); err != nil {
			return false, err
		}
		return false, nil
	}

	// Models match — nothing to do.
	if meta.EmbeddingModel == embedModel {
		return false, nil
	}

	// Models differ — delete the old collection first. If the process crashes
	// after deletion but before writing the new metadata, the next startup will
	// see no metadata (meta == nil) and write new metadata — a safe recovery path.
	// The reverse order (write-then-delete) would leave stale collection data
	// with matching metadata that would never be cleaned up.
	log.Printf("embedding model mismatch: stored=%q, current=%q; deleting old collection", meta.EmbeddingModel, embedModel)
	if err := db.DeleteCollection(collectionName); err != nil {
		return false, fmt.Errorf("failed to delete collection during model mismatch cleanup: %w", err)
	}

	newMeta := &embeddingMeta{
		EmbeddingModel: embedModel,
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	if err := writeEmbeddingMeta(storePath, newMeta); err != nil {
		return false, err
	}

	return true, nil
}
