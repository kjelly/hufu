// reviewprep is a team-owned, deterministic workset producer.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

const manifestSchemaVersion = 2

const (
	defaultMaxTotalDiffBytes = 1_048_576
	defaultMaxTotalDiffLines = 20_000
	defaultMaxChangedPaths   = 256
	defaultMaxWorksetItems   = 64
)

type actionRequest struct {
	Capability string `json:"capability,omitempty"`
	Type       string `json:"type"`
	Payload    string `json:"payload"`
}

type resolverScope struct {
	Kind    string `json:"kind"`
	Count   int    `json:"count,omitempty"`
	History string `json:"history"`
	Head    string `json:"head"`
	Base    string `json:"base,omitempty"`
	Since   string `json:"since,omitempty"`
}

type scopeCandidate struct {
	value    resolverScope
	evidence team.RunInputResolverEvidence
}

var (
	lastCommitsPattern  = regexp.MustCompile(`(?i)\blast[ \t]+([0-9]+)[ \t]+commits?\b`)
	lastDaysPattern     = regexp.MustCompile(`(?i)\blast[ \t]+([0-9]+)[ \t]+days?\b`)
	recentDaysPattern   = regexp.MustCompile(`最近[ \t]*([0-9]+)[ \t]*天`)
	headRangePattern    = regexp.MustCompile(`(?i)\bHEAD~([0-9]+)\.\.HEAD\b`)
	revisionPattern     = regexp.MustCompile(`(?i)([A-Za-z0-9][A-Za-z0-9._/-]{0,159})\.\.([A-Za-z0-9][A-Za-z0-9._/-]{0,159})`)
	revisionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^{}-]{0,159}$`)
	sincePattern        = regexp.MustCompile(`(?i)\bsince[ \t]+([0-9]{4}-[0-9]{2}-[0-9]{2})\b`)
)

// Config intentionally contains only team-owned workset semantics. Hufu
// passes it as an opaque Action payload and does not interpret these fields.
type Config struct {
	Repository   string        `json:"repository"`
	OutputDir    string        `json:"output_dir"`
	ArtifactRoot string        `json:"artifact_root"`
	Scope        resolverScope `json:"scope"`
	// Since and MaxCommits are retained for direct Go callers during the
	// provider payload migration. New action requests use Scope exclusively.
	Since             string `json:"since"`
	MaxCommits        int    `json:"max_commits"`
	MaxTotalDiffBytes int    `json:"max_total_diff_bytes"`
	MaxTotalDiffLines int    `json:"max_total_diff_lines"`
	MaxChangedPaths   int    `json:"max_changed_paths"`
	MaxWorksetItems   int    `json:"max_workset_items"`
	MaxDiffBytes      int    `json:"max_diff_bytes"`
	MaxDiffLines      int    `json:"max_diff_lines"`
	MaxPaths          int    `json:"max_paths"`
}

type wireConfig struct {
	Repository        string         `json:"repository"`
	OutputDir         string         `json:"output_dir"`
	ArtifactRoot      string         `json:"artifact_root"`
	Scope             *resolverScope `json:"scope,omitempty"`
	Since             string         `json:"since,omitempty"`
	MaxCommits        string         `json:"max_commits,omitempty"`
	MaxTotalDiffBytes int            `json:"max_total_diff_bytes"`
	MaxTotalDiffLines int            `json:"max_total_diff_lines"`
	MaxChangedPaths   int            `json:"max_changed_paths"`
	MaxWorksetItems   int            `json:"max_workset_items"`
	MaxDiffBytes      int            `json:"max_diff_bytes"`
	MaxDiffLines      int            `json:"max_diff_lines"`
	MaxPaths          int            `json:"max_paths"`
}

type actionResult struct {
	Outputs   map[string]any `json:"outputs"`
	Artifacts []artifact     `json:"artifacts"`
}

type artifact struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
}

type manifest struct {
	SchemaVersion int              `json:"schema_version"`
	Scope         scopeAttestation `json:"scope"`
	Observed      observedBudget   `json:"observed_budget"`
	Range         reviewRange      `json:"range"`
	ChangedFiles  int              `json:"changed_files"`
	Items         []item           `json:"items"`
}

type requestedScope = resolverScope

type resolvedScope struct {
	Base                 string `json:"base"`
	Head                 string `json:"head"`
	SelectedCommitCount  int    `json:"selected_commit_count"`
	AvailableCommitCount int    `json:"available_commit_count"`
	HistoryExhausted     bool   `json:"history_exhausted"`
	RepositoryShallow    bool   `json:"repository_shallow"`
}

type scopeAttestation struct {
	Requested          requestedScope `json:"requested"`
	RequestedInputHash string         `json:"requested_input_hash"`
	Resolved           resolvedScope  `json:"resolved"`
	ObservedBudget     observedBudget `json:"observed_budget"`
	Satisfied          bool           `json:"satisfied"`
}

type observedBudget struct {
	TotalDiffBytes int `json:"total_diff_bytes"`
	TotalDiffLines int `json:"total_diff_lines"`
	ChangedPaths   int `json:"changed_paths"`
	WorksetItems   int `json:"workset_items"`
}

type rangeResolution struct {
	Range                reviewRange
	AvailableCommitCount int
	HistoryExhausted     bool
	RepositoryShallow    bool
}

type reviewRange struct {
	Start       string `json:"start"`
	End         string `json:"end"`
	Since       string `json:"since"`
	CommitCount int    `json:"commit_count"`
}

type item struct {
	Key          string            `json:"key"`
	Lens         string            `json:"lens"`
	Bindings     map[string]string `json:"bindings"`
	TouchedPaths []string          `json:"touched_paths"`
	Inputs       []artifact        `json:"inputs,omitempty"`
	DiffPath     string            `json:"diff_path"`
	DiffSHA      string            `json:"diff_sha256"`
	DiffBytes    int               `json:"diff_bytes"`
	DiffLines    int               `json:"diff_lines"`
}

type batch struct {
	lens     string
	paths    []string
	pathSeen map[string]struct{}
	diff     bytes.Buffer
	lines    int
}

func main() {
	if err := run(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "reviewprep:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, in io.Reader, out io.Writer) error {
	decoder := json.NewDecoder(io.LimitReader(in, maxRunInputRequestBytes+1))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode action request: %w", err)
	}
	if len(raw) > maxRunInputRequestBytes {
		return fmt.Errorf("request exceeds %d bytes", maxRunInputRequestBytes)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode action request: %w", err)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode request type: %w", err)
	}
	if envelope.Type == "resolve_run_input" {
		var request team.RunInputResolverRequest
		if err := decodeStrictJSON(raw, &request); err != nil {
			return fmt.Errorf("decode resolver request: %w", err)
		}
		response := resolveReviewScopeInput(request)
		return json.NewEncoder(out).Encode(response)
	}
	var request actionRequest
	if err := decodeStrictJSON(raw, &request); err != nil {
		return fmt.Errorf("decode action request: %w", err)
	}
	if request.Type != "prepare_review_workset" && request.Type != "prepare" {
		return fmt.Errorf("unsupported action type %q", request.Type)
	}
	config, err := decodeWireConfig(request.Payload)
	if err != nil {
		return fmt.Errorf("decode action payload: %w", err)
	}
	result, err := Prepare(ctx, config)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

const maxRunInputRequestBytes = 512 * 1024

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func resolveReviewScopeInput(request team.RunInputResolverRequest) team.RunInputResolverResponse {
	return resolveReviewScopeInputAt(request, time.Now().UTC())
}

func resolveReviewScopeInputAt(request team.RunInputResolverRequest, now time.Time) team.RunInputResolverResponse {
	response := team.RunInputResolverResponse{ResolverVersion: "2"}
	if request.Type != "resolve_run_input" || request.InputName != "review.scope" || request.ResolverID != "review-scope-v1" {
		response.Status = "invalid"
		response.Diagnostic = "resolver request identity is unsupported"
		return response
	}
	if len(request.ExplicitValue) > 0 && string(request.ExplicitValue) != "null" {
		var explicit resolverScope
		if err := decodeStrictJSON(request.ExplicitValue, &explicit); err != nil {
			response.Status = "invalid"
			response.Diagnostic = "explicit review scope is not a valid scope object"
			return response
		}
		if err := validateRequestedScope(explicit); err != nil {
			response.Status = "invalid"
			response.Diagnostic = err.Error()
			return response
		}
	}
	candidates, invalid := parseScopeCandidates(request.Prompt, now)
	if invalid != "" {
		response.Status = "invalid"
		response.Diagnostic = invalid
		return response
	}
	if len(candidates) == 0 {
		response.Status = "no_match"
		return response
	}
	byValue := make(map[string][]team.RunInputResolverEvidence)
	for _, candidate := range candidates {
		encoded, err := json.Marshal(candidate.value)
		if err != nil {
			response.Status = "invalid"
			response.Diagnostic = "scope candidate could not be encoded"
			return response
		}
		byValue[string(encoded)] = append(byValue[string(encoded)], candidate.evidence)
	}
	if len(byValue) != 1 {
		response.Status = "ambiguous"
		response.Diagnostic = "prompt contains incompatible review scope expressions"
		return response
	}
	for value, evidence := range byValue {
		response.Status = "matched"
		response.Value = json.RawMessage(value)
		response.Evidence = evidence
	}
	return response
}

func parseScopeCandidates(prompt string, now time.Time) ([]scopeCandidate, string) {
	candidates := make([]scopeCandidate, 0)
	headRanges := headRangePattern.FindAllStringSubmatchIndex(prompt, -1)
	for _, match := range lastCommitsPattern.FindAllStringSubmatchIndex(prompt, -1) {
		count, err := parseScopeCount(prompt[match[2]:match[3]])
		if err != nil {
			return nil, err.Error()
		}
		candidates = append(candidates, lastNScopeCandidate(count, match[0], match[1], "last_n_commits"))
	}
	for _, match := range headRanges {
		count, err := parseScopeCount(prompt[match[2]:match[3]])
		if err != nil {
			return nil, err.Error()
		}
		candidates = append(candidates, lastNScopeCandidate(count, match[0], match[1], "head_relative_range"))
	}
	for _, pattern := range []struct {
		expression *regexp.Regexp
		kind       string
	}{
		{expression: lastDaysPattern, kind: "last_days"},
		{expression: recentDaysPattern, kind: "recent_days_zh"},
	} {
		for _, match := range pattern.expression.FindAllStringSubmatchIndex(prompt, -1) {
			days, err := parseRelativeDayCount(prompt[match[2]:match[3]])
			if err != nil {
				return nil, err.Error()
			}
			date := now.UTC().AddDate(0, 0, -days).Format(time.DateOnly)
			candidates = append(candidates, scopeCandidate{
				value:    resolverScope{Kind: "since", History: "first_parent", Head: "HEAD", Since: date},
				evidence: team.RunInputResolverEvidence{Source: "prompt", Start: match[0], End: match[1], Kind: pattern.kind},
			})
		}
	}
	for _, match := range revisionPattern.FindAllStringSubmatchIndex(prompt, -1) {
		if overlapsAny(match[0], match[1], headRanges) {
			continue
		}
		candidates = append(candidates, scopeCandidate{
			value:    resolverScope{Kind: "revision_range", History: "first_parent", Base: prompt[match[2]:match[3]], Head: prompt[match[4]:match[5]]},
			evidence: team.RunInputResolverEvidence{Source: "prompt", Start: match[0], End: match[1], Kind: "revision_range"},
		})
	}
	for _, match := range sincePattern.FindAllStringSubmatchIndex(prompt, -1) {
		date := prompt[match[2]:match[3]]
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return nil, "since date must use a valid YYYY-MM-DD calendar date"
		}
		candidates = append(candidates, scopeCandidate{
			value:    resolverScope{Kind: "since", History: "first_parent", Head: "HEAD", Since: date},
			evidence: team.RunInputResolverEvidence{Source: "prompt", Start: match[0], End: match[1], Kind: "since_date"},
		})
	}
	return candidates, ""
}

func parseRelativeDayCount(value string) (int, error) {
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > 366 {
		return 0, errors.New("relative review day count must be a base-10 integer between 1 and 366")
	}
	return days, nil
}

func parseScopeCount(value string) (int, error) {
	count, err := strconv.Atoi(value)
	if err != nil || count < 1 || count > 100 {
		return 0, fmt.Errorf("review commit count must be a base-10 integer between 1 and 100")
	}
	return count, nil
}

func lastNScopeCandidate(count, start, end int, kind string) scopeCandidate {
	return scopeCandidate{
		value:    resolverScope{Kind: "last_n", Count: count, History: "first_parent", Head: "HEAD"},
		evidence: team.RunInputResolverEvidence{Source: "prompt", Start: start, End: end, Kind: kind},
	}
}

func overlapsAny(start, end int, ranges [][]int) bool {
	for _, match := range ranges {
		if start < match[1] && end > match[0] {
			return true
		}
	}
	return false
}

func decodeWireConfig(payload string) (Config, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var wire wireConfig
	if err := decoder.Decode(&wire); err != nil {
		return Config{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Config{}, err
	}
	if wire.Scope != nil && strings.TrimSpace(wire.MaxCommits) != "" {
		return Config{}, errors.New("scope and deprecated max_commits cannot be combined")
	}
	var scope resolverScope
	maxCommits := 0
	if wire.Scope != nil {
		scope = *wire.Scope
	} else if strings.TrimSpace(wire.MaxCommits) != "" {
		parsed, err := strconv.Atoi(wire.MaxCommits)
		if err != nil {
			return Config{}, fmt.Errorf("max_commits must be a base-10 integer string between 1 and 100: %w", err)
		}
		maxCommits = parsed
		scope = lastNScopeCandidate(parsed, 0, 0, "compatibility").value
	}
	if scope.Kind != "" {
		if err := validateRequestedScope(scope); err != nil {
			return Config{}, err
		}
		if scope.Kind == "last_n" {
			maxCommits = scope.Count
		}
	}
	config := Config{
		Repository: wire.Repository, OutputDir: wire.OutputDir, ArtifactRoot: wire.ArtifactRoot,
		Scope: scope, Since: wire.Since, MaxCommits: maxCommits,
		MaxTotalDiffBytes: wire.MaxTotalDiffBytes, MaxTotalDiffLines: wire.MaxTotalDiffLines,
		MaxChangedPaths: wire.MaxChangedPaths, MaxWorksetItems: wire.MaxWorksetItems,
		MaxDiffBytes: wire.MaxDiffBytes, MaxDiffLines: wire.MaxDiffLines, MaxPaths: wire.MaxPaths,
	}
	return config, nil
}

func validateRequestedScope(scope resolverScope) error {
	if scope.History != "first_parent" {
		return fmt.Errorf("review scope history must be first_parent")
	}
	if !validRevision(scope.Head) {
		return fmt.Errorf("review scope head %q is invalid", scope.Head)
	}
	switch scope.Kind {
	case "last_n":
		if err := validateMaxCommits(scope.Count); err != nil {
			return err
		}
		if scope.Base != "" || scope.Since != "" {
			return errors.New("last_n review scope cannot set base or since")
		}
	case "revision_range":
		if scope.Count != 0 || scope.Since != "" || !validRevision(scope.Base) {
			return errors.New("revision_range review scope requires a valid base and cannot set count or since")
		}
	case "since":
		if scope.Count != 0 || scope.Base != "" {
			return errors.New("since review scope cannot set count or base")
		}
		if _, err := time.Parse(time.DateOnly, scope.Since); err != nil {
			return errors.New("since review scope requires a valid YYYY-MM-DD date")
		}
	default:
		return fmt.Errorf("unsupported review scope kind %q", scope.Kind)
	}
	return nil
}

func validRevision(value string) bool {
	return revisionNamePattern.MatchString(value)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value is not allowed")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

// Prepare materialises a deterministic, bounded review workset. It never
// reads the working tree: every path and patch comes from the selected commit
// range. OutputDir must be new or empty to avoid replacing another run's data.
func Prepare(ctx context.Context, config Config) (actionResult, error) {
	applyConfigDefaults(&config)
	if err := validateConfig(config); err != nil {
		return actionResult{}, err
	}
	repo, err := resolveRepository(ctx, config.Repository)
	if err != nil {
		return actionResult{}, err
	}
	artifactRoot, err := resolveArtifactRoot(repo, config.ArtifactRoot)
	if err != nil {
		return actionResult{}, err
	}
	outputDir, err := resolveOutputDir(repo, config.OutputDir)
	if err != nil {
		return actionResult{}, err
	}
	if err := ensureEmptyOutputDir(outputDir); err != nil {
		return actionResult{}, err
	}
	if !pathWithin(artifactRoot, outputDir) {
		return actionResult{}, fmt.Errorf("output_dir %q must be beneath artifact_root %q", config.OutputDir, config.ArtifactRoot)
	}

	resolution, err := resolveRequestedRange(ctx, repo, config)
	if err != nil {
		return actionResult{}, err
	}
	reviewRangeValue := resolution.Range
	if reviewRangeValue.CommitCount == 0 {
		return actionResult{}, fmt.Errorf("scope_empty: requested review scope %s selected no commits", compactScope(config.Scope))
	}
	paths, err := changedPaths(ctx, repo, reviewRangeValue)
	if err != nil {
		return actionResult{}, err
	}
	batches, err := buildBatches(ctx, repo, reviewRangeValue, paths, config)
	if err != nil {
		return actionResult{}, err
	}
	observed := observeBudget(batches, paths)
	if err := enforceTotalBudget(config, observed); err != nil {
		return actionResult{}, err
	}
	requested := config.Scope
	scope := scopeAttestation{
		Requested:          requested,
		RequestedInputHash: scopeInputDigest(requested),
		Resolved: resolvedScope{
			Base: reviewRangeValue.Start, Head: reviewRangeValue.End,
			SelectedCommitCount: reviewRangeValue.CommitCount, AvailableCommitCount: resolution.AvailableCommitCount,
			HistoryExhausted: resolution.HistoryExhausted, RepositoryShallow: resolution.RepositoryShallow,
		},
		ObservedBudget: observed,
		Satisfied:      true,
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return actionResult{}, fmt.Errorf("create output directory: %w", err)
	}
	items, err := writeItems(artifactRoot, outputDir, batches)
	if err != nil {
		return actionResult{}, err
	}
	manifestPath := filepath.Join(outputDir, "workset-manifest.json")
	m := manifest{SchemaVersion: manifestSchemaVersion, Scope: scope, Observed: observed, Range: reviewRangeValue, ChangedFiles: len(paths), Items: items}
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return actionResult{}, fmt.Errorf("encode manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(manifestPath, encoded, 0o644); err != nil {
		return actionResult{}, fmt.Errorf("write manifest: %w", err)
	}

	manifestArtifact, err := fileArtifact(artifactRoot, manifestPath, "workset_manifest")
	if err != nil {
		return actionResult{}, err
	}
	manifestArtifact.Description = "workset manifest"
	artifacts := []artifact{manifestArtifact}
	for _, entry := range items {
		path := filepath.Join(outputDir, filepath.FromSlash(entry.DiffPath))
		diffArtifact, err := fileArtifact(artifactRoot, path, "review_diff")
		if err != nil {
			return actionResult{}, err
		}
		diffArtifact.Description = "bounded workset diff"
		artifacts = append(artifacts, diffArtifact)
	}
	return actionResult{Outputs: map[string]any{
		"manifest_path":    manifestArtifact.Path,
		"scope":            scope,
		"range_start":      reviewRangeValue.Start,
		"range_end":        reviewRangeValue.End,
		"commit_count":     reviewRangeValue.CommitCount,
		"changed_files":    len(paths),
		"item_count":       len(items),
		"total_diff_bytes": observed.TotalDiffBytes,
		"total_diff_lines": observed.TotalDiffLines,
	}, Artifacts: artifacts}, nil
}

func applyConfigDefaults(config *Config) {
	legacyScope := config.Scope.Kind == ""
	if strings.TrimSpace(config.Repository) == "" {
		if repo := os.Getenv("HUFU_REPOSITORY"); repo != "" {
			config.Repository = repo
		} else {
			config.Repository = "."
		}
	}
	if strings.TrimSpace(config.ArtifactRoot) == "" {
		if ws := os.Getenv("HUFU_WORKSPACE"); ws != "" {
			config.ArtifactRoot = ws
		}
	}
	if strings.TrimSpace(config.OutputDir) == "" {
		if config.ArtifactRoot != "" {
			config.OutputDir = filepath.Join(config.ArtifactRoot, "workset")
		}
	}
	if legacyScope && strings.TrimSpace(config.Since) == "" {
		config.Since = "2.days.ago"
	}
	if legacyScope && config.MaxCommits == 0 {
		config.MaxCommits = 10
	}
	if legacyScope {
		config.Scope = lastNScopeCandidate(config.MaxCommits, 0, 0, "compatibility").value
	} else if config.Scope.Kind == "last_n" {
		config.MaxCommits = config.Scope.Count
	}
	if config.MaxTotalDiffBytes == 0 && config.MaxTotalDiffLines == 0 && config.MaxChangedPaths == 0 && config.MaxWorksetItems == 0 {
		config.MaxTotalDiffBytes = defaultMaxTotalDiffBytes
		config.MaxTotalDiffLines = defaultMaxTotalDiffLines
		config.MaxChangedPaths = defaultMaxChangedPaths
		config.MaxWorksetItems = defaultMaxWorksetItems
	}
	if config.MaxDiffBytes == 0 && config.MaxDiffLines == 0 && config.MaxPaths == 0 {
		config.MaxDiffBytes = 24000
		config.MaxDiffLines = 600
		config.MaxPaths = 16
	}
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.Repository) == "" {
		return errors.New("repository is required")
	}
	if strings.TrimSpace(config.OutputDir) == "" {
		return errors.New("output_dir is required")
	}
	if err := validateRequestedScope(config.Scope); err != nil {
		return err
	}
	if config.MaxDiffBytes <= 0 || config.MaxDiffLines <= 0 || config.MaxPaths <= 0 {
		return errors.New("max_diff_bytes, max_diff_lines, and max_paths must be positive")
	}
	if config.MaxTotalDiffBytes <= 0 || config.MaxTotalDiffLines <= 0 || config.MaxChangedPaths <= 0 || config.MaxWorksetItems <= 0 {
		return errors.New("max_total_diff_bytes, max_total_diff_lines, max_changed_paths, and max_workset_items must be positive")
	}
	return nil
}

func validateMaxCommits(value int) error {
	if value < 1 || value > 100 {
		return fmt.Errorf("max_commits must be between 1 and 100, got %d", value)
	}
	return nil
}

func resolveRepository(ctx context.Context, repository string) (string, error) {
	root, err := git(ctx, repository, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve repository: %w", err)
	}
	return strings.TrimSpace(root), nil
}

func resolveOutputDir(repo, output string) (string, error) {
	if !filepath.IsAbs(output) {
		output = filepath.Join(repo, output)
	}
	return canonicalPath(output)
}

func resolveArtifactRoot(repo, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		if ws := os.Getenv("HUFU_WORKSPACE"); ws != "" {
			root = ws
		} else {
			return repo, nil
		}
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(repo, root)
	}
	return canonicalPath(root)
}

// canonicalPath resolves the existing portion of path before appending any
// missing suffix. This keeps containment checks correct when a path contains a
// symlink, while still allowing the producer to create a new action directory.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	for current := abs; ; current = filepath.Dir(current) {
		resolved, resolveErr := filepath.EvalSymlinks(current)
		if resolveErr == nil {
			suffix, suffixErr := filepath.Rel(current, abs)
			if suffixErr != nil {
				return "", suffixErr
			}
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", fmt.Errorf("resolve path %q: %w", path, resolveErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("resolve path %q: %w", path, resolveErr)
		}
	}
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func ensureEmptyOutputDir(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat output_dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output_dir %q is not a directory", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read output_dir: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("output_dir %q is not empty", path)
	}
	return nil
}

func resolveRequestedRange(ctx context.Context, repo string, config Config) (rangeResolution, error) {
	shallowText, err := git(ctx, repo, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return rangeResolution{}, fmt.Errorf("detect shallow repository: %w", err)
	}
	if strings.TrimSpace(shallowText) == "true" {
		return rangeResolution{}, errors.New("history_incomplete: shallow repository cannot prove the requested first-parent history")
	}
	switch config.Scope.Kind {
	case "last_n":
		return resolveLastNRange(ctx, repo, config.Scope.Head, config.Since, config.Scope.Count)
	case "revision_range":
		return resolveRevisionRange(ctx, repo, config.Scope)
	case "since":
		return resolveSinceRange(ctx, repo, config.Scope)
	default:
		return rangeResolution{}, fmt.Errorf("unsupported review scope kind %q", config.Scope.Kind)
	}
}

// resolveRange preserves the direct helper contract used by the producer's
// focused tests while action requests migrate to typed Scope payloads.
func resolveRange(ctx context.Context, repo, since string, maxCommits int) (rangeResolution, error) {
	config := Config{Since: since, MaxCommits: maxCommits, Scope: lastNScopeCandidate(maxCommits, 0, 0, "compatibility").value}
	return resolveRequestedRange(ctx, repo, config)
}

func resolveLastNRange(ctx context.Context, repo, head, since string, maxCommits int) (rangeResolution, error) {
	end, err := resolveCommit(ctx, repo, head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve review head %q: %w", head, err)
	}
	// Review selection follows the repository's first-parent history. Without
	// this, selecting the last N commits from a merge-heavy repository and then
	// representing them as a single parent..HEAD range can silently expand the
	// range to include side-branch commits that were not selected.
	args := []string{"rev-list", "--first-parent", "--reverse"}
	if since != "" {
		args = append(args, "--since="+since)
	}
	args = append(args, end)
	commitsText, err := git(ctx, repo, args...)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("list commits: %w", err)
	}
	commits := nonEmptyLines(commitsText)
	available := len(commits)
	if maxCommits > 0 && len(commits) > maxCommits {
		commits = commits[len(commits)-maxCommits:]
	}
	r := reviewRange{End: strings.TrimSpace(end), Since: since, CommitCount: len(commits)}
	resolution := rangeResolution{Range: r, AvailableCommitCount: available, HistoryExhausted: available < maxCommits}
	return finalizeSelectedRange(ctx, repo, commits, resolution)
}

func resolveRevisionRange(ctx context.Context, repo string, scope resolverScope) (rangeResolution, error) {
	base, err := resolveCommit(ctx, repo, scope.Base)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve review base %q: %w", scope.Base, err)
	}
	head, err := resolveCommit(ctx, repo, scope.Head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve review head %q: %w", scope.Head, err)
	}
	base, head = strings.TrimSpace(base), strings.TrimSpace(head)
	chainText, err := git(ctx, repo, "rev-list", "--first-parent", head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("list first-parent history: %w", err)
	}
	if !slices.Contains(nonEmptyLines(chainText), base) {
		return rangeResolution{}, fmt.Errorf("scope_not_first_parent: base %q is not on the first-parent history of %q", scope.Base, scope.Head)
	}
	commitsText, err := git(ctx, repo, "rev-list", "--first-parent", "--reverse", base+".."+head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("list revision range: %w", err)
	}
	commits := nonEmptyLines(commitsText)
	return rangeResolution{Range: reviewRange{Start: base, End: head, CommitCount: len(commits)}, AvailableCommitCount: len(commits)}, nil
}

func resolveSinceRange(ctx context.Context, repo string, scope resolverScope) (rangeResolution, error) {
	head, err := resolveCommit(ctx, repo, scope.Head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve review head %q: %w", scope.Head, err)
	}
	commitsText, err := git(ctx, repo, "rev-list", "--first-parent", "--reverse", "--since="+scope.Since, head)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("list commits since %q: %w", scope.Since, err)
	}
	commits := nonEmptyLines(commitsText)
	resolution := rangeResolution{Range: reviewRange{End: strings.TrimSpace(head), Since: scope.Since, CommitCount: len(commits)}, AvailableCommitCount: len(commits)}
	return finalizeSelectedRange(ctx, repo, commits, resolution)
}

func resolveCommit(ctx context.Context, repo, revision string) (string, error) {
	resolved, err := git(ctx, repo, "rev-parse", "--verify", revision+"^{commit}")
	return strings.TrimSpace(resolved), err
}

func finalizeSelectedRange(ctx context.Context, repo string, commits []string, resolution rangeResolution) (rangeResolution, error) {
	r := resolution.Range
	if len(commits) == 0 {
		return resolution, nil
	}
	start, err := git(ctx, repo, "rev-parse", commits[0]+"^")
	if err != nil {
		root, rootErr := git(ctx, repo, "rev-list", "--max-parents=0", commits[0])
		if rootErr != nil || strings.TrimSpace(root) != commits[0] {
			return rangeResolution{}, fmt.Errorf("history_incomplete: resolve parent of first selected commit: %w", err)
		}
		start, err = gitWithInput(ctx, repo, nil, "hash-object", "-t", "tree", "--stdin")
		if err != nil {
			return rangeResolution{}, fmt.Errorf("resolve empty tree for root commit: %w", err)
		}
		r.Start = strings.TrimSpace(start)
		resolution.Range = r
		return resolution, nil
	}
	r.Start = strings.TrimSpace(start)
	verified, err := git(ctx, repo, "rev-list", "--first-parent", "--count", r.Start+".."+r.End)
	if err != nil {
		return rangeResolution{}, fmt.Errorf("verify commit count: %w", err)
	}
	if strings.TrimSpace(verified) != fmt.Sprintf("%d", r.CommitCount) {
		return rangeResolution{}, fmt.Errorf("commit range count mismatch: expected %d, got %s", r.CommitCount, strings.TrimSpace(verified))
	}
	resolution.Range = r
	return resolution, nil
}

func changedPaths(ctx context.Context, repo string, r reviewRange) ([]string, error) {
	if r.CommitCount == 0 {
		return nil, nil
	}
	output, err := git(ctx, repo, "diff", "--no-renames", "--name-only", "-z", r.Start+".."+r.End)
	if err != nil {
		return nil, fmt.Errorf("list changed paths: %w", err)
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, path := range strings.Split(output, "\x00") {
		if path == "" {
			continue
		}
		if strings.ContainsAny(path, "\r\n\t") {
			return nil, fmt.Errorf("changed path %q cannot be represented in a line-oriented manifest", path)
		}
		normalizedPath, err := team.NormalizeTouchedPath(path)
		if err != nil {
			return nil, fmt.Errorf("normalize changed path %q: %w", path, err)
		}
		seen[normalizedPath] = struct{}{}
	}
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func buildBatches(ctx context.Context, repo string, r reviewRange, paths []string, config Config) ([]*batch, error) {
	var batches []*batch
	for _, path := range paths {
		diff, err := git(ctx, repo, "diff", "--no-renames", "--unified=0", r.Start+".."+r.End, "--", path)
		if err != nil {
			return nil, fmt.Errorf("diff %q: %w", path, err)
		}
		if diff == "" {
			diff = fmt.Sprintf("diff --git a/%s b/%s\n", path, path)
		}
		lens := lensForPath(path)
		for _, unit := range splitDiff([]byte(diff)) {
			unitLines := bytes.Count(unit, []byte("\n"))
			candidate := findBatch(batches, lens)
			newPath := candidate == nil || !candidate.hasPath(path)
			if candidate == nil || candidate.shouldFlush(len(unit), unitLines, newPath, config) {
				candidate = &batch{lens: lens, pathSeen: make(map[string]struct{})}
				batches = append(batches, candidate)
			}
			candidate.add(path, unit, unitLines)
		}
	}

	return batches, nil
}

func writeItems(artifactRoot, outputDir string, batches []*batch) ([]item, error) {
	items := make([]item, 0, len(batches))
	for index, current := range batches {
		key := fmt.Sprintf("unit-%04d", index)
		dir := filepath.Join(outputDir, "batches", key)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create batch %q: %w", key, err)
		}
		diffPath := filepath.Join(dir, "diff.patch")
		if err := os.WriteFile(diffPath, current.diff.Bytes(), 0o644); err != nil {
			return nil, fmt.Errorf("write batch diff %q: %w", key, err)
		}
		pathsPath := filepath.Join(dir, "paths.txt")
		if err := os.WriteFile(pathsPath, []byte(strings.Join(current.paths, "\n")+"\n"), 0o644); err != nil {
			return nil, fmt.Errorf("write batch paths %q: %w", key, err)
		}
		diffArtifact, err := fileArtifact(artifactRoot, diffPath, "review_diff")
		if err != nil {
			return nil, fmt.Errorf("describe batch diff %q: %w", key, err)
		}
		diffArtifact.Description = "bounded workset diff"
		items = append(items, item{
			Key: key, Lens: current.lens,
			Bindings:     map[string]string{"key": key, "lens": current.lens},
			TouchedPaths: append([]string(nil), current.paths...),
			Inputs:       []artifact{diffArtifact},
			DiffPath:     filepath.ToSlash(filepath.Join("batches", key, "diff.patch")),
			DiffSHA:      sha256Hex(current.diff.Bytes()), DiffBytes: current.diff.Len(), DiffLines: current.lines,
		})
	}
	return items, nil
}

func observeBudget(batches []*batch, paths []string) observedBudget {
	observed := observedBudget{ChangedPaths: len(paths), WorksetItems: len(batches)}
	for _, current := range batches {
		observed.TotalDiffBytes += current.diff.Len()
		observed.TotalDiffLines += current.lines
	}
	return observed
}

func enforceTotalBudget(config Config, observed observedBudget) error {
	if observed.TotalDiffBytes <= config.MaxTotalDiffBytes &&
		observed.TotalDiffLines <= config.MaxTotalDiffLines &&
		observed.ChangedPaths <= config.MaxChangedPaths &&
		observed.WorksetItems <= config.MaxWorksetItems {
		return nil
	}
	return fmt.Errorf("scope_too_large\nrequested scope: %s\nobserved totals: bytes=%d lines=%d paths=%d items=%d\nconfigured caps: bytes=%d lines=%d paths=%d items=%d\nremediation: narrow --input review.scope, raise an explicit budget, or split the run",
		compactScope(config.Scope),
		observed.TotalDiffBytes, observed.TotalDiffLines, observed.ChangedPaths, observed.WorksetItems,
		config.MaxTotalDiffBytes, config.MaxTotalDiffLines, config.MaxChangedPaths, config.MaxWorksetItems,
	)
}

func scopeInputDigest(requested requestedScope) string {
	encoded, err := json.Marshal(requested)
	if err != nil {
		panic(fmt.Sprintf("encode requested scope: %v", err))
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		panic(fmt.Sprintf("canonicalize requested scope: %v", err))
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		panic(fmt.Sprintf("encode canonical requested scope: %v", err))
	}
	return "sha256:" + sha256Hex(canonical)
}

func compactScope(scope resolverScope) string {
	encoded, err := json.Marshal(scope)
	if err != nil {
		return scope.Kind
	}
	return string(encoded)
}

func findBatch(batches []*batch, lens string) *batch {
	if len(batches) == 0 {
		return nil
	}
	last := batches[len(batches)-1]
	if last.lens == lens {
		return last
	}
	return nil
}

func (b *batch) hasPath(path string) bool {
	_, ok := b.pathSeen[path]
	return ok
}

func (b *batch) shouldFlush(bytesToAdd, linesToAdd int, newPath bool, config Config) bool {
	if b.diff.Len() == 0 {
		return false
	}
	return b.diff.Len()+bytesToAdd > config.MaxDiffBytes || b.lines+linesToAdd > config.MaxDiffLines || (newPath && len(b.paths) >= config.MaxPaths)
}

func (b *batch) add(path string, unit []byte, lines int) {
	b.diff.Write(unit)
	b.lines += lines
	if _, ok := b.pathSeen[path]; !ok {
		b.pathSeen[path] = struct{}{}
		b.paths = append(b.paths, path)
	}
}

func splitDiff(diff []byte) [][]byte {
	lines := bytes.SplitAfter(diff, []byte("\n"))
	var header bytes.Buffer
	var units [][]byte
	var current bytes.Buffer
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("@@ ")) {
			if current.Len() > 0 {
				units = append(units, append([]byte(nil), current.Bytes()...))
			}
			current.Reset()
			current.Write(header.Bytes())
			current.Write(line)
			continue
		}
		if current.Len() == 0 {
			header.Write(line)
			continue
		}
		current.Write(line)
	}
	if current.Len() > 0 {
		units = append(units, append([]byte(nil), current.Bytes()...))
	}
	if len(units) == 0 && len(diff) > 0 {
		return [][]byte{append([]byte(nil), diff...)}
	}
	return units
}

func lensForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "internal/tools/"):
		return "security-tool"
	case strings.HasPrefix(path, "internal/team/"), strings.HasPrefix(path, "internal/agent/"), strings.HasPrefix(path, "internal/memory/"), strings.HasPrefix(path, "internal/skill/"):
		return "runtime-integrity"
	case strings.HasPrefix(path, "cmd/hufu/"), strings.HasPrefix(path, "internal/config/"), strings.HasPrefix(path, "internal/mcp/"), strings.HasPrefix(path, "internal/sidecar/"), strings.HasPrefix(path, "internal/tui/"), strings.HasPrefix(path, "internal/readline/"), strings.HasPrefix(path, "internal/hooks/"), strings.HasPrefix(path, "internal/notify/"):
		return "boundary-tui"
	default:
		return "general"
	}
}

func fileArtifact(root, path, kind string) (artifact, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return artifact{}, fmt.Errorf("read artifact %q: %w", path, err)
	}
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return artifact{}, fmt.Errorf("resolve artifact root: %w", err)
	}
	resolvedPath, err := canonicalPath(path)
	if err != nil {
		return artifact{}, fmt.Errorf("resolve artifact path: %w", err)
	}
	if !pathWithin(canonicalRoot, resolvedPath) {
		return artifact{}, fmt.Errorf("artifact path %q escapes artifact root %q", path, root)
	}
	rel, err := filepath.Rel(canonicalRoot, resolvedPath)
	if err != nil {
		return artifact{}, fmt.Errorf("make artifact path relative: %w", err)
	}
	digest := sha256Hex(data)
	return artifact{ID: "sha256-" + digest, Path: filepath.ToSlash(rel), Kind: kind, SHA256: digest, Bytes: int64(len(data))}, nil
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitWithInput(ctx, dir, nil, args...)
}

func gitWithInput(ctx context.Context, dir string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
