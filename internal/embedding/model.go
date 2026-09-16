package embedding

import (
	"context"
	"fmt"
)

const maxInputBytes = 1 << 20

// Model is immutable after construction and safe for concurrent embedding.
type Model struct {
	identity       ModelIdentity
	tokenizer      *wordPieceTokenizer
	vectors        []float32
	dimensions     int
	queryPrefix    string
	documentPrefix string
	skipTokenIDs   map[int]struct{}
}

func (m *Model) Identity() ModelIdentity {
	return m.identity
}

func (m *Model) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return m.embed(ctx, m.queryPrefix, text)
}

func (m *Model) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return m.embed(ctx, m.documentPrefix, text)
}

func (m *Model) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vector, err := m.EmbedDocument(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("embed document %d: %w", i, err)
		}
		result[i] = vector
	}
	return result, nil
}

func (m *Model) embed(ctx context.Context, prefix, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(prefix) > maxInputBytes || len(text) > maxInputBytes-len(prefix) {
		return nil, fmt.Errorf("embedding input exceeds %d bytes", maxInputBytes)
	}
	ids, err := m.tokenizer.tokenize(ctx, prefix+text)
	if err != nil {
		return nil, err
	}
	return meanPoolNormalized(ctx, ids, m.vectors, m.dimensions, m.skipTokenIDs)
}
