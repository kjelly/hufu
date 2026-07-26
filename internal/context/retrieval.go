package context

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

// QueryParts is the deterministic decomposition used before hybrid retrieval.
// It intentionally does not require an extra model call.
type QueryParts struct {
	Original    string   `json:"original"`
	Quoted      []string `json:"quoted,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	Symbols     []string `json:"symbols,omitempty"`
	Commands    []string `json:"commands,omitempty"`
	SHAs        []string `json:"shas,omitempty"`
	ErrorCodes  []string `json:"error_codes,omitempty"`
	TaskIDs     []string `json:"task_ids,omitempty"`
	AttemptIDs  []string `json:"attempt_ids,omitempty"`
	ToolNames   []string `json:"tool_names,omitempty"`
	IPs         []string `json:"ips,omitempty"`
	Ports       []string `json:"ports,omitempty"`
	ArtifactIDs []string `json:"artifact_ids,omitempty"`
	Remainder   string   `json:"remainder,omitempty"`
}

var quotedQueryRE = regexp.MustCompile(`"([^"\n]+)"|'([^'\n]+)'`)
var pathQueryRE = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+`)
var symbolQueryRE = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*\([^)]*\)`)
var commandQueryRE = regexp.MustCompile("(?:`)?((?:go|git|npm|make|cargo|pytest)\\s+[^\\n`]+)")
var shaQueryRE = regexp.MustCompile(`\b[0-9a-fA-F]{7,64}\b`)
var errorCodeQueryRE = regexp.MustCompile(`\b(?:E\d{2,5}|[A-Za-z]+Error)\b`)
var taskIDQueryRE = regexp.MustCompile(`(?i)\btask[-_ ]?id[:= ]+([A-Za-z0-9._-]+)`)
var attemptIDQueryRE = regexp.MustCompile(`(?i)\battempt[-_ ]?id[:= ]+([A-Za-z0-9._-]+)`)
var toolNameQueryRE = regexp.MustCompile(`(?i)\btool(?:_name)?[:= ]+([A-Za-z0-9._-]+)`)
var ipQueryRE = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
var portQueryRE = regexp.MustCompile(`:(\d{2,5})\b`)
var artifactIDQueryRE = regexp.MustCompile(`(?i)\bartifact[-_ ]?id[:= ]+([A-Za-z0-9._-]+)`)

func DecomposeQuery(query string) QueryParts {
	p := QueryParts{Original: query}
	for _, m := range quotedQueryRE.FindAllStringSubmatch(query, -1) {
		p.Quoted = appendUniqueString(p.Quoted, firstNonEmpty(m[1], m[2]))
	}
	p.Paths = retrievalUniqueStrings(pathQueryRE.FindAllString(query, -1))
	p.Symbols = retrievalUniqueStrings(symbolQueryRE.FindAllString(query, -1))
	for _, m := range commandQueryRE.FindAllStringSubmatch(query, -1) {
		if len(m) > 1 {
			p.Commands = appendUniqueString(p.Commands, strings.TrimSpace(m[1]))
		}
	}
	p.SHAs = retrievalUniqueStrings(shaQueryRE.FindAllString(query, -1))
	p.ErrorCodes = retrievalUniqueStrings(errorCodeQueryRE.FindAllString(query, -1))
	p.TaskIDs = captureTerms(taskIDQueryRE, query)
	p.AttemptIDs = captureTerms(attemptIDQueryRE, query)
	p.ToolNames = captureTerms(toolNameQueryRE, query)
	p.ArtifactIDs = captureTerms(artifactIDQueryRE, query)
	p.IPs = retrievalUniqueStrings(ipQueryRE.FindAllString(query, -1))
	p.Ports = captureTerms(portQueryRE, query)
	p.Remainder = strings.TrimSpace(query)
	return p
}
func captureTerms(re *regexp.Regexp, query string) []string {
	var out []string
	for _, m := range re.FindAllStringSubmatch(query, -1) {
		if len(m) > 1 {
			out = appendUniqueString(out, m[1])
		}
	}
	return out
}

func retrievalUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func appendUniqueString(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	if value != "" {
		return append(values, value)
	}
	return values
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// VectorSearcher is deliberately separate from Repository: the vector index
// is rebuildable and must never become the canonical source of records.
type VectorSearcher interface {
	SearchVector(context.Context, SearchRequest) ([]SearchResult, error)
}

type RetrievalTrace struct {
	Query                 string         `json:"query"`
	ExactResults          []SearchResult `json:"exact_results"`
	LexicalResults        []SearchResult `json:"lexical_results"`
	VectorResults         []SearchResult `json:"vector_results"`
	FusedResults          []SearchResult `json:"fused_results"`
	Selected              []string       `json:"selected"`
	RetrievalInsufficient bool           `json:"retrieval_insufficient"`
}

// HybridRetrieve applies exact-first selection, reciprocal-rank fusion (k=60),
// content deduplication (the first MMR pass), and deterministic tie-breakers.
func HybridRetrieve(ctx context.Context, repo Repository, vector VectorSearcher, req SearchRequest) ([]SearchResult, RetrievalTrace, error) {
	trace := RetrievalTrace{Query: req.Query}
	parts := DecomposeQuery(req.Query)
	exactTerms := []string{}
	for _, group := range [][]string{parts.Quoted, parts.Paths, parts.Symbols, parts.Commands, parts.SHAs, parts.ErrorCodes, parts.TaskIDs, parts.AttemptIDs, parts.ToolNames, parts.IPs, parts.Ports, parts.ArtifactIDs} {
		exactTerms = append(exactTerms, group...)
	}
	for _, exact := range exactTerms {
		found, err := repo.SearchExact(ctx, SearchRequest{Query: exact, Scope: req.Scope, Limit: req.Limit})
		if err != nil {
			return nil, trace, err
		}
		trace.ExactResults = mergeResults(trace.ExactResults, found)
	}
	lexical, err := repo.SearchLexical(ctx, req)
	if err != nil {
		return nil, trace, err
	}
	trace.LexicalResults = lexical
	if vector != nil {
		vectorResults, vectorErr := vector.SearchVector(ctx, req)
		if vectorErr == nil {
			trace.VectorResults = vectorResults
		}
	}
	fused := rrf(trace.LexicalResults, trace.VectorResults)
	fused = applyMMR(rankForScope(fused, req.Scope), 0.75)
	// Exact matches are a deterministic prefix, not a short-circuit: lexical
	// and vector retrieval can still contribute relevant context for the rest
	// of a mixed query.
	trace.FusedResults = mergeResults(rankForScope(trace.ExactResults, req.Scope), fused)
	if req.Limit > 0 && len(trace.FusedResults) > req.Limit {
		trace.FusedResults = trace.FusedResults[:req.Limit]
	}
	trace.Selected = resultIDs(trace.FusedResults)
	trace.RetrievalInsufficient = len(trace.FusedResults) == 0 || (len(trace.ExactResults) == 0 && !hasRelevantScore(trace.LexicalResults) && !hasRelevantScore(trace.VectorResults))
	return trace.FusedResults, trace, nil
}
func hasRelevantScore(results []SearchResult) bool {
	for _, result := range results {
		if result.Score >= .05 {
			return true
		}
	}
	return false
}

func rrf(lists ...[]SearchResult) []SearchResult {
	scores := map[string]SearchResult{}
	for _, list := range lists {
		for rank, result := range list {
			current, ok := scores[result.Item.ID]
			if !ok {
				current = result
			}
			current.Score += 1.0 / float64(60+rank+1)
			scores[result.Item.ID] = current
		}
	}
	out := make([]SearchResult, 0, len(scores))
	for _, result := range scores {
		out = append(out, result)
	}
	// Map iteration is deliberately normalized before duplicate suppression.
	// This makes equal-content winner selection deterministic.
	out = rankDeterministic(out)
	seenContent := map[string]bool{}
	deduped := make([]SearchResult, 0, len(out))
	for _, result := range out {
		key := result.Item.ContentHash
		if key == "" {
			key = result.Item.ID
		}
		if !seenContent[key] {
			seenContent[key] = true
			deduped = append(deduped, result)
		}
	}
	return deduped
}

// applyMMR selects diverse results using Maximal Marginal Relevance. Lambda
// 0.75 balances fused relevance against lexical token overlap.
func applyMMR(candidates []SearchResult, lambda float64) []SearchResult {
	if len(candidates) < 2 {
		return candidates
	}
	remaining := append([]SearchResult(nil), candidates...)
	selected := []SearchResult{remaining[0]}
	remaining = remaining[1:]
	for len(remaining) > 0 {
		best := 0
		bestScore := -1.0
		for i, c := range remaining {
			penalty := 0.0
			for _, s := range selected {
				if sim := contentSimilarity(c.Item.Content, s.Item.Content); sim > penalty {
					penalty = sim
				}
			}
			score := lambda*c.Score - (1-lambda)*penalty
			if score > bestScore || (score == bestScore && c.Item.ID < remaining[best].Item.ID) {
				best, bestScore = i, score
			}
		}
		selected = append(selected, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return selected
}
func contentSimilarity(a, b string) float64 {
	tokens := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, t := range strings.Fields(strings.ToLower(s)) {
			if len(t) > 2 {
				m[t] = true
			}
		}
		return m
	}
	aa, bb := tokens(a), tokens(b)
	if len(aa) == 0 || len(bb) == 0 {
		return 0
	}
	inter := 0
	for t := range aa {
		if bb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(aa)+len(bb)-inter)
}
func mergeResults(base, add []SearchResult) []SearchResult {
	seen := map[string]bool{}
	for _, r := range base {
		seen[r.Item.ID] = true
	}
	for _, r := range add {
		if !seen[r.Item.ID] {
			seen[r.Item.ID] = true
			base = append(base, r)
		}
	}
	return base
}
func rankDeterministic(results []SearchResult) []SearchResult {
	return rankForScope(results, Scope{})
}

// rankForScope implements the §15.5 tie-break order. A smaller scope distance
// means the item is more specific to the request; a wider shared scope is
// preferred only after relevance and priority are equal.
func rankForScope(results []SearchResult, scope Scope) []SearchResult {
	out := append([]SearchResult(nil), results...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Item.Priority != out[j].Item.Priority {
			return out[i].Item.Priority > out[j].Item.Priority
		}
		if left, right := scopeDistance(scope, out[i].Item.Scope), scopeDistance(scope, out[j].Item.Scope); left != right {
			return left < right
		}
		if out[i].Item.Confidence != out[j].Item.Confidence {
			return out[i].Item.Confidence > out[j].Item.Confidence
		}
		if !out[i].Item.UpdatedAt.Equal(out[j].Item.UpdatedAt) {
			return out[i].Item.UpdatedAt.After(out[j].Item.UpdatedAt)
		}
		return out[i].Item.ID < out[j].Item.ID
	})
	return out
}

func scopeDistance(request, item Scope) int {
	if request.ProjectID != "" && request.ProjectID != item.ProjectID {
		return 1 << 20
	}
	distance := 0
	for _, level := range [][2]string{{request.TeamID, item.TeamID}, {request.SessionID, item.SessionID}, {request.AgentID, item.AgentID}, {request.TaskID, item.TaskID}, {request.AttemptID, item.AttemptID}} {
		if level[0] == "" {
			continue
		}
		if level[1] == "" {
			distance++
		} else if level[0] != level[1] {
			return 1 << 20
		}
	}
	return distance
}
func resultIDs(results []SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.Item.ID)
	}
	return ids
}
