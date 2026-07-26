package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/philippgille/chromem-go"
)

const contextVectorCollection = "context_items"

// VectorStore is a rebuildable chromem index whose document IDs are canonical
// ContextItem IDs. The SQLite repository remains the source of truth.
type VectorStore struct {
	db         *chromem.DB
	collection *chromem.Collection
	model      string
	embed      chromem.EmbeddingFunc
	modelPath  string
}

func NewVectorStore(path, model string, embed chromem.EmbeddingFunc) (*VectorStore, error) {
	if model == "" || embed == nil {
		return nil, fmt.Errorf("vector model and embedding function are required")
	}
	db, err := chromem.NewPersistentDB(path, false)
	if err != nil {
		return nil, err
	}
	modelPath := filepath.Join(path, "embedding_model")
	oldModel, _ := os.ReadFile(modelPath)
	if strings.TrimSpace(string(oldModel)) != "" && strings.TrimSpace(string(oldModel)) != model {
		if err := db.DeleteCollection(contextVectorCollection); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(modelPath, []byte(model+"\n"), 0o600); err != nil {
		return nil, err
	}
	collection, err := db.GetOrCreateCollection(contextVectorCollection, map[string]string{"embedding_model": model}, embed)
	if err != nil {
		return nil, err
	}
	return &VectorStore{db: db, collection: collection, model: model, embed: embed, modelPath: modelPath}, nil
}

func OpenOllamaVectorStore(workspace, model, ollamaURL string) (*VectorStore, error) {
	return NewVectorStore(filepath.Join(workspace, "context-vectors"), model, chromem.NewEmbeddingFuncOllama(model, ollamaURL))
}

func (s *VectorStore) Rebuild(ctx context.Context, repo Repository, scope Scope) error {
	items, err := repo.Query(ctx, RepositoryQuery{Scope: scope, Limit: 100000})
	if err != nil {
		return err
	}
	if err := s.db.DeleteCollection(contextVectorCollection); err != nil {
		return err
	}
	collection, err := s.db.GetOrCreateCollection(contextVectorCollection, map[string]string{"embedding_model": s.model}, s.embed)
	if err != nil {
		return err
	}
	s.collection = collection
	docs := make([]chromem.Document, 0, len(items))
	for _, item := range items {
		docs = append(docs, chromem.Document{ID: item.ID, Content: item.Content})
	}
	if len(docs) == 0 {
		return nil
	}
	return s.collection.AddDocuments(ctx, docs, 1)
}

func (s *VectorStore) SearchVector(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if s.collection.Count() == 0 {
		return nil, nil
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > s.collection.Count() {
		limit = s.collection.Count()
	}
	results, err := s.collection.Query(ctx, req.Query, limit, nil, nil)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(results))
	for _, result := range results {
		out = append(out, SearchResult{Item: ContextItem{ID: result.ID, Content: result.Content, Scope: req.Scope}, Score: float64(result.Similarity)})
	}
	return out, nil
}
