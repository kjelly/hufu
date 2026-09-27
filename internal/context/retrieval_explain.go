package context

import (
	"sort"
)

// RetrievalObservation records, for one HybridRetrieve call, how each path
// and fusion step ranked every candidate. It holds only IDs, ranks, scores,
// and enums; it lives in memory for the call that produced it and must never
// be persisted. Production callers leave HybridRetrievalOptions.Observer nil,
// and every method is a no-op on a nil receiver.
type RetrievalObservation struct {
	Paths      []RetrievalPathObservation      `json:"paths"`
	Candidates []RetrievalCandidateObservation `json:"candidates"`
	byID       map[string]*RetrievalCandidateObservation
}

// RetrievalPathObservation says whether one retrieval path ran.
type RetrievalPathObservation struct {
	Path              string                 `json:"path"` // "exact" | "lexical" | "vector"
	Executed          bool                   `json:"executed"`
	UnavailableReason SemanticFallbackReason `json:"unavailable_reason,omitempty"`
	ResultCount       int                    `json:"result_count"`
}

// RetrievalCandidateObservation is one candidate's rank and score at every
// step. Ranks are 1-based; zero means the step did not include the item.
type RetrievalCandidateObservation struct {
	ItemID       string  `json:"item_id"`
	ExactRank    int     `json:"exact_rank,omitempty"`
	ExactScore   float64 `json:"exact_score,omitempty"`
	LexicalRank  int     `json:"lexical_rank,omitempty"`
	LexicalScore float64 `json:"lexical_score,omitempty"`
	VectorRank   int     `json:"vector_rank,omitempty"`
	VectorScore  float64 `json:"vector_score,omitempty"`
	// CarriedScore is the raw score of the first fused list that contained
	// the item. rrf starts from it before adding reciprocal ranks, so
	// FusedScore == CarriedScore + LexicalRRF + VectorRRF.
	CarriedScore  float64 `json:"carried_score"`
	LexicalRRF    float64 `json:"lexical_rrf,omitempty"`
	VectorRRF     float64 `json:"vector_rrf,omitempty"`
	FusedScore    float64 `json:"fused_score"`
	DuplicateOf   string  `json:"duplicate_of,omitempty"`
	ExactPrefix   bool    `json:"exact_prefix,omitempty"`
	PreMMRRank    int     `json:"pre_mmr_rank,omitempty"`
	MMRRank       int     `json:"mmr_rank,omitempty"`
	MMRPenalty    float64 `json:"mmr_penalty,omitempty"`
	MMRScore      float64 `json:"mmr_score,omitempty"`
	FilePathBoost float64 `json:"file_path_boost,omitempty"`
	RetrievalRank int     `json:"retrieval_rank,omitempty"`
	CutByLimit    bool    `json:"cut_by_limit,omitempty"`
}

// Candidate returns the observation for one item.
func (o *RetrievalObservation) Candidate(id string) (RetrievalCandidateObservation, bool) {
	if o == nil {
		return RetrievalCandidateObservation{}, false
	}
	for _, candidate := range o.Candidates {
		if candidate.ItemID == id {
			return candidate, true
		}
	}
	return RetrievalCandidateObservation{}, false
}

func (o *RetrievalObservation) reset() {
	if o == nil {
		return
	}
	o.Paths, o.Candidates, o.byID = nil, nil, map[string]*RetrievalCandidateObservation{}
}

func (o *RetrievalObservation) candidate(id string) *RetrievalCandidateObservation {
	if o.byID == nil {
		o.byID = map[string]*RetrievalCandidateObservation{}
	}
	c, ok := o.byID[id]
	if !ok {
		c = &RetrievalCandidateObservation{ItemID: id}
		o.byID[id] = c
	}
	return c
}

func (o *RetrievalObservation) recordPath(path string, executed bool, reason SemanticFallbackReason, results []SearchResult) {
	if o == nil {
		return
	}
	if executed {
		reason = ""
	}
	o.Paths = append(o.Paths, RetrievalPathObservation{Path: path, Executed: executed, UnavailableReason: reason, ResultCount: len(results)})
	for i, result := range results {
		c := o.candidate(result.Item.ID)
		switch path {
		case "exact":
			c.ExactRank, c.ExactScore = i+1, result.Score
		case "lexical":
			c.LexicalRank, c.LexicalScore = i+1, result.Score
		case "vector":
			c.VectorRank, c.VectorScore = i+1, result.Score
		}
	}
}

func (o *RetrievalObservation) recordCarried(id string, score float64) {
	if o != nil {
		o.candidate(id).CarriedScore = score
	}
}

func (o *RetrievalObservation) recordRRF(path, id string, contribution float64) {
	if o == nil {
		return
	}
	c := o.candidate(id)
	switch path {
	case "lexical":
		c.LexicalRRF += contribution
	case "vector":
		c.VectorRRF += contribution
	}
}

func (o *RetrievalObservation) recordFused(id string, score float64, duplicateOf string) {
	if o == nil {
		return
	}
	c := o.candidate(id)
	c.FusedScore, c.DuplicateOf = score, duplicateOf
}

func (o *RetrievalObservation) recordPreMMR(results []SearchResult) {
	if o == nil {
		return
	}
	for i, result := range results {
		o.candidate(result.Item.ID).PreMMRRank = i + 1
	}
}

func (o *RetrievalObservation) recordMMR(id string, rank int, penalty, score float64) {
	if o == nil {
		return
	}
	c := o.candidate(id)
	c.MMRRank, c.MMRPenalty, c.MMRScore = rank, penalty, score
}

func (o *RetrievalObservation) recordExactPrefix(results []SearchResult) {
	if o == nil {
		return
	}
	for _, result := range results {
		o.candidate(result.Item.ID).ExactPrefix = true
	}
}

func (o *RetrievalObservation) recordBoost(ids []string, boost float64) {
	if o == nil {
		return
	}
	for _, id := range ids {
		o.candidate(id).FilePathBoost = boost
	}
}

// finish records the pre-limit order and the limit cut, then freezes the
// candidates into a stable slice.
func (o *RetrievalObservation) finish(ranked []SearchResult, limit int) {
	if o == nil {
		return
	}
	for i, result := range ranked {
		c := o.candidate(result.Item.ID)
		c.RetrievalRank = i + 1
		c.CutByLimit = limit > 0 && i >= limit
	}
	o.Candidates = make([]RetrievalCandidateObservation, 0, len(o.byID))
	for _, c := range o.byID {
		o.Candidates = append(o.Candidates, *c)
	}
	sort.Slice(o.Candidates, func(i, j int) bool {
		left, right := o.Candidates[i], o.Candidates[j]
		if (left.RetrievalRank == 0) != (right.RetrievalRank == 0) {
			return right.RetrievalRank == 0
		}
		if left.RetrievalRank != right.RetrievalRank {
			return left.RetrievalRank < right.RetrievalRank
		}
		return left.ItemID < right.ItemID
	})
}
