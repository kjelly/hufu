package team

import (
	"cmp"
	"context"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

const contextQuerySupplementalTimeout = 100 * time.Millisecond

type ContextQuerySourceKind string

const (
	QuerySharedPersistent  ContextQuerySourceKind = "shared_persistent"
	QueryPrivateSession    ContextQuerySourceKind = "private_session"
	QueryPrivatePersistent ContextQuerySourceKind = "private_persistent"
)

type ContextQueryMode string

const (
	QueryModeLegacy        ContextQueryMode = "legacy"
	QueryModeActive        ContextQueryMode = "active"
	QueryModeMixed         ContextQueryMode = "mixed"
	QueryModeNotApplicable ContextQueryMode = "not_applicable"
)

// ContextQuerySource is an immutable authorized primary snapshot. Eligible is
// the canonical, lifecycle/activation-checked universe, never model-authored.
type ContextQuerySource struct {
	Kind              ContextQuerySourceKind
	Request           contextstore.SearchRequest
	Primary           []contextstore.SearchResult
	Eligible          map[string]contextstore.ContextItem
	Revision          string
	Disabled          ContextQueryFallback
	RestrictedLineage bool
	Unavailable       bool
}

type ContextQuerySourceAttribution struct {
	Source           ContextQuerySourceKind `json:"source"`
	Mode             ContextQueryMode       `json:"mode"`
	Fallback         ContextQueryFallback   `json:"fallback"`
	PrimaryHash      string                 `json:"primary_hash"`
	SupplementalHash string                 `json:"supplemental_hash,omitempty"`
	TruncatedTerms   int                    `json:"truncated_terms,omitzero"`
}

type ContextQueryAttribution struct {
	PolicyVersion string                          `json:"policy_version"`
	PlanHash      string                          `json:"plan_hash"`
	Mode          ContextQueryMode                `json:"mode"`
	Sources       []ContextQuerySourceAttribution `json:"sources"`
}

type ContextQuerySourceSelection struct {
	Source  ContextQuerySourceKind
	Results []contextstore.SearchResult
	Work    contextstore.QueryWork
}

type ContextQuerySelection struct {
	Sources       []ContextQuerySourceSelection
	Attribution   ContextQueryAttribution
	CompositeHash string
}

// SelectContextQuerySources stages all new scopes under ONE child deadline.
// An I/O/revision failure discards every staged supplement, leaving immutable
// primary snapshots intact. Outer cancellation never permits model fallback.
func SelectContextQuerySources(ctx context.Context, repo contextstore.RetrievalRepository, plan ContextQueryPlan, sources []ContextQuerySource, revision func(context.Context, ContextQuerySource) (string, error)) (ContextQuerySelection, error) {
	child, cancel := context.WithTimeout(ctx, contextQuerySupplementalTimeout)
	defer cancel()
	return selectContextQuerySources(ctx, child, repo, plan, sources, revision)
}

func selectContextQuerySources(ctx, child context.Context, repo contextstore.RetrievalRepository, plan ContextQueryPlan, sources []ContextQuerySource, revision func(context.Context, ContextQuerySource) (string, error)) (ContextQuerySelection, error) {
	out, err := initializeQuerySelection(plan, sources)
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if revision == nil {
		for index := range out.Attribution.Sources {
			attribution := &out.Attribution.Sources[index]
			if attribution.Mode == QueryModeLegacy && attribution.Fallback == QueryFallbackNone {
				attribution.Fallback = QueryFallbackHintUnavailable
			}
		}
		return finishQuerySelection(out), nil
	}
	rollback := func(reason ContextQueryFallback) ContextQuerySelection {
		for index, source := range sources {
			out.Sources[index].Results = contextstore.CloneSearchResults(source.Primary)
			attribution := &out.Attribution.Sources[index]
			if attribution.Mode != QueryModeNotApplicable && (attribution.Fallback == QueryFallbackNone || attribution.Fallback == QueryFallbackNoMatches) {
				attribution.Mode = QueryModeLegacy
				attribution.Fallback = reason
				attribution.SupplementalHash = ""
			}
		}
		return finishQuerySelection(out)
	}
	for index, source := range sources {
		attribution := &out.Attribution.Sources[index]
		if attribution.Mode == QueryModeNotApplicable || attribution.Fallback != QueryFallbackNone {
			continue
		}
		if revision != nil {
			current, err := revision(child, source)
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if err != nil {
				return rollback(querySupplementalFailure(child, err)), nil
			}
			if current != source.Revision {
				return rollback(QueryFallbackRevisionChanged), nil
			}
		}
		request := source.Request
		request.Query = plan.Supplemental
		request.FilePaths = slices.Clone(plan.Paths)
		// A non-nil empty allow-set intentionally permits nothing.
		request = request.WithAllowedItemIDs(slices.Sorted(maps.Keys(source.Eligible)))
		if request.AllowedItemIDs == nil {
			request.AllowedItemIDs = []string{}
		}
		request.Limit = min(contextstore.SupplementalMaxSourceRows, max(1, source.Request.Limit))
		supplemental, err := contextstore.RetrieveSupplemental(child, repo, request)
		out.Sources[index].Work = supplemental.Work
		attribution.TruncatedTerms = supplemental.TruncatedTerms
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if err != nil {
			return rollback(querySupplementalFailure(child, err)), nil
		}
		if revision != nil {
			current, err := revision(child, source)
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if err != nil {
				return rollback(querySupplementalFailure(child, err)), nil
			}
			if current != source.Revision {
				return rollback(QueryFallbackRevisionChanged), nil
			}
		}
		if child.Err() != nil {
			return rollback(QueryFallbackTimeout), nil
		}
		for resultIndex, row := range supplemental.Results {
			canonical, ok := source.Eligible[row.Item.ID]
			if !ok || canonical.ContentHash != row.Item.ContentHash || !canonical.UpdatedAt.Equal(row.Item.UpdatedAt) || canonical.Scope != row.Item.Scope {
				return rollback(QueryFallbackRevisionChanged), nil
			}
			supplemental.Results[resultIndex].Item = canonical
		}
		if len(supplemental.Results) == 0 {
			attribution.Fallback = QueryFallbackNoMatches
			continue
		}
		out.Sources[index].Results = MergeContextQueryLanes(source.Primary, supplemental.Results, source.Request.Scope, source.Request.Limit)
		attribution.Mode = QueryModeActive
		attribution.SupplementalHash = plan.SupplementalHash
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if child.Err() != nil {
		return rollback(QueryFallbackTimeout), nil
	}
	return finishQuerySelection(out), nil
}

// initializeQuerySelection validates the authorized source topology and records
// sources that must retain legacy before any supplemental I/O is attempted.
func initializeQuerySelection(plan ContextQueryPlan, sources []ContextQuerySource) (ContextQuerySelection, error) {
	out := ContextQuerySelection{Attribution: ContextQueryAttribution{PolicyVersion: plan.PolicyVersion, PlanHash: plan.PlanHash, Mode: QueryModeLegacy}}
	if len(sources) > 3 {
		return out, fmt.Errorf("query plan allows at most three supplemental scopes")
	}
	seen := make(map[ContextQuerySourceKind]bool)
	privateRestricted := slices.ContainsFunc(sources, func(source ContextQuerySource) bool {
		return source.Kind != QuerySharedPersistent && source.RestrictedLineage
	})
	for _, source := range sources {
		if !validQuerySource(source.Kind) || seen[source.Kind] {
			return out, fmt.Errorf("invalid or duplicate query source %q", source.Kind)
		}
		seen[source.Kind] = true
		if source.Request.Limit <= 0 || source.Request.Scope.ProjectID == "" {
			return out, fmt.Errorf("query source requires an admitted scope and positive candidate limit")
		}
		if source.Disabled != "" && source.Disabled != QueryFallbackNone && source.Disabled != QueryFallbackMemoryDisabled && source.Disabled != QueryFallbackHistoryNotAllowed {
			return out, fmt.Errorf("invalid disabled query source")
		}
		out.Sources = append(out.Sources, ContextQuerySourceSelection{Source: source.Kind, Results: contextstore.CloneSearchResults(source.Primary)})
		attribution := ContextQuerySourceAttribution{Source: source.Kind, Mode: QueryModeLegacy, Fallback: QueryFallbackNone, PrimaryHash: plan.PrimaryHash}
		switch {
		case source.Disabled != "" && source.Disabled != QueryFallbackNone:
			attribution.Mode, attribution.Fallback = QueryModeNotApplicable, source.Disabled
		case source.Unavailable:
			attribution.Fallback = QueryFallbackHintUnavailable
		case privateRestricted && source.Kind != QuerySharedPersistent:
			attribution.Fallback = QueryFallbackRestrictedLineage
		case plan.Fallback != QueryFallbackNone:
			attribution.Fallback = plan.Fallback
		case plan.Supplemental == "":
			attribution.Fallback = QueryFallbackNoHints
		}
		out.Attribution.Sources = append(out.Attribution.Sources, attribution)
	}
	return out, nil
}

func querySupplementalFailure(ctx context.Context, err error) ContextQueryFallback {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return QueryFallbackTimeout
	}
	return QueryFallbackSearchFailed
}

func validQuerySource(source ContextQuerySourceKind) bool {
	return source == QuerySharedPersistent || source == QueryPrivateSession || source == QueryPrivatePersistent
}

// MergeContextQueryLanes uses the fixed v2.1 rank policy; input scores and
// metadata are not mutated. The returned unit scores must not be multiplied61.
func MergeContextQueryLanes(primary, supplemental []contextstore.SearchResult, scope contextstore.Scope, topK int) []contextstore.SearchResult {
	if topK <= 0 {
		return nil
	}
	laneLimit := min(topK, contextstore.SupplementalMaxSourceRows)
	merged := make(map[string]contextstore.SearchResult)
	for lane, results := range [][]contextstore.SearchResult{contextstore.CloneSearchResults(primary), contextstore.CloneSearchResults(supplemental)} {
		weight := .6
		if lane == 1 {
			weight = .4
		}
		if len(results) > laneLimit {
			results = results[:laneLimit]
		}
		for rank, result := range results {
			current, found := merged[result.Item.ID]
			if !found {
				current = result
				current.Score = 0
			}
			current.Score += 5 * weight / float64(5+rank)
			merged[result.Item.ID] = current
		}
	}
	out := slices.Collect(maps.Values(merged))
	slices.SortFunc(out, func(a, b contextstore.SearchResult) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(memoryScopeDistance(a.Item.Scope, scope), memoryScopeDistance(b.Item.Scope, scope)), cmp.Compare(b.Item.Priority, a.Item.Priority), cmp.Compare(b.Item.Confidence, a.Item.Confidence), b.Item.UpdatedAt.Compare(a.Item.UpdatedAt), strings.Compare(a.Item.ID, b.Item.ID))
	})
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func finishQuerySelection(out ContextQuerySelection) ContextQuerySelection {
	active, legacy := false, false
	for _, source := range out.Attribution.Sources {
		active = active || source.Mode == QueryModeActive
		legacy = legacy || source.Mode == QueryModeLegacy
	}
	if active {
		out.Attribution.Mode = QueryModeActive
		if legacy {
			out.Attribution.Mode = QueryModeMixed
		}
	}
	slices.SortFunc(out.Attribution.Sources, func(a, b ContextQuerySourceAttribution) int {
		return strings.Compare(string(a.Source), string(b.Source))
	})
	out.CompositeHash = hashQueryAttribution(out.Attribution)
	return out
}

func hashQueryAttribution(attribution ContextQueryAttribution) string {
	copyAttribution := attribution
	copyAttribution.Sources = slices.Clone(attribution.Sources)
	slices.SortFunc(copyAttribution.Sources, func(a, b ContextQuerySourceAttribution) int {
		return strings.Compare(string(a.Source), string(b.Source))
	})
	data, err := json.Marshal(copyAttribution, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return contextQueryDigest(string(data))
}

func ValidateContextQueryAttribution(attribution ContextQueryAttribution) error {
	if attribution.PolicyVersion != ContextQueryPolicyVersion || !validQueryDigest(attribution.PlanHash) || len(attribution.Sources) > 3 {
		return fmt.Errorf("invalid query policy, plan hash or source count")
	}
	seen := map[ContextQuerySourceKind]bool{}
	active, legacy := false, false
	for _, source := range attribution.Sources {
		if !validQuerySource(source.Source) || seen[source.Source] || !validQueryDigest(source.PrimaryHash) || source.TruncatedTerms < 0 {
			return fmt.Errorf("invalid query source attribution")
		}
		seen[source.Source] = true
		if !slices.Contains([]ContextQueryFallback{QueryFallbackNone, QueryFallbackNoHints, QueryFallbackNoMatches, QueryFallbackInputOverLimit, QueryFallbackTimeout, QueryFallbackSearchFailed, QueryFallbackRevisionChanged, QueryFallbackRestrictedLineage, QueryFallbackMemoryDisabled, QueryFallbackHistoryNotAllowed, QueryFallbackHintUnavailable}, source.Fallback) {
			return fmt.Errorf("invalid query fallback %q", source.Fallback)
		}
		switch source.Mode {
		case QueryModeActive:
			active = true
			if source.Fallback != QueryFallbackNone || !validQueryDigest(source.SupplementalHash) {
				return fmt.Errorf("active query source has fallback or no query hash")
			}
		case QueryModeLegacy:
			legacy = true
			if source.SupplementalHash != "" {
				return fmt.Errorf("legacy query source claims an executed supplement")
			}
		case QueryModeNotApplicable:
			if source.SupplementalHash != "" || (source.Fallback != QueryFallbackMemoryDisabled && source.Fallback != QueryFallbackHistoryNotAllowed) {
				return fmt.Errorf("invalid disabled query source")
			}
		default:
			return fmt.Errorf("invalid query source mode %q", source.Mode)
		}
	}
	want := QueryModeLegacy
	if active {
		want = QueryModeActive
		if legacy {
			want = QueryModeMixed
		}
	}
	if attribution.Mode != want {
		return fmt.Errorf("overall query mode does not match its sources")
	}
	return nil
}

func validQueryDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
