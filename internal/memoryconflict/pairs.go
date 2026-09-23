package memoryconflict

import (
	"context"
	"fmt"
	"sort"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// SimilarSearcher is satisfied by *contextstore.VectorStore.
type SimilarSearcher interface {
	SearchSimilarTo(ctx context.Context, itemID string, req contextstore.SearchRequest) ([]contextstore.SearchResult, error)
}

type PairOptions struct {
	TopK              int     // lexical neighbors per item; default 5
	MinSharedTokens   int     // default 2
	MinOverlap        float64 // |A∩B| / min(|A|,|B|); default 0.2
	MaxItemsPerScope  int     // default 2000; exceeding it is an error
	MaxFilePathFanout int     // a path shared by more items is ignored; default 20
	// Vector adds semantic neighbors; nil disables the vector source.
	Vector     SimilarSearcher
	VectorTopK int // default 5
	// MinVectorSimilarity is the chromem cosine similarity floor; default
	// 0.45. Measured with nomic-embed-text, unrelated memories score about
	// 0.37-0.38 while "Use SQLite for storage" vs "Architecture migrated to
	// PostgreSQL" scores 0.487, so 0.5 would miss that contradiction.
	MinVectorSimilarity float64
}

func (o PairOptions) withDefaults() PairOptions {
	if o.TopK <= 0 {
		o.TopK = 5
	}
	if o.MinSharedTokens <= 0 {
		o.MinSharedTokens = 2
	}
	if o.MinOverlap <= 0 {
		o.MinOverlap = 0.2
	}
	if o.MaxItemsPerScope <= 0 {
		o.MaxItemsPerScope = 2000
	}
	if o.MaxFilePathFanout <= 0 {
		o.MaxFilePathFanout = 20
	}
	if o.VectorTopK <= 0 {
		o.VectorTopK = 5
	}
	if o.MinVectorSimilarity <= 0 {
		o.MinVectorSimilarity = 0.45
	}
	return o
}

// Pair is two memories in the same scope with A.ID < B.ID.
type Pair struct{ A, B contextstore.ContextItem }

type CandidateSet struct {
	Pairs       []Pair
	Diagnostics []Diagnostic
	VectorUsed  bool
	// Distinct pairs each source contributed before the union.
	LexicalPairs, FilePathPairs, VectorPairs int
}

type pairKey struct{ a, b string }

type pairCollector struct {
	byID  map[string]contextstore.ContextItem
	pairs map[pairKey]Pair
}

// add records a pair and reports whether the source had not already added it.
// Identical content is a duplicate, never a contradiction, and is skipped.
func (c *pairCollector) add(seen map[pairKey]struct{}, x, y string) bool {
	if x == y {
		return false
	}
	if y < x {
		x, y = y, x
	}
	a, b := c.byID[x], c.byID[y]
	if a.ContentHash == b.ContentHash {
		return false
	}
	key := pairKey{a: x, b: y}
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	c.pairs[key] = Pair{A: a, B: b}
	return true
}

type scored struct {
	id    string
	score float64
}

func topScored(candidates []scored, n int) []scored {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].id < candidates[j].id
	})
	return candidates[:min(n, len(candidates))]
}

type scopeKey struct{ project, team, agent string }

// CandidatePairs builds candidate pairs from token overlap, shared file-path
// evidence, and (when opts.Vector is set) semantic neighbors. Pairs never
// cross project, team, or agent scope. Any vector error drops every vector
// candidate of this call so the result does not depend on where it failed.
func CandidatePairs(ctx context.Context, items []contextstore.ContextItem, opts PairOptions) (CandidateSet, error) {
	opts = opts.withDefaults()
	groups := map[scopeKey][]contextstore.ContextItem{}
	for _, item := range items {
		key := scopeKey{item.Scope.ProjectID, item.Scope.TeamID, item.Scope.AgentID}
		groups[key] = append(groups[key], item)
	}
	keys := make([]scopeKey, 0, len(groups))
	for key, group := range groups {
		if len(group) > opts.MaxItemsPerScope {
			return CandidateSet{}, fmt.Errorf("scope %s/%s/%s has %d memories, over the limit of %d; raise --max-items", key.project, key.team, key.agent, len(group), opts.MaxItemsPerScope)
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].project != keys[j].project {
			return keys[i].project < keys[j].project
		}
		if keys[i].team != keys[j].team {
			return keys[i].team < keys[j].team
		}
		return keys[i].agent < keys[j].agent
	})

	var set CandidateSet
	collector := &pairCollector{byID: map[string]contextstore.ContextItem{}, pairs: map[pairKey]Pair{}}
	lexicalSeen, pathSeen := map[pairKey]struct{}{}, map[pairKey]struct{}{}
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		for _, item := range group {
			collector.byID[item.ID] = item
		}
		set.LexicalPairs += lexicalPairs(collector, lexicalSeen, group, opts)
		pathPairs, diagnostics := filePathPairs(collector, pathSeen, group, opts)
		set.FilePathPairs += pathPairs
		set.Diagnostics = append(set.Diagnostics, diagnostics...)
	}

	if opts.Vector != nil {
		vectorPairs, err := vectorCandidates(ctx, keys, groups, opts)
		if err != nil {
			set.Diagnostics = append(set.Diagnostics, Diagnostic{Reason: ReasonVectorUnavailable})
		} else {
			set.VectorUsed = true
			vectorSeen := map[pairKey]struct{}{}
			for _, p := range vectorPairs {
				if collector.add(vectorSeen, p.a, p.b) {
					set.VectorPairs++
				}
			}
		}
	}

	set.Pairs = make([]Pair, 0, len(collector.pairs))
	for _, pair := range collector.pairs {
		set.Pairs = append(set.Pairs, pair)
	}
	sort.Slice(set.Pairs, func(i, j int) bool {
		if set.Pairs[i].A.ID != set.Pairs[j].A.ID {
			return set.Pairs[i].A.ID < set.Pairs[j].A.ID
		}
		return set.Pairs[i].B.ID < set.Pairs[j].B.ID
	})
	return set, nil
}

func lexicalPairs(collector *pairCollector, seen map[pairKey]struct{}, group []contextstore.ContextItem, opts PairOptions) int {
	tokens := make([]map[string]struct{}, len(group))
	for i, item := range group {
		tokens[i] = Tokens(item.Content)
	}
	added := 0
	for i, x := range group {
		var candidates []scored
		for j, y := range group {
			if i == j || len(tokens[i]) == 0 || len(tokens[j]) == 0 {
				continue
			}
			shared := 0
			for token := range tokens[i] {
				if _, ok := tokens[j][token]; ok {
					shared++
				}
			}
			overlap := float64(shared) / float64(min(len(tokens[i]), len(tokens[j])))
			if shared >= opts.MinSharedTokens && overlap >= opts.MinOverlap {
				candidates = append(candidates, scored{id: y.ID, score: overlap})
			}
		}
		for _, c := range topScored(candidates, opts.TopK) {
			if collector.add(seen, x.ID, c.id) {
				added++
			}
		}
	}
	return added
}

func filePathPairs(collector *pairCollector, seen map[pairKey]struct{}, group []contextstore.ContextItem, opts PairOptions) (int, []Diagnostic) {
	byPath := map[string][]string{}
	for _, item := range group {
		paths := map[string]struct{}{}
		for _, evidence := range item.Evidence {
			if evidence.Type == "file_path" && evidence.Ref != "" {
				paths[evidence.Ref] = struct{}{}
			}
		}
		for path := range paths {
			byPath[path] = append(byPath[path], item.ID)
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	added := 0
	var diagnostics []Diagnostic
	for _, path := range paths {
		ids := byPath[path]
		if len(ids) > opts.MaxFilePathFanout {
			diagnostics = append(diagnostics, Diagnostic{Reason: ReasonFilePathTooCommon, Detail: path})
			continue
		}
		sort.Strings(ids)
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				if collector.add(seen, ids[i], ids[j]) {
					added++
				}
			}
		}
	}
	return added, diagnostics
}

func vectorCandidates(ctx context.Context, keys []scopeKey, groups map[scopeKey][]contextstore.ContextItem, opts PairOptions) ([]pairKey, error) {
	var pairs []pairKey
	for _, key := range keys {
		group := groups[key]
		inGroup := make(map[string]struct{}, len(group))
		for _, item := range group {
			inGroup[item.ID] = struct{}{}
		}
		for _, x := range group {
			results, err := opts.Vector.SearchSimilarTo(ctx, x.ID, contextstore.SearchRequest{
				Scope:      contextstore.Scope{ProjectID: key.project, TeamID: key.team, AgentID: key.agent},
				Visibility: contextstore.VisibilityExact,
				Limit:      4 * opts.VectorTopK,
			})
			if err != nil {
				return nil, fmt.Errorf("vector neighbors of %s: %w", x.ID, err)
			}
			var candidates []scored
			for _, result := range results {
				if _, ok := inGroup[result.Item.ID]; !ok || result.Item.ID == x.ID || result.Score < opts.MinVectorSimilarity {
					continue
				}
				candidates = append(candidates, scored{id: result.Item.ID, score: result.Score})
			}
			for _, c := range topScored(candidates, opts.VectorTopK) {
				pairs = append(pairs, pairKey{a: x.ID, b: c.id})
			}
		}
	}
	return pairs, nil
}
