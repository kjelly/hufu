package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/utils"
)

const ContextQueryPolicyVersion = "context-query-v2.1"

func contextQueryDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

const (
	queryConstraintRunes   = 384
	queryVerificationRunes = 256
	queryDependencyRunes   = 256
	queryDependencyCount   = 4
	queryPathCount         = 8
	queryPathRunes         = 128
	querySupplementalRunes = 1024
	queryTotalRunes        = 4096
)

type ContextQueryFallback string

const (
	QueryFallbackNone              ContextQueryFallback = "none"
	QueryFallbackNoHints           ContextQueryFallback = "no_hints"
	QueryFallbackNoMatches         ContextQueryFallback = "no_matches"
	QueryFallbackInputOverLimit    ContextQueryFallback = "input_over_limit"
	QueryFallbackTimeout           ContextQueryFallback = "supplemental_timeout"
	QueryFallbackSearchFailed      ContextQueryFallback = "supplemental_search_failed"
	QueryFallbackRevisionChanged   ContextQueryFallback = "source_revision_changed"
	QueryFallbackRestrictedLineage ContextQueryFallback = "restricted_lineage_legacy"
	QueryFallbackMemoryDisabled    ContextQueryFallback = "memory_disabled"
	QueryFallbackHistoryNotAllowed ContextQueryFallback = "history_not_allowed"
	QueryFallbackHintUnavailable   ContextQueryFallback = "hint_source_unavailable"
)

// ContextQueryHints contains already-authorized runtime data, not model claims.
// Builders cannot read a repository, resolve task prose or create permissions.
type ContextQueryHints struct {
	Dependencies   []ContextQueryDependencyHint
	Paths          []string
	PathSourceID   string
	PathSourceHash string
	Unavailable    bool
}

type ContextQueryDependencyHint struct{ TaskID, Summary string }

type ContextQueryProvenance struct {
	Kind        string `json:"kind"`
	SourceID    string `json:"source_id"`
	ContentHash string `json:"content_hash"`
}

// Raw queries/paths exist only in the invocation. JSON is a content-free
// projection suitable for attribution; PlanHash also binds the raw query hashes.
type ContextQueryPlan struct {
	PolicyVersion         string                   `json:"policy_version"`
	Primary               string                   `json:"-"`
	Supplemental          string                   `json:"-"`
	Paths                 []string                 `json:"-"`
	PrimaryHash           string                   `json:"primary_hash"`
	SupplementalHash      string                   `json:"supplemental_hash,omitempty"`
	Provenance            []ContextQueryProvenance `json:"provenance,omitempty"`
	Fallback              ContextQueryFallback     `json:"fallback"`
	TruncatedPaths        int                      `json:"truncated_paths,omitzero"`
	TruncatedDependencies int                      `json:"truncated_dependencies,omitzero"`
	TruncatedRunes        int                      `json:"truncated_runes,omitzero"`
	PlanHash              string                   `json:"plan_hash"`
}

func normalizedQueryHint(text string) string {
	return strings.Join(strings.Fields(utils.RedactSecrets(text)), " ")
}

// BuildContextQueryPlan preserves the v1 primary and request identity exactly.
func BuildContextQueryPlan(request ContextRequest, hints ContextQueryHints) ContextQueryPlan {
	plan := ContextQueryPlan{PolicyVersion: ContextQueryPolicyVersion, Primary: request.RetrievalQuery(), Fallback: QueryFallbackNone}
	plan.PrimaryHash = contextQueryDigest(plan.Primary)
	remaining := min(querySupplementalRunes, max(0, queryTotalRunes-utf8.RuneCountInString(plan.Primary)))
	if utf8.RuneCountInString(plan.Primary) > queryTotalRunes {
		plan.Fallback = QueryFallbackInputOverLimit
		return finishQueryPlan(plan)
	}
	seen := make(map[string]bool)
	appendHint := func(kind, id, text string, limit int, whole bool) bool {
		text = normalizedQueryHint(text)
		if text == "" || seen[text] {
			return false
		}
		length := utf8.RuneCountInString(text)
		if whole && length > limit {
			plan.TruncatedPaths++
			return false
		}
		separator := 0
		if plan.Supplemental != "" {
			separator = 1
		}
		available := min(limit, max(0, remaining-separator))
		if whole && length > available {
			plan.TruncatedPaths++
			return false
		}
		if available == 0 {
			plan.TruncatedRunes += length
			return false
		}
		if length > available {
			plan.TruncatedRunes += length - available
			text = string([]rune(text)[:available])
		}
		if seen[text] {
			return false
		}
		seen[text] = true
		if separator > 0 {
			plan.Supplemental += "\n"
		}
		plan.Supplemental += text
		remaining -= separator + utf8.RuneCountInString(text)
		plan.Provenance = append(plan.Provenance, ContextQueryProvenance{Kind: kind, SourceID: id, ContentHash: contextQueryDigest(text)})
		return true
	}
	appendHint("constraints", request.TaskID, request.Constraints, queryConstraintRunes, false)
	appendHint("verification", request.TaskID, request.VerificationCriteria, queryVerificationRunes, false)
	dependencyRemaining := queryDependencyRunes
	for index, dependency := range hints.Dependencies {
		if index >= queryDependencyCount {
			plan.TruncatedDependencies++
			continue
		}
		before := utf8.RuneCountInString(plan.Supplemental)
		if appendHint("dependency_summary", dependency.TaskID, dependency.Summary, dependencyRemaining, false) {
			used := utf8.RuneCountInString(plan.Supplemental) - before
			if before > 0 {
				used--
			}
			dependencyRemaining -= used
		}
	}
	paths := slices.Clone(hints.Paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	for _, path := range paths {
		if len(plan.Paths) >= queryPathCount {
			plan.TruncatedPaths++
			continue
		}
		normalized, err := NormalizeTouchedPath(path)
		if err != nil {
			plan.TruncatedPaths++
			continue
		}
		if appendHint("workset_path", hints.PathSourceID, normalized, queryPathRunes, true) {
			plan.Paths = append(plan.Paths, normalized)
		}
	}
	if plan.Supplemental == "" {
		plan.Fallback = QueryFallbackNoHints
		if hints.Unavailable {
			plan.Fallback = QueryFallbackHintUnavailable
		}
	}
	if hints.PathSourceHash != "" && len(plan.Paths) > 0 {
		plan.Provenance = append(plan.Provenance, ContextQueryProvenance{Kind: "workset_source", SourceID: hints.PathSourceID, ContentHash: hints.PathSourceHash})
	}
	return finishQueryPlan(plan)
}

func finishQueryPlan(plan ContextQueryPlan) ContextQueryPlan {
	if plan.Supplemental != "" {
		plan.SupplementalHash = contextQueryDigest(plan.Supplemental)
	}
	plan.PlanHash = ""
	data, err := json.Marshal(plan, json.Deterministic(true))
	if err != nil {
		panic(err)
	} // Fixed scalar/slice projection cannot fail serialization.
	plan.PlanHash = contextQueryDigest(string(data))
	return plan
}

// resolveContextQueryHints only reads durable accepted sources. An unavailable
// optional source produces no hint; required evidence still uses its own gate.
func (c *Coordinator) resolveContextQueryHints(ctx context.Context, request ContextRequest) ContextQueryHints {
	var hints ContextQueryHints
	item := c.todoItemByID(request.TaskID)
	if item == nil || !c.isDurableTaskOccurrence(item.ID) {
		return hints
	}
	allowed := resultDependencyIDs(item)
	for _, source := range c.taskTracker.TodoList().Items() {
		if source == nil || !slices.Contains(allowed, source.ID) {
			continue
		}
		if len(hints.Dependencies) >= queryDependencyCount {
			break
		}
		result := source.TypedResult
		receipt := source.ExecutionReceipt
		if source.Status != TaskDone || result == nil || !taskResultStatusIsSuccessful(result.Status) || result.TaskID != source.ID ||
			receipt == nil || receipt.RunID != request.RunID || receipt.TaskID != source.ID || receipt.Attempt != result.Attempt ||
			c.validateAcceptedDependencyPayload(source) != nil {
			hints.Unavailable = true
			continue
		}
		projected := projectDependencyResultForWorker(result, item.WorksetBinding)
		hints.Dependencies = append(hints.Dependencies, ContextQueryDependencyHint{TaskID: source.ID, Summary: projected.Summary})
	}
	if item.WorksetBinding == nil {
		return hints
	}
	binding := item.WorksetBinding
	var visible []*WorksetExpansionReceipt
	for _, task := range c.taskTracker.TodoList().Items() {
		if task != nil && task.WorksetReceipt != nil {
			visible = append(visible, task.WorksetReceipt)
		}
	}
	receipts, conflicts := collectWorksetReceipts(visible)
	receipt := receipts[binding.WorksetID]
	_, conflicted := conflicts[binding.WorksetID]
	if conflicted || receipt == nil || receipt.Children[binding.ItemKey] != item.ID || receipt.ParentTaskID != binding.ParentTaskID || receipt.SourceArtifactID != binding.SourceArtifactID || receipt.SourceSHA256 != binding.SourceSHA256 || !sameArtifactOccurrence(receipt.SourceArtifact, binding.SourceArtifact) || c.verifyWorksetSource(ctx, receipt) != nil {
		hints.Unavailable = true
		return hints
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		hints.Unavailable = true
		return hints
	}
	ref, err := resolveWorksetArtifactContent(ctx, store, receipt.SourceArtifact)
	if err != nil {
		hints.Unavailable = true
		return hints
	}
	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		hints.Unavailable = true
		return hints
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxWorksetBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(data) > maxWorksetBytes {
		hints.Unavailable = true
		return hints
	}
	manifest, err := decodeWorksetManifest(data)
	if err != nil {
		hints.Unavailable = true
		return hints
	}
	for _, source := range manifest.Items {
		if source.Key != binding.ItemKey {
			continue
		}
		if !slices.Equal(source.TouchedPaths, binding.TouchedPaths) {
			hints.Unavailable = true
			return hints
		}
		hints.Paths = slices.Clone(source.TouchedPaths)
		hints.PathSourceID = binding.WorksetID + ":" + binding.ItemKey
		hints.PathSourceHash = binding.SourceSHA256
		return hints
	}
	hints.Unavailable = true
	return hints
}
