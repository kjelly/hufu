package memory

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/philippgille/chromem-go"
)

const collectionName = "memory"

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
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434/api"
	}
	if embedModel == "" {
		embedModel = "nomic-embed-text"
	}

	embedFunc := chromem.NewEmbeddingFuncOllama(embedModel, ollamaURL)

	basePath, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("failed to determine memory data directory: %w", err)
	}

	hash := projectDirHash(projectDir)
	storePath := filepath.Join(basePath, hash)

	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create memory store directory %s: %w", storePath, err)
	}

	db, err := chromem.NewPersistentDB(storePath, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create persistent memory database: %w", err)
	}

	collection, err := db.GetOrCreateCollection(collectionName, nil, embedFunc)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory collection: %w", err)
	}

	return &MemoryStore{
		db:         db,
		collection: collection,
		embedFunc:  embedFunc,
		basePath:   basePath,
	}, nil
}

func NewGlobalMemoryStore(ollamaURL, embedModel string) (*MemoryStore, error) {
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434/api"
	}
	if embedModel == "" {
		embedModel = "nomic-embed-text"
	}

	embedFunc := chromem.NewEmbeddingFuncOllama(embedModel, ollamaURL)

	basePath, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("failed to determine memory data directory: %w", err)
	}

	storePath := filepath.Join(basePath, "_global")

	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create global memory store directory %s: %w", storePath, err)
	}

	db, err := chromem.NewPersistentDB(storePath, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create persistent global memory database: %w", err)
	}

	collection, err := db.GetOrCreateCollection(collectionName, nil, embedFunc)
	if err != nil {
		return nil, fmt.Errorf("failed to create global memory collection: %w", err)
	}

	return &MemoryStore{
		db:         db,
		collection: collection,
		embedFunc:  embedFunc,
		basePath:   basePath,
	}, nil
}

func (s *MemoryStore) Save(ctx context.Context, id, content string, metadata map[string]string) error {
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata["saved_at"] = time.Now().Format(time.RFC3339)

	doc := chromem.Document{
		ID:       id,
		Content:  content,
		Metadata: metadata,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.AddDocuments(ctx, []chromem.Document{doc}, runtime.NumCPU())
}

func (s *MemoryStore) Query(ctx context.Context, query string, n int, filter map[string]string) ([]Result, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.Delete(ctx, nil, nil, id)
}

func (s *MemoryStore) Close() error {
	return nil
}
