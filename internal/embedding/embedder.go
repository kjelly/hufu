package embedding

import (
	"context"
	"errors"
)

var (
	ErrNoEmbeddableTokens = errors.New("no embeddable tokens")
	ErrInvalidEmbedding   = errors.New("invalid embedding")
)

// ModelIdentity is the immutable identity of a verified static embedding model.
type ModelIdentity struct {
	ID              string
	Revision        string
	Dimensions      int
	ManifestSHA256  string
	TableSHA256     string
	TokenizerSHA256 string
}

// Embedder separates query and document entry points so a future verified
// model may use asymmetric prefixes without changing retrieval callers.
type Embedder interface {
	Identity() ModelIdentity
	EmbedQuery(context.Context, string) ([]float32, error)
	EmbedDocument(context.Context, string) ([]float32, error)
	EmbedDocuments(context.Context, []string) ([][]float32, error)
}
