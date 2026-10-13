package embedding

import (
	"context"
	"errors"
)

var (
	ErrNoEmbeddableTokens = errors.New("no embeddable tokens")
	ErrInvalidEmbedding   = errors.New("invalid embedding")
)

// Identity schemes say what the hash fields of a ModelIdentity attest.
const (
	// IdentitySchemeStaticTable: ManifestSHA256, TableSHA256 and
	// TokenizerSHA256 are hashes of verified local static-table files.
	IdentitySchemeStaticTable = "static-table-v1"
	// IdentitySchemeOllamaEncoder: ManifestSHA256 is the Ollama model digest
	// (the SHA-256 of its manifest), Revision is the EncoderProfile
	// fingerprint, and the table and tokenizer hashes are empty because
	// Ollama does not expose those bytes.
	IdentitySchemeOllamaEncoder = "ollama-encoder-v1"
)

// ModelIdentity is the immutable identity of an embedding encoder. An empty
// Scheme means IdentitySchemeStaticTable.
type ModelIdentity struct {
	Scheme          string
	ID              string
	Revision        string
	Dimensions      int
	ManifestSHA256  string
	TableSHA256     string
	TokenizerSHA256 string
}

// EffectiveScheme resolves the legacy empty scheme.
func (m ModelIdentity) EffectiveScheme() string {
	if m.Scheme == "" {
		return IdentitySchemeStaticTable
	}
	return m.Scheme
}

// Embedder separates query and document entry points so a future verified
// model may use asymmetric prefixes without changing retrieval callers.
type Embedder interface {
	Identity() ModelIdentity
	EmbedQuery(context.Context, string) ([]float32, error)
	EmbedDocument(context.Context, string) ([]float32, error)
	EmbedDocuments(context.Context, []string) ([][]float32, error)
}
