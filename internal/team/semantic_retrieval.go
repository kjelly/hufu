package team

import (
	"fmt"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
)

const SemanticRetrievalPolicyVersion = "hybrid-exact-fts-semantic-rrf60-mmr075-v1"

// SemanticRetrievalIdentity is the content-free identity of the semantic
// component that was allowed to affect an active retrieval. It deliberately
// excludes queries, item IDs, vectors, and raw errors.
type SemanticRetrievalIdentity struct {
	Mode                   contextstore.RetrievalMode          `json:"mode"`
	ModelID                string                              `json:"model_id"`
	ModelRevision          string                              `json:"model_revision"`
	ModelHash              string                              `json:"model_hash"`
	GenerationID           string                              `json:"generation_id"`
	SourceRevision         int64                               `json:"source_revision"`
	RetrievalPolicyVersion string                              `json:"retrieval_policy_version"`
	FallbackReason         contextstore.SemanticFallbackReason `json:"fallback_reason"`
}

type semanticTraceIdentityProvider interface {
	SemanticTraceIdentity() (embedding.ModelIdentity, string, int64)
}

func cloneSemanticRetrievalIdentity(src *SemanticRetrievalIdentity) *SemanticRetrievalIdentity {
	if src == nil {
		return nil
	}
	clone := *src
	return &clone
}

// SemanticRetrievalIdentityForTask returns the latest durable identity,
// preferring the receipt boundary and retaining read compatibility with
// checkpoints written before receipts carried the top-level projection.
func SemanticRetrievalIdentityForTask(item *TodoItem) *SemanticRetrievalIdentity {
	if item == nil {
		return nil
	}
	if item.ExecutionReceipt != nil {
		return semanticRetrievalIdentityFromReceipt(item.ExecutionReceipt)
	}
	if last := len(item.ExecutionReceipts) - 1; last >= 0 {
		return semanticRetrievalIdentityFromReceipt(&item.ExecutionReceipts[last])
	}
	for i := len(item.ContextManifests) - 1; i >= 0; i-- {
		if item.ContextManifests[i].Semantic != nil {
			return cloneSemanticRetrievalIdentity(item.ContextManifests[i].Semantic)
		}
	}
	for i := len(item.MemoryManifests) - 1; i >= 0; i-- {
		if item.MemoryManifests[i].Semantic != nil {
			return cloneSemanticRetrievalIdentity(item.MemoryManifests[i].Semantic)
		}
	}
	return nil
}

func semanticRetrievalIdentityFromReceipt(receipt *ExecutionReceipt) *SemanticRetrievalIdentity {
	if receipt == nil {
		return nil
	}
	if receipt.Semantic != nil {
		return cloneSemanticRetrievalIdentity(receipt.Semantic)
	}
	if receipt.ContextManifest != nil && receipt.ContextManifest.Semantic != nil {
		return cloneSemanticRetrievalIdentity(receipt.ContextManifest.Semantic)
	}
	if receipt.MemoryManifest != nil && receipt.MemoryManifest.Semantic != nil {
		return cloneSemanticRetrievalIdentity(receipt.MemoryManifest.Semantic)
	}
	return nil
}

func normalizeExecutionReceiptSemantic(receipt *ExecutionReceipt) error {
	if receipt == nil {
		return nil
	}
	identities := []*SemanticRetrievalIdentity{receipt.Semantic}
	if receipt.ContextManifest != nil {
		identities = append(identities, receipt.ContextManifest.Semantic)
	}
	if receipt.MemoryManifest != nil {
		identities = append(identities, receipt.MemoryManifest.Semantic)
	}
	var canonical *SemanticRetrievalIdentity
	for _, identity := range identities {
		if identity == nil {
			continue
		}
		if canonical == nil {
			canonical = identity
			continue
		}
		if *canonical != *identity {
			return fmt.Errorf("execution receipt semantic retrieval identity does not match dispatched manifests")
		}
	}
	receipt.Semantic = cloneSemanticRetrievalIdentity(canonical)
	return nil
}

func semanticRetrievalIdentity(
	mode contextstore.RetrievalMode,
	searcher contextstore.VectorSearcher,
	fallback contextstore.SemanticFallbackReason,
) *SemanticRetrievalIdentity {
	if mode != contextstore.RetrievalActive {
		return nil
	}
	identity := &SemanticRetrievalIdentity{
		Mode: mode, RetrievalPolicyVersion: SemanticRetrievalPolicyVersion,
		FallbackReason: fallback,
	}
	if provider, ok := searcher.(semanticTraceIdentityProvider); ok {
		model, generationID, sourceRevision := provider.SemanticTraceIdentity()
		identity.ModelID = model.ID
		identity.ModelRevision = model.Revision
		identity.ModelHash = model.ManifestSHA256
		identity.GenerationID = generationID
		identity.SourceRevision = sourceRevision
	}
	return identity
}

func mergeSemanticRetrievalIdentity(current, next *SemanticRetrievalIdentity) *SemanticRetrievalIdentity {
	if current == nil {
		return cloneSemanticRetrievalIdentity(next)
	}
	if next == nil {
		return cloneSemanticRetrievalIdentity(current)
	}
	// A successful semantic component is the strongest statement about an
	// active multi-scope recall and must remain bound to the generation that
	// produced it. Otherwise retain the latest complete fallback identity.
	if current.FallbackReason == contextstore.SemanticFallbackNone {
		return cloneSemanticRetrievalIdentity(current)
	}
	if next.FallbackReason == contextstore.SemanticFallbackNone || next.GenerationID != "" {
		return cloneSemanticRetrievalIdentity(next)
	}
	return cloneSemanticRetrievalIdentity(current)
}
