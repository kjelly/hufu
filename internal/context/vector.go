package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	repo       Repository
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
	s.repo = repo
	var rebuildErrs []error
	for _, item := range items {
		doc := chromem.Document{ID: item.ID, Content: item.Content}
		if err := s.collection.AddDocuments(ctx, []chromem.Document{doc}, 1); err != nil {
			// The index was wiped before this attempt, so even a previously
			// embedded document is no longer indexed when re-embedding fails.
			// Mark it pending for a later retry, then continue indexing unrelated
			// documents instead of letting one failure abort the rebuild.
			if stateErr := repo.UpdateEmbeddingState(ctx, item.ID, "pending", s.model); stateErr != nil {
				rebuildErrs = append(rebuildErrs, fmt.Errorf("recording pending state for %q: %w", item.ID, stateErr))
			}
			rebuildErrs = append(rebuildErrs, fmt.Errorf("embedding %q: %w", item.ID, err))
			continue
		}
		if err := repo.UpdateEmbeddingState(ctx, item.ID, "embedded", s.model); err != nil {
			rebuildErrs = append(rebuildErrs, fmt.Errorf("recording embedded state for %q: %w", item.ID, err))
		}
	}
	return errors.Join(rebuildErrs...)
}

func (s *VectorStore) SearchVector(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if s.repo == nil {
		return nil, errors.New("vector store has no canonical repository; rebuild before searching")
	}
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
		item, err := s.repo.Get(ctx, result.ID)
		if errors.Is(err, sql.ErrNoRows) {
			continue // A stale rebuildable index must never surface a deleted canonical item.
		}
		if err != nil {
			return nil, fmt.Errorf("hydrating vector result %q from canonical store: %w", result.ID, err)
		}
		if !isRetrievable(item, req.Scope, time.Now()) {
			continue
		}
		out = append(out, SearchResult{Item: item, Score: float64(result.Similarity)})
	}
	return out, nil
}

// isRetrievable is the vector equivalent of SQLite's scope, supersede, and
// temporal predicates. Vector documents carry only content and canonical ID;
// every result must therefore be hydrated and authorized by the canonical row.
func isRetrievable(item ContextItem, scope Scope, now time.Time) bool {
	if scope.ProjectID == "" || item.Scope.ProjectID != scope.ProjectID || item.SupersededBy != "" {
		return false
	}
	for _, level := range [][2]string{{scope.TeamID, item.Scope.TeamID}, {scope.SessionID, item.Scope.SessionID}, {scope.AgentID, item.Scope.AgentID}, {scope.TaskID, item.Scope.TaskID}, {scope.AttemptID, item.Scope.AttemptID}} {
		if level[0] != "" && level[1] != "" && level[0] != level[1] {
			return false
		}
	}
	return (item.ValidFrom == nil || !item.ValidFrom.After(now)) &&
		(item.ValidUntil == nil || item.ValidUntil.After(now)) &&
		(item.ExpiresAt == nil || item.ExpiresAt.After(now))
}
