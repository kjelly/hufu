package team

import (
	"fmt"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// explainHistoricalMemory reads the frozen selection. The caller has already
// checked canonical item scope; this path performs no retrieval or reranking.
func explainHistoricalMemory(in MemoryExplainInput, scope contextstore.Scope) (MemoryExplanation, error) {
	session := LoadSession(in.Workspace)
	if session == nil {
		return MemoryExplanation{}, fmt.Errorf("historical memory manifest unavailable")
	}
	for _, task := range session.Tasks {
		if task == nil {
			continue
		}
		for _, manifest := range task.MemoryManifests {
			if manifest.RetrievalID != in.RetrievalID || (in.PolicyVersion != "" && manifest.PolicyVersion != in.PolicyVersion) {
				continue
			}
			if manifest.Query == nil {
				return MemoryExplanation{}, fmt.Errorf("retrieval has no versioned query plan; use --query for legacy recomputation")
			}
			if err := ValidateContextQueryAttribution(*manifest.Query); err != nil {
				return MemoryExplanation{}, err
			}
			if manifest.QueryHash != hashQueryAttribution(*manifest.Query) {
				return MemoryExplanation{}, fmt.Errorf("historical query hash mismatch")
			}
			for _, item := range manifest.Items {
				if item.ContextItemID != in.ItemID {
					continue
				}
				return MemoryExplanation{SchemaVersion: 3, Recomputed: false, Scope: scope, ModeSource: "historical_manifest",
					MemoryScoreExplanation: MemoryScoreExplanation{ContextItemID: item.ContextItemID, PolicyVersion: manifest.PolicyVersion, RetrievalID: manifest.RetrievalID, ScoreParts: item.ScoreParts, FinalScore: item.FinalScore},
					QueryHash:              manifest.QueryHash, QueryPolicy: cloneContextQueryAttribution(manifest.Query), QueryStatus: "query_plan_required",
					Ranking: MemoryExplainRanking{BaseRank: item.Rank, Selected: true, Injected: true},
					Reasons: []MemoryExplainReason{"historical_inclusion"}, NotEvaluated: []string{"current_ranking", "activation", "token_budget"}}, nil
			}
		}
	}
	return MemoryExplanation{}, fmt.Errorf("historical retrieval/item binding not found")
}
