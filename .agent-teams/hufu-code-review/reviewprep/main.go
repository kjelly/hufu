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
	lastCommitsPattern = regexp.MustCompile(`(?i)\blast[ \t]+([0-9]+)[ \t]+commits?\b`)
	headRangePattern   = regexp.MustCompile(`(?i)\bHEAD~([0-9]+)\.\.HEAD\b`)
	revisionPattern    = regexp.MustCompile(`(?i)([A-Za-z0-9][A-Za-z0-9._/-]{0,159})\.\.([A-Za-z0-9][A-Za-z0-9._/-]{0,159})`)
	sincePattern       = regexp.MustCompile(`(?i)\bsince[ \t]+([0-9]{4}-[0-9]{2}-[0-9]{2})\b`)
)

// Config intentionally contains only team-owned workset semantics. Hufu
// passes it as an opaque Action payload and does not interpret these fields.
type Config struct {
	Repository        string `json:"repository"`
	OutputDir         string `json:"output_dir"`
	ArtifactRoot      string `json:"artifact_root"`
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
	Repository        string `json:"repository"`
	OutputDir         string `json:"output_dir"`
	ArtifactRoot      string `json:"artifact_root"`
	Since             string `json:"since"`
	MaxCommits        string `json:"max_commits"`
	MaxTotalDiffBytes int    `json:"max_total_diff_bytes"`
	MaxTotalDiffLines int    `json:"max_total_diff_lines"`
	MaxChangedPaths   int    `json:"max_changed_paths"`
	MaxWorksetItems   int    `json:"max_workset_items"`
	MaxDiffBytes      int    `json:"max_diff_bytes"`
	MaxDiffLines      int    `json:"max_diff_lines"`
	MaxPaths          int    `json:"max_paths"`
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

type requestedScope struct {
	Kind    string `json:"kind"`
	Count   int    `json:"count"`
	History string `json:"history"`
	Head    string `json:"head"`
}

type resolvedScope struct {
	Base                 string `json:"base"`
	Head                 string `json:"head"`
	SelectedCommitCount  int    `json:"selected_commit_count"`
	AvailableCommitCount int    `json:"available_commit_count"`
	HistoryExhausted     bool   `json:"history_exhausted"`
	RepositoryShallow    bool   `json:"repository_shallow"`
}

type scopeAttestation struct {
	Requested   requestedScope `json:"requested"`
	Resolved    resolvedScope  `json:"resolved"`
	Satisfied   bool           `json:"satisfied"`
	InputDigest string         `json:"input_digest"`
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
	response := team.RunInputResolverResponse{ResolverVersion: "1"}
	if request.Type != "resolve_run_input" || request.InputName != "review.scope" || request.ResolverID != "review-scope-v1" {
		response.Status = "invalid"
		response.Diagnostic = "resolver request identity is unsupported"
		return response
	}
	candidates, invalid := parseScopeCandidates(request.Prompt)
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

func parseScopeCandidates(prompt string) ([]scopeCandidate, string) {
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
	maxCommits, err := strconv.Atoi(wire.MaxCommits)
	if err != nil {
		return Config{}, fmt.Errorf("max_commits must be a base-10 integer string between 1 and 100: %w", err)
	}
	config := Config{
		Repository: wire.Repository, OutputDir: wire.OutputDir, ArtifactRoot: wire.ArtifactRoot,
		Since: wire.Since, MaxCommits: maxCommits,
		MaxTotalDiffBytes: wire.MaxTotalDiffBytes, MaxTotalDiffLines: wire.MaxTotalDiffLines,
		MaxChangedPaths: wire.MaxChangedPaths, MaxWorksetItems: wire.MaxWorksetItems,
		MaxDiffBytes: wire.MaxDiffBytes, MaxDiffLines: wire.MaxDiffLines, MaxPaths: wire.MaxPaths,
	}
	if err := validateMaxCommits(config.MaxCommits); err != nil {
		return Config{}, err
	}
	return config, nil
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
	if rel, relErr := filepath.Rel(artifactRoot, outputDir); relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return actionResult{}, fmt.Errorf("output_dir %q must be beneath artifact_root %q", config.OutputDir, config.ArtifactRoot)
	}

	resolution, err := resolveRange(ctx, repo, config.Since, config.MaxCommits)
	if err != nil {
		return actionResult{}, err
	}
	reviewRangeValue := resolution.Range
	if reviewRangeValue.CommitCount == 0 {
		return actionResult{}, fmt.Errorf("scope_empty: requested last %d first-parent commits ending at HEAD selected no commits", config.MaxCommits)
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
	requested := requestedScope{Kind: "last_n", Count: config.MaxCommits, History: "first_parent", Head: "HEAD"}
	scope := scopeAttestation{
		Requested: requested,
		Resolved: resolvedScope{
			Base: reviewRangeValue.Start, Head: reviewRangeValue.End,
			SelectedCommitCount: reviewRangeValue.CommitCount, AvailableCommitCount: resolution.AvailableCommitCount,
			HistoryExhausted: resolution.HistoryExhausted, RepositoryShallow: resolution.RepositoryShallow,
		},
		Satisfied: true, InputDigest: scopeInputDigest(requested),
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
	if strings.TrimSpace(config.Since) == "" {
		config.Since = "2.days.ago"
	}
	if config.MaxCommits == 0 {
		config.MaxCommits = 10
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
	if strings.TrimSpace(config.Since) == "" {
		return errors.New("since is required")
	}
	if err := validateMaxCommits(config.MaxCommits); err != nil {
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
	output = filepath.Clean(output)
	rel, err := filepath.Rel(repo, output)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("output_dir %q must be inside repository", output)
	}
	return output, nil
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
	root = filepath.Clean(root)
	rel, err := filepath.Rel(repo, root)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact_root %q must be inside repository", root)
	}
	return root, nil
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

func resolveRange(ctx context.Context, repo, since string, maxCommits int) (rangeResolution, error) {
	shallowText, err := git(ctx, repo, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return rangeResolution{}, fmt.Errorf("detect shallow repository: %w", err)
	}
	if strings.TrimSpace(shallowText) == "true" {
		return rangeResolution{}, errors.New("history_incomplete: shallow repository cannot prove the requested first-parent history")
	}
	end, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	// Review selection follows the repository's first-parent history. Without
	// this, selecting the last N commits from a merge-heavy repository and then
	// representing them as a single parent..HEAD range can silently expand the
	// range to include side-branch commits that were not selected.
	commitsText, err := git(ctx, repo, "rev-list", "--first-parent", "--reverse", "--since="+since, "HEAD")
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
	return fmt.Errorf("scope_too_large\nrequested scope: last %d first-parent commits ending at HEAD\nobserved totals: bytes=%d lines=%d paths=%d items=%d\nconfigured caps: bytes=%d lines=%d paths=%d items=%d\nremediation: narrow --input review.scope, raise an explicit budget, or split the run",
		config.MaxCommits,
		observed.TotalDiffBytes, observed.TotalDiffLines, observed.ChangedPaths, observed.WorksetItems,
		config.MaxTotalDiffBytes, config.MaxTotalDiffLines, config.MaxChangedPaths, config.MaxWorksetItems,
	)
}

func scopeInputDigest(requested requestedScope) string {
	encoded, err := json.Marshal(requested)
	if err != nil {
		panic(fmt.Sprintf("encode requested scope: %v", err))
	}
	return "sha256:" + sha256Hex(encoded)
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
	rel, err := filepath.Rel(root, path)
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
