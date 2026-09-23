// reviewprep is a team-owned, deterministic workset producer.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	manifestSchemaVersion = 2
	scopeResolverVersion  = "semantic-fallback-2"
)

const (
	defaultMaxTotalDiffBytes = 1_048_576
	defaultMaxTotalDiffLines = 20_000
	defaultMaxChangedPaths   = 256
	defaultMaxWorksetItems   = 64
	routingDocumentation     = "documentation"
)

type actionRequest struct {
	Capability string `json:"capability,omitempty"`
	Type       string `json:"type"`
	Payload    string `json:"payload"`
}

type resolverScope struct {
	Kind       string `json:"kind"`
	Count      int    `json:"count,omitempty"`
	History    string `json:"history"`
	Head       string `json:"head"`
	Base       string `json:"base,omitempty"`
	Since      string `json:"since,omitempty"`
	CommitType string `json:"commit_type,omitempty"`
}

type runInputResolverRequest struct {
	Type          string          `json:"type"`
	InputName     string          `json:"input_name"`
	Prompt        string          `json:"prompt"`
	ExplicitValue json.RawMessage `json:"explicit_value"`
	SchemaHash    string          `json:"schema_hash"`
	ResolverID    string          `json:"resolver_id"`
}

type runInputResolverEvidence struct {
	Source string `json:"source"`
	Start  int    `json:"start,omitzero"`
	End    int    `json:"end,omitzero"`
	Kind   string `json:"kind,omitempty"`
}

type runInputResolverResponse struct {
	Status          string                     `json:"status"`
	Value           json.RawMessage            `json:"value,omitempty"`
	Evidence        []runInputResolverEvidence `json:"evidence,omitempty"`
	ResolverVersion string                     `json:"resolver_version,omitempty"`
	Diagnostic      string                     `json:"diagnostic,omitempty"`
}

var (
	revisionNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^{}-]{0,159}$`)
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
	Routing           string `json:"routing"`
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
	Routing           string         `json:"routing,omitempty"`
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
	Start       string   `json:"start"`
	End         string   `json:"end"`
	Since       string   `json:"since"`
	CommitCount int      `json:"commit_count"`
	Source      string   `json:"source,omitempty"`
	Commits     []string `json:"commits,omitempty"`
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

type documentationVerification struct {
	Passed         bool     `json:"passed"`
	CheckedFiles   []string `json:"checked_files,omitempty"`
	CheckedLinks   int      `json:"checked_links"`
	CheckedPaths   int      `json:"checked_paths"`
	CheckedSymbols int      `json:"checked_symbols"`
	Issues         []string `json:"issues,omitempty"`
}

type routedWorkset struct {
	name        string
	description string
	paths       []string
	batches     []*batch
	extraInputs []artifact
}

type listedGoPackage struct {
	Dir        string   `json:"Dir"`
	ImportPath string   `json:"ImportPath"`
	Name       string   `json:"Name"`
	GoFiles    []string `json:"GoFiles"`
	CgoFiles   []string `json:"CgoFiles"`
}

type loadedGoPackage struct {
	value listedGoPackage
	err   error
}

type goImportCandidate struct {
	path          string
	explicitAlias bool
	preferred     bool
}

type reviewGoSymbolResolver struct {
	ctx              context.Context
	repo             string
	rangeDef         reviewRange
	resolutionDir    string
	resolutionErr    error
	packages         map[string]loadedGoPackage
	standardPackages map[string][]string
	standardLoaded   bool
	standardErr      error
}

var (
	markdownLinkPattern = regexp.MustCompile(`\[[^\]]*\]\(([^)[:space:]]+)(?:[[:space:]]+"[^"]*")?\)`)
	inlineCodePattern   = regexp.MustCompile("`([^`\\n]+)`")
	goSymbolPattern     = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)
	qualifiedGoSymbol   = regexp.MustCompile(`^[a-z][A-Za-z0-9_]*\.[A-Z][A-Za-z0-9_]*$`)
)

// Run is the stable entrypoint invoked by Hufu's embedded trusted-static Go
// runtime. The wire format intentionally duplicates the runtime-neutral JSON
// contract so this team-owned script does not import Hufu internals.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	return run(ctx, in, out)
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
		var request runInputResolverRequest
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

func resolveReviewScopeInput(request runInputResolverRequest) runInputResolverResponse {
	response := runInputResolverResponse{ResolverVersion: scopeResolverVersion}
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
	// Natural-language interpretation belongs to Hufu's semantic_json model
	// resolver. This deterministic provider is only the fail-closed fallback;
	// it validates explicit structured input but never guesses scope from prose.
	response.Status = "no_match"
	return response
}

func lastNScope(count int) resolverScope {
	return resolverScope{Kind: "last_n", Count: count, History: "first_parent", Head: "HEAD"}
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
		scope = lastNScope(parsed)
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
		Routing: wire.Routing,
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
		if scope.CommitType != "" && scope.CommitType != "feat" {
			return fmt.Errorf("last_n review scope commit_type %q is unsupported", scope.CommitType)
		}
	case "revision_range":
		if scope.Count != 0 || scope.Since != "" || scope.CommitType != "" || !validRevision(scope.Base) {
			return errors.New("revision_range review scope requires a valid base and cannot set count, since, or commit_type")
		}
	case "since":
		if scope.Count != 0 || scope.Base != "" || scope.CommitType != "" {
			return errors.New("since review scope cannot set count, base, or commit_type")
		}
		if _, err := time.Parse(time.DateOnly, scope.Since); err != nil {
			return errors.New("since review scope requires a valid YYYY-MM-DD date")
		}
	case "working_tree":
		if scope.Count != 0 || scope.Base != "" || scope.Since != "" || scope.CommitType != "" || scope.Head != "HEAD" {
			return errors.New("working_tree review scope requires head HEAD and cannot set count, base, since, or commit_type")
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

// Prepare materialises a deterministic, bounded review workset. Commit scopes
// read Git objects; working_tree scope reads the caller's current staged,
// unstaged, and non-ignored untracked changes. OutputDir must be new or empty,
// and the repository mutation canary verifies that preparation does not change
// repository state.
func Prepare(ctx context.Context, config Config) (result actionResult, resultErr error) {
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
	if err := rejectGitAdministrativeOutput(ctx, repo, outputDir); err != nil {
		return actionResult{}, err
	}
	before, err := captureRepositorySnapshot(ctx, repo, outputDir)
	if err != nil {
		return actionResult{}, fmt.Errorf("capture repository mutation baseline: %w", err)
	}
	defer func() {
		after, err := captureRepositorySnapshot(ctx, repo, outputDir)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("verify repository remained unchanged: %w", err))
			return
		}
		resultErr = errors.Join(resultErr, compareRepositorySnapshots(before, after))
	}()

	resolution, err := resolveRequestedRange(ctx, repo, config)
	if err != nil {
		return actionResult{}, err
	}
	reviewRangeValue := resolution.Range
	if reviewRangeValue.CommitCount == 0 && !reviewRangeValue.isWorkingTree() {
		return actionResult{}, fmt.Errorf("scope_empty: requested review scope %s selected no commits", compactScope(config.Scope))
	}
	paths, err := changedPaths(ctx, repo, reviewRangeValue)
	if err != nil {
		return actionResult{}, err
	}
	if len(paths) == 0 {
		return actionResult{}, fmt.Errorf("scope_empty: requested review scope %s selected no changed paths", compactScope(config.Scope))
	}
	if config.Routing == routingDocumentation {
		return prepareRoutedReview(ctx, repo, artifactRoot, outputDir, paths, resolution, config)
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

func prepareRoutedReview(ctx context.Context, repo, artifactRoot, outputDir string, paths []string, resolution rangeResolution, config Config) (actionResult, error) {
	r := resolution.Range
	documentationPaths := make([]string, 0)
	primaryPaths := make([]string, 0)
	routineDocumentationPaths := make([]string, 0)
	escalatedDocumentationPaths := make([]string, 0)
	for _, path := range paths {
		switch classifyReviewPath(ctx, repo, r, path) {
		case "documentation":
			documentationPaths = append(documentationPaths, path)
			routineDocumentationPaths = append(routineDocumentationPaths, path)
		case "documentation-risk":
			documentationPaths = append(documentationPaths, path)
			primaryPaths = append(primaryPaths, path)
			escalatedDocumentationPaths = append(escalatedDocumentationPaths, path)
		default:
			primaryPaths = append(primaryPaths, path)
		}
	}

	verification, err := verifyDocumentationChanges(ctx, repo, r, documentationPaths)
	if err != nil {
		return actionResult{}, err
	}
	primaryBatches, err := buildBatches(ctx, repo, r, primaryPaths, config)
	if err != nil {
		return actionResult{}, err
	}
	documentationBatches, err := buildBatches(ctx, repo, r, routineDocumentationPaths, config)
	if err != nil {
		return actionResult{}, err
	}
	escalationBatches, err := buildBatches(ctx, repo, r, escalatedDocumentationPaths, config)
	if err != nil {
		return actionResult{}, err
	}
	setBatchLens(documentationBatches, "documentation")
	setBatchLens(escalationBatches, "documentation-risk")
	for _, current := range primaryBatches {
		for _, currentPath := range current.paths {
			if isDocumentationPath(currentPath) {
				current.lens = "documentation-risk"
				break
			}
		}
	}
	annotateBatches(primaryBatches, "primary")
	annotateBatches(documentationBatches, "documentation")
	annotateBatches(escalationBatches, "documentation-escalation")
	allBatches := append(append(append([]*batch(nil), primaryBatches...), documentationBatches...), escalationBatches...)
	observed := observeBudget(allBatches, paths)
	if err := enforceTotalBudget(config, observed); err != nil {
		return actionResult{}, err
	}
	scope := scopeAttestation{
		Requested:          config.Scope,
		RequestedInputHash: scopeInputDigest(config.Scope),
		Resolved: resolvedScope{
			Base: r.Start, Head: r.End,
			SelectedCommitCount: r.CommitCount, AvailableCommitCount: resolution.AvailableCommitCount,
			HistoryExhausted: resolution.HistoryExhausted, RepositoryShallow: resolution.RepositoryShallow,
		},
		ObservedBudget: observed,
		Satisfied:      true,
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return actionResult{}, fmt.Errorf("create output directory: %w", err)
	}

	verificationPath := filepath.Join(outputDir, "documentation-verification.json")
	verificationData, err := json.MarshalIndent(verification, "", "  ")
	if err != nil {
		return actionResult{}, fmt.Errorf("encode documentation verification: %w", err)
	}
	verificationData = append(verificationData, '\n')
	if err := os.WriteFile(verificationPath, verificationData, 0o644); err != nil {
		return actionResult{}, fmt.Errorf("write documentation verification: %w", err)
	}
	verificationArtifact, err := fileArtifact(artifactRoot, verificationPath, "documentation_verification")
	if err != nil {
		return actionResult{}, err
	}
	verificationArtifact.Description = "documentation verification report"

	worksets := []routedWorkset{
		{name: "primary", description: "primary review workset manifest", paths: primaryPaths, batches: primaryBatches, extraInputs: []artifact{verificationArtifact}},
		{name: "documentation", description: "documentation review workset manifest", paths: routineDocumentationPaths, batches: documentationBatches, extraInputs: []artifact{verificationArtifact}},
		{name: "documentation-escalation", description: "documentation escalation workset manifest", paths: escalatedDocumentationPaths, batches: escalationBatches, extraInputs: []artifact{verificationArtifact}},
	}
	artifacts := make([]artifact, 0, 1+len(worksets)*2)
	outputs := map[string]any{
		"scope":                      scope,
		"range_start":                r.Start,
		"range_end":                  r.End,
		"commit_count":               r.CommitCount,
		"changed_files":              len(paths),
		"total_diff_bytes":           observed.TotalDiffBytes,
		"total_diff_lines":           observed.TotalDiffLines,
		"documentation_verification": verification,
	}
	for index := range worksets {
		current := &worksets[index]
		if len(current.batches) == 0 {
			current.batches = []*batch{noopBatch(current.name)}
		}
		manifestArtifact, worksetArtifacts, itemCount, writeErr := writeRoutedWorkset(artifactRoot, outputDir, r, scope, *current)
		if writeErr != nil {
			return actionResult{}, writeErr
		}
		if index == 0 {
			outputs["manifest_path"] = manifestArtifact.Path
		}
		outputs[current.name+"_manifest_path"] = manifestArtifact.Path
		outputs[current.name+"_item_count"] = itemCount
		artifacts = append(artifacts, manifestArtifact)
		artifacts = append(artifacts, worksetArtifacts...)
	}
	artifacts = append(artifacts, verificationArtifact)
	outputs["item_count"] = len(primaryBatches) + len(documentationBatches) + len(escalationBatches)
	return actionResult{Outputs: outputs, Artifacts: artifacts}, nil
}

func writeRoutedWorkset(artifactRoot, outputDir string, r reviewRange, scope scopeAttestation, workset routedWorkset) (artifact, []artifact, int, error) {
	worksetDir := filepath.Join(outputDir, workset.name)
	items, err := writeItems(artifactRoot, worksetDir, workset.batches)
	if err != nil {
		return artifact{}, nil, 0, err
	}
	for index := range items {
		items[index].Bindings["route"] = workset.name
		if items[index].Lens == "documentation" || items[index].Lens == "documentation-risk" {
			items[index].Inputs = append(items[index].Inputs, workset.extraInputs...)
		}
	}
	partitionObserved := observeBudget(workset.batches, workset.paths)
	partitionScope := scope
	partitionScope.ObservedBudget = partitionObserved
	m := manifest{SchemaVersion: manifestSchemaVersion, Scope: partitionScope, Observed: partitionObserved, Range: r, ChangedFiles: len(workset.paths), Items: items}
	encoded, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return artifact{}, nil, 0, fmt.Errorf("encode %s manifest: %w", workset.name, err)
	}
	encoded = append(encoded, '\n')
	manifestPath := filepath.Join(worksetDir, "workset-manifest.json")
	if err := os.WriteFile(manifestPath, encoded, 0o644); err != nil {
		return artifact{}, nil, 0, fmt.Errorf("write %s manifest: %w", workset.name, err)
	}
	manifestArtifact, err := fileArtifact(artifactRoot, manifestPath, "workset_manifest")
	if err != nil {
		return artifact{}, nil, 0, err
	}
	manifestArtifact.Description = workset.description
	artifacts := make([]artifact, 0, len(items))
	for _, entry := range items {
		path := filepath.Join(worksetDir, filepath.FromSlash(entry.DiffPath))
		diffArtifact, artifactErr := fileArtifact(artifactRoot, path, "review_diff")
		if artifactErr != nil {
			return artifact{}, nil, 0, artifactErr
		}
		diffArtifact.Description = "bounded workset diff"
		artifacts = append(artifacts, diffArtifact)
	}
	itemCount := len(items)
	return manifestArtifact, artifacts, itemCount, nil
}

func noopBatch(route string) *batch {
	content := []byte("No changed paths were routed to " + route + ". Submit a minimal successful no-op result.\n")
	value := &batch{lens: "noop", pathSeen: make(map[string]struct{})}
	value.diff.Write(content)
	value.lines = bytes.Count(content, []byte("\n"))
	return value
}

func setBatchLens(batches []*batch, lens string) {
	for _, current := range batches {
		current.lens = lens
	}
}

func annotateBatches(batches []*batch, route string) {
	for _, current := range batches {
		content := bytes.Clone(current.diff.Bytes())
		current.diff.Reset()
		fmt.Fprintf(&current.diff, "# hufu-review-route: %s\n", route)
		_, _ = current.diff.Write(content)
		current.lines++
	}
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
		config.Scope = lastNScope(config.MaxCommits)
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
	if config.Routing != "" && config.Routing != routingDocumentation {
		return fmt.Errorf("unsupported routing mode %q", config.Routing)
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

type repositorySnapshot struct {
	headDigest  string
	indexDigest string
	status      string
	dirtyDigest string
}

func rejectGitAdministrativeOutput(ctx context.Context, repo, outputDir string) error {
	gitDir, err := git(ctx, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve git administrative directory: %w", err)
	}
	commonDir, err := git(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git common directory: %w", err)
	}
	for _, raw := range []string{gitDir, commonDir} {
		candidate := strings.TrimSpace(raw)
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(repo, candidate)
		}
		candidate, err = canonicalPath(candidate)
		if err != nil {
			return fmt.Errorf("canonicalize git administrative directory: %w", err)
		}
		if pathWithin(candidate, outputDir) {
			return fmt.Errorf("output_dir %q must not be inside Git administrative directory %q", outputDir, candidate)
		}
	}
	return nil
}

func captureRepositorySnapshot(ctx context.Context, repo, outputDir string) (repositorySnapshot, error) {
	head, err := git(ctx, repo, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read HEAD: %w", err)
	}
	indexPath, err := git(ctx, repo, "rev-parse", "--git-path", "index")
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("resolve index path: %w", err)
	}
	indexPath = strings.TrimSpace(indexPath)
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(repo, indexPath)
	}
	indexData, err := os.ReadFile(indexPath)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read index: %w", err)
	}
	status, err := git(ctx, repo, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=no")
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("read worktree status: %w", err)
	}
	status, err = excludeOutputStatus(status, repo, outputDir)
	if err != nil {
		return repositorySnapshot{}, err
	}
	dirtyDigest, err := digestDirtyWorktree(repo, status)
	if err != nil {
		return repositorySnapshot{}, fmt.Errorf("hash dirty worktree: %w", err)
	}
	return repositorySnapshot{
		headDigest:  strings.TrimSpace(head),
		indexDigest: sha256Hex(indexData),
		status:      status,
		dirtyDigest: dirtyDigest,
	}, nil
}

func excludeOutputStatus(status, repo, outputDir string) (string, error) {
	relativeOutput, err := filepath.Rel(repo, outputDir)
	if err != nil || relativeOutput == "." || relativeOutput == ".." || strings.HasPrefix(relativeOutput, ".."+string(filepath.Separator)) || filepath.IsAbs(relativeOutput) {
		return status, nil
	}
	relativeOutput = filepath.ToSlash(relativeOutput)
	records := strings.Split(status, "\x00")
	var filtered strings.Builder
	for index := 0; index < len(records)-1; index++ {
		record := records[index]
		if len(record) < 4 || record[2] != ' ' {
			return "", fmt.Errorf("parse git status record %q", record)
		}
		paths := []string{record[3:]}
		isRenameOrCopy := record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C'
		if isRenameOrCopy {
			index++
			if index >= len(records)-1 {
				return "", errors.New("parse git status rename record: missing source path")
			}
			paths = append(paths, records[index])
		}
		allWithinOutput := true
		for _, current := range paths {
			if current != relativeOutput && !strings.HasPrefix(current, relativeOutput+"/") {
				allWithinOutput = false
				break
			}
		}
		if allWithinOutput {
			continue
		}
		filtered.WriteString(record)
		filtered.WriteByte(0)
		if isRenameOrCopy {
			filtered.WriteString(paths[1])
			filtered.WriteByte(0)
		}
	}
	return filtered.String(), nil
}

func digestDirtyWorktree(repo, status string) (string, error) {
	paths, err := statusPaths(status)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	for _, path := range paths {
		if _, err := io.WriteString(hasher, path+"\x00"); err != nil {
			return "", err
		}
		fullPath := filepath.Join(repo, filepath.FromSlash(path))
		info, err := os.Lstat(fullPath)
		if errors.Is(err, os.ErrNotExist) {
			if _, err := io.WriteString(hasher, "missing\x00"); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("stat %q: %w", path, err)
		}
		if _, err := fmt.Fprintf(hasher, "%s\x00", info.Mode()); err != nil {
			return "", err
		}
		switch {
		case info.Mode().IsRegular():
			file, err := os.Open(fullPath)
			if err != nil {
				return "", fmt.Errorf("open %q: %w", path, err)
			}
			_, copyErr := io.Copy(hasher, file)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return "", errors.Join(copyErr, closeErr)
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(fullPath)
			if err != nil {
				return "", fmt.Errorf("read symlink %q: %w", path, err)
			}
			if _, err := io.WriteString(hasher, target); err != nil {
				return "", err
			}
		}
		if _, err := io.WriteString(hasher, "\x00"); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func statusPaths(status string) ([]string, error) {
	records := strings.Split(status, "\x00")
	seen := make(map[string]struct{})
	for index := 0; index < len(records)-1; index++ {
		record := records[index]
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("parse git status record %q", record)
		}
		seen[record[3:]] = struct{}{}
		if record[0] == 'R' || record[0] == 'C' || record[1] == 'R' || record[1] == 'C' {
			index++
			if index >= len(records)-1 {
				return nil, errors.New("parse git status rename record: missing source path")
			}
			seen[records[index]] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func compareRepositorySnapshots(before, after repositorySnapshot) error {
	changes := make([]string, 0, 4)
	if before.headDigest != after.headDigest {
		changes = append(changes, fmt.Sprintf("HEAD changed from %s to %s", before.headDigest, after.headDigest))
	}
	if before.indexDigest != after.indexDigest {
		changes = append(changes, "Git index content changed")
	}
	if before.status != after.status {
		changes = append(changes, fmt.Sprintf("worktree status changed (before sha256:%s, after sha256:%s)", sha256Hex([]byte(before.status)), sha256Hex([]byte(after.status))))
	}
	if before.dirtyDigest != after.dirtyDigest {
		changes = append(changes, "dirty tracked or untracked file content changed")
	}
	if len(changes) == 0 {
		return nil
	}
	return fmt.Errorf("repository mutation detected: %s", strings.Join(changes, "; "))
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
		if config.Scope.CommitType != "" {
			return resolveLastNByCommitType(ctx, repo, config.Scope.Head, config.Since, config.Scope.Count, config.Scope.CommitType)
		}
		return resolveLastNRange(ctx, repo, config.Scope.Head, config.Since, config.Scope.Count)
	case "revision_range":
		return resolveRevisionRange(ctx, repo, config.Scope)
	case "since":
		return resolveSinceRange(ctx, repo, config.Scope)
	case "working_tree":
		return resolveWorkingTreeRange(ctx, repo)
	default:
		return rangeResolution{}, fmt.Errorf("unsupported review scope kind %q", config.Scope.Kind)
	}
}

func resolveWorkingTreeRange(ctx context.Context, repo string) (rangeResolution, error) {
	head, err := resolveCommit(ctx, repo, "HEAD")
	if err != nil {
		return rangeResolution{}, fmt.Errorf("resolve working tree base: %w", err)
	}
	return rangeResolution{Range: reviewRange{Start: strings.TrimSpace(head), End: "WORKTREE", Source: "working_tree"}}, nil
}

// resolveRange preserves the direct helper contract used by the producer's
// focused tests while action requests migrate to typed Scope payloads.
func resolveRange(ctx context.Context, repo, since string, maxCommits int) (rangeResolution, error) {
	config := Config{Since: since, MaxCommits: maxCommits, Scope: lastNScope(maxCommits)}
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
	if !containsString(nonEmptyLines(chainText), base) {
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
	var output string
	var err error
	if r.isSelectedCommits() {
		var selectedOutput strings.Builder
		for _, commit := range r.Commits {
			current, diffErr := diffSelectedCommit(ctx, repo, commit, "--name-only", "-z")
			if diffErr != nil {
				return nil, fmt.Errorf("list paths changed by selected commit %q: %w", commit, diffErr)
			}
			selectedOutput.WriteString(current)
		}
		output = selectedOutput.String()
	} else if r.isWorkingTree() {
		output, err = git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", r.Start, "--")
		if err == nil {
			var untracked string
			untracked, err = git(ctx, repo, "ls-files", "--others", "--exclude-standard", "-z")
			output += untracked
		}
	} else {
		output, err = git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", r.Start+".."+r.End)
	}
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
		normalizedPath, err := normalizeTouchedPath(path)
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

func (r reviewRange) isWorkingTree() bool {
	return r.Source == "working_tree"
}

func (r reviewRange) isSelectedCommits() bool {
	return r.Source == "selected_commits"
}

func normalizeTouchedPath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("path must not be empty")
	}
	if len(value) > 256 {
		return "", errors.New("path exceeds 256 bytes")
	}
	if strings.ContainsRune(value, 0) || strings.Contains(value, `\`) {
		return "", errors.New("path must use repository-relative slash-separated syntax")
	}
	if path.IsAbs(value) || strings.ContainsAny(value, "*?[]{}") {
		return "", errors.New("path must be an exact repository-relative path")
	}
	directoryPrefix := strings.HasSuffix(value, "/")
	core := strings.TrimSuffix(value, "/")
	if core == "" || path.Clean(core) != core {
		return "", errors.New("path must not contain empty, dot, or parent segments")
	}
	for _, segment := range strings.Split(core, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("path must not contain empty, dot, or parent segments")
		}
	}
	if core == "*" {
		return "", errors.New("touched path must not be the global selector")
	}
	if directoryPrefix {
		return core + "/", nil
	}
	return core, nil
}

func classifyReviewPath(ctx context.Context, repo string, r reviewRange, path string) string {
	if !isDocumentationPath(path) {
		return "primary"
	}
	lower := strings.ToLower(filepath.ToSlash(path))
	if strings.HasPrefix(lower, "docs/architecture/") ||
		strings.Contains(lower, "security") || strings.Contains(lower, "safety") || strings.Contains(lower, "threat") ||
		strings.Contains(lower, "runtime") || strings.Contains(lower, "contract") {
		return "documentation-risk"
	}
	content, err := reviewPathContent(ctx, repo, r, path)
	if err == nil && strings.Contains(strings.ToLower(content), "> authority: normative") {
		return "documentation-risk"
	}
	if isRoutineDocumentationPath(lower) {
		return "documentation"
	}
	return "documentation-risk"
}

func isDocumentationPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := strings.ToLower(filepath.Base(lower))
	return strings.HasPrefix(lower, "docs/") ||
		strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "release")
}

func isRoutineDocumentationPath(lower string) bool {
	base := filepath.Base(lower)
	if strings.HasPrefix(base, "readme") || strings.HasPrefix(base, "changelog") || strings.HasPrefix(base, "release") {
		return true
	}
	for _, prefix := range []string{"docs/tutorial/", "docs/tutorials/", "docs/guide/", "docs/guides/", "docs/getting-started/", "docs/releases/", "docs/release-notes/"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func verifyDocumentationChanges(ctx context.Context, repo string, r reviewRange, paths []string) (documentationVerification, error) {
	verification := documentationVerification{Passed: true, CheckedFiles: append([]string(nil), paths...)}
	seenLinks := make(map[string]struct{})
	seenPaths := make(map[string]struct{})
	seenSymbols := make(map[string]struct{})
	symbolResolver := newReviewGoSymbolResolver(ctx, repo, r)
	defer symbolResolver.close()
	for _, documentPath := range paths {
		// Archived documents are immutable historical records. Inline paths and
		// symbols describe the repository state that the record was written
		// against, so treating them as live references creates false failures as
		// soon as scratch inputs are removed or implementations are renamed. The
		// documentation reviewer still receives these files through the
		// documentation-risk workset; only this current-tree reference gate is
		// intentionally skipped.
		if strings.HasPrefix(filepath.ToSlash(documentPath), "docs/archive/") {
			continue
		}
		exists, err := reviewPathExists(ctx, repo, r, documentPath)
		if err != nil {
			return verification, err
		}
		if !exists {
			continue
		}
		addedLines, err := addedDocumentationLines(ctx, repo, r, documentPath)
		if err != nil {
			return verification, err
		}
		for _, line := range addedLines {
			for _, match := range markdownLinkPattern.FindAllStringSubmatch(line, -1) {
				target := strings.Trim(match[1], "<>")
				repositoryPath, ok := repositoryLinkPath(documentPath, target)
				if !ok {
					continue
				}
				key := documentPath + "\x00" + repositoryPath
				if _, duplicate := seenLinks[key]; duplicate {
					continue
				}
				seenLinks[key] = struct{}{}
				verification.CheckedLinks++
				found, checkErr := reviewPathExists(ctx, repo, r, repositoryPath)
				if checkErr != nil {
					return verification, checkErr
				}
				if !found {
					verification.Issues = append(verification.Issues, fmt.Sprintf("%s: relative link target %q does not exist at %s", documentPath, target, reviewTargetLabel(r)))
				}
			}
			inlineText := markdownLinkPattern.ReplaceAllString(line, "")
			for _, match := range inlineCodePattern.FindAllStringSubmatch(inlineText, -1) {
				token := strings.TrimSpace(match[1])
				if strings.HasPrefix(token, "workspace:path/") {
					verification.Issues = append(verification.Issues, fmt.Sprintf("%s: %q is not the canonical workspace resource syntax; use workspace:path:<path>", documentPath, token))
					continue
				}
				if repositoryPath, ok := repositoryCodePath(documentPath, token); ok {
					if _, duplicate := seenPaths[repositoryPath]; !duplicate {
						seenPaths[repositoryPath] = struct{}{}
						verification.CheckedPaths++
						found, checkErr := reviewPathExists(ctx, repo, r, repositoryPath)
						if checkErr != nil {
							return verification, checkErr
						}
						if !found {
							verification.Issues = append(verification.Issues, fmt.Sprintf("%s: repository path %q does not exist at %s", documentPath, repositoryPath, reviewTargetLabel(r)))
						}
					}
				}
				if !isGoSymbolCandidate(token) {
					continue
				}
				found, checkErr := symbolResolver.exists(token)
				if checkErr != nil {
					return verification, checkErr
				}
				// A bare CamelCase code span can name a product concept rather than
				// a Go declaration (for example DecisionPrimitive). Count it as a
				// symbol only when it resolves. A package-qualified reference is an
				// unambiguous Go claim and therefore still fails closed when absent.
				if !found && !qualifiedGoSymbol.MatchString(token) {
					continue
				}
				if _, duplicate := seenSymbols[token]; duplicate {
					continue
				}
				seenSymbols[token] = struct{}{}
				verification.CheckedSymbols++
				if !found {
					verification.Issues = append(verification.Issues, fmt.Sprintf("%s: Go symbol %q does not exist at %s", documentPath, token, reviewTargetLabel(r)))
				}
			}
		}
	}
	if len(verification.Issues) > 0 {
		verification.Passed = false
		return verification, fmt.Errorf("documentation_verification_failed:\n%s", strings.Join(verification.Issues, "\n"))
	}
	return verification, nil
}

func addedDocumentationLines(ctx context.Context, repo string, r reviewRange, path string) ([]string, error) {
	diff, err := reviewDiff(ctx, repo, r, path)
	if err != nil {
		return nil, fmt.Errorf("diff documentation %q: %w", path, err)
	}
	lines := make([]string, 0)
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			lines = append(lines, strings.TrimPrefix(line, "+"))
		}
	}
	return lines, nil
}

func repositoryLinkPath(documentPath, target string) (string, bool) {
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return "", false
	}
	if before, _, found := strings.Cut(target, "#"); found {
		target = before
	}
	if before, _, found := strings.Cut(target, "?"); found {
		target = before
	}
	if target == "" {
		return "", false
	}
	joined := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(documentPath), filepath.FromSlash(target))))
	if joined == ".." || strings.HasPrefix(joined, "../") || filepath.IsAbs(joined) {
		return "", false
	}
	return joined, true
}

func repositoryCodePath(documentPath, token string) (string, bool) {
	if before, _, found := strings.Cut(token, "#"); found {
		token = before
	}
	if before, _, found := strings.Cut(token, ":"); found && strings.HasSuffix(before, ".go") {
		token = before
	}
	token = filepath.ToSlash(token)
	if token == "" || strings.ContainsAny(token, " <>*{}()") || strings.Contains(token, "://") || strings.HasPrefix(token, "mailto:") || filepath.IsAbs(token) {
		return "", false
	}
	if !hasExplicitRepositoryPath(token) {
		return "", false
	}
	if strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") {
		token = filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(documentPath), filepath.FromSlash(token))))
	} else {
		token = filepath.ToSlash(filepath.Clean(token))
	}
	if token == "." || token == ".." || strings.HasPrefix(token, "../") {
		return "", false
	}
	base := strings.ToLower(filepath.Base(token))
	if containsString([]string{"go.mod", "go.sum", "agents.md", "makefile", "dockerfile", "hufu.yaml", "hufu.yml"}, base) {
		return token, true
	}
	for _, suffix := range []string{".go", ".md", ".yaml", ".yml", ".json", ".toml", ".sh", ".nu", ".sql"} {
		if strings.HasSuffix(base, suffix) {
			return token, true
		}
	}
	for _, prefix := range []string{"cmd/", "internal/", "docs/", ".agent-teams/", ".agents/", "scripts/"} {
		if strings.HasPrefix(token, prefix) && !strings.Contains(base, ".") {
			return token, true
		}
	}
	return "", false
}

func hasExplicitRepositoryPath(token string) bool {
	return strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") || strings.Contains(token, "/")
}

func isGoSymbolCandidate(token string) bool {
	if qualifiedGoSymbol.MatchString(token) {
		return true
	}
	if !goSymbolPattern.MatchString(token) {
		return false
	}
	uppercase := 0
	hasLowercase := false
	for _, current := range token {
		if unicode.IsUpper(current) {
			uppercase++
		}
		if unicode.IsLower(current) {
			hasLowercase = true
		}
	}
	return uppercase >= 2 && hasLowercase
}

func gitObjectExists(ctx context.Context, repo, object string) (bool, error) {
	cmd := newGitCommand(ctx, repo, "cat-file", "-e", object)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("check git object %q: %w: %s", object, err, strings.TrimSpace(stderr.String()))
}

func reviewPathContent(ctx context.Context, repo string, r reviewRange, path string) (string, error) {
	if !r.isWorkingTree() {
		return git(ctx, repo, "show", r.End+":"+path)
	}
	resolved, err := resolveWorktreePath(repo, path)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func reviewPathExists(ctx context.Context, repo string, r reviewRange, path string) (bool, error) {
	if !r.isWorkingTree() {
		return gitObjectExists(ctx, repo, r.End+":"+path)
	}
	resolved, err := resolveWorktreePath(repo, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_, err = os.Stat(resolved)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func resolveWorktreePath(repo, path string) (string, error) {
	normalized, err := normalizeTouchedPath(path)
	if err != nil {
		return "", err
	}
	resolved, err := canonicalPath(filepath.Join(repo, filepath.FromSlash(normalized)))
	if err != nil {
		return "", err
	}
	if !pathWithin(repo, resolved) {
		return "", fmt.Errorf("worktree path %q escapes repository %q", path, repo)
	}
	return resolved, nil
}

func gitGrepSymbol(ctx context.Context, repo, revision, symbol string) (bool, error) {
	args := []string{"grep"}
	if revision == "" {
		args = append(args, "--untracked")
	}
	args = append(args, "-q", "-F", symbol)
	if revision != "" {
		args = append(args, revision)
	}
	args = append(args, "--", "*.go")
	cmd := newGitCommand(ctx, repo, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("check Go symbol %q: %w: %s", symbol, err, strings.TrimSpace(stderr.String()))
}

func reviewSymbolExists(ctx context.Context, repo string, r reviewRange, symbol string) (bool, error) {
	resolver := newReviewGoSymbolResolver(ctx, repo, r)
	defer resolver.close()
	return resolver.exists(symbol)
}

func newReviewGoSymbolResolver(ctx context.Context, repo string, r reviewRange) *reviewGoSymbolResolver {
	return &reviewGoSymbolResolver{
		ctx: ctx, repo: repo, rangeDef: r,
		packages: make(map[string]loadedGoPackage), standardPackages: make(map[string][]string),
	}
}

func (r *reviewGoSymbolResolver) exists(symbol string) (bool, error) {
	if !qualifiedGoSymbol.MatchString(symbol) {
		if r.rangeDef.isWorkingTree() {
			return gitGrepSymbol(r.ctx, r.repo, "", symbol)
		}
		return gitGrepSymbol(r.ctx, r.repo, r.rangeDef.End, symbol)
	}
	packageName, symbolName, _ := strings.Cut(symbol, ".")
	found, recognized, err := r.importedQualifiedGoSymbolExists(packageName, symbolName)
	if err != nil || found || recognized {
		return found, err
	}
	found, recognized, err = r.standardLibraryQualifiedGoSymbolExists(packageName, symbolName)
	if err != nil || found || recognized {
		return found, err
	}
	if r.rangeDef.isWorkingTree() {
		return worktreeQualifiedGoSymbolExists(r.ctx, r.repo, packageName, symbolName)
	}
	return gitQualifiedGoSymbolExists(r.ctx, r.repo, r.rangeDef.End, packageName, symbolName)
}

// importedQualifiedGoSymbolExists resolves a selector through import
// declarations at the reviewed revision before inspecting the selected
// package's actual source. This distinguishes repository packages such as
// rule.DecideFunc from standard-library and module dependencies such as
// context.Context or yaml.Node without hard-coding package names.
func (r *reviewGoSymbolResolver) importedQualifiedGoSymbolExists(packageName, symbol string) (bool, bool, error) {
	revision := r.rangeDef.End
	if r.rangeDef.isWorkingTree() {
		revision = ""
	}
	paths, err := gitSymbolCandidatePaths(r.ctx, r.repo, revision, packageName+"."+symbol)
	if err != nil {
		return false, false, err
	}
	candidates := make([]goImportCandidate, 0)
	seen := make(map[string]int)
	for _, candidatePath := range paths {
		content, readErr := reviewGoSource(r.ctx, r.repo, r.rangeDef, candidatePath)
		if readErr != nil {
			return false, false, readErr
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), candidatePath, content, parser.ImportsOnly)
		if parseErr != nil {
			return false, false, fmt.Errorf("parse imports for Go symbol candidate %q: %w", candidatePath, parseErr)
		}
		for _, imported := range parsed.Imports {
			importPath, unquoteErr := strconv.Unquote(imported.Path.Value)
			if unquoteErr != nil {
				return false, false, fmt.Errorf("parse import path in %q: %w", candidatePath, unquoteErr)
			}
			explicitAlias := imported.Name != nil
			preferred := false
			if explicitAlias {
				alias := imported.Name.Name
				if alias == "_" || alias == "." || alias != packageName {
					continue
				}
				preferred = true
			} else if path.Base(importPath) != packageName {
				// The declared package name may intentionally differ from the
				// import-path base (for example gopkg.in/yaml.v3 -> yaml).
				// Keep it as a lower-priority candidate and confirm its actual
				// package name through go list below.
				explicitAlias = false
			} else {
				preferred = true
			}
			if index, duplicate := seen[importPath]; duplicate {
				if explicitAlias {
					candidates[index].explicitAlias = true
				}
				candidates[index].preferred = candidates[index].preferred || preferred
				continue
			}
			seen[importPath] = len(candidates)
			candidates = append(candidates, goImportCandidate{path: importPath, explicitAlias: explicitAlias, preferred: preferred})
		}
	}
	recognized := false
	for _, preferred := range []bool{true, false} {
		for _, candidate := range candidates {
			if candidate.preferred != preferred {
				continue
			}
			pkg, loadErr := r.loadGoPackage(candidate.path)
			if loadErr != nil {
				// An unresolved lower-priority import may still declare the
				// requested package name. Do not turn an incomplete lookup into
				// a false "symbol does not exist" result.
				return false, true, loadErr
			}
			if !candidate.explicitAlias && pkg.Name != packageName {
				continue
			}
			recognized = true
			found, declareErr := listedPackageDeclaresGoSymbol(pkg, symbol)
			if declareErr != nil {
				return false, true, declareErr
			}
			if found {
				return true, true, nil
			}
		}
	}
	return false, recognized, nil
}

// standardLibraryQualifiedGoSymbolExists handles valid standard-library
// references that are documented but not otherwise used by repository Go
// files. go list supplies the package-name mapping, so paths such as
// encoding/json are resolved without a language- or package-specific table.
func (r *reviewGoSymbolResolver) standardLibraryQualifiedGoSymbolExists(packageName, symbol string) (bool, bool, error) {
	if err := r.loadStandardPackages(); err != nil {
		return false, false, err
	}
	paths := r.standardPackages[packageName]
	for _, importPath := range paths {
		pkg := r.packages[importPath].value
		found, err := listedPackageDeclaresGoSymbol(pkg, symbol)
		if err != nil {
			return false, true, err
		}
		if found {
			return true, true, nil
		}
	}
	return false, len(paths) > 0, nil
}

func (r *reviewGoSymbolResolver) loadStandardPackages() error {
	if r.standardLoaded {
		return r.standardErr
	}
	r.standardLoaded = true
	goListDir, err := r.goListDirectory()
	if err != nil {
		r.standardErr = err
		return r.standardErr
	}
	cmd := exec.CommandContext(r.ctx, "go", "list", "-mod=readonly", "-json", "--", "std")
	cmd.Dir = goListDir
	cmd.Env = r.goListEnvironment(goListDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		r.standardErr = fmt.Errorf("resolve Go standard-library packages: %w: %s", err, strings.TrimSpace(stderr.String()))
		return r.standardErr
	}
	decoder := json.NewDecoder(&stdout)
	for {
		var pkg listedGoPackage
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			r.standardErr = fmt.Errorf("decode Go standard-library package metadata: %w", err)
			return r.standardErr
		}
		if strings.TrimSpace(pkg.Dir) == "" || strings.TrimSpace(pkg.Name) == "" || strings.TrimSpace(pkg.ImportPath) == "" {
			continue
		}
		r.packages[pkg.ImportPath] = loadedGoPackage{value: pkg}
		r.standardPackages[pkg.Name] = append(r.standardPackages[pkg.Name], pkg.ImportPath)
	}
	return nil
}

func reviewGoSource(ctx context.Context, repo string, r reviewRange, candidatePath string) ([]byte, error) {
	if !r.isWorkingTree() {
		content, err := git(ctx, repo, "show", r.End+":"+candidatePath)
		if err != nil {
			return nil, fmt.Errorf("read Go symbol candidate %q: %w", candidatePath, err)
		}
		return []byte(content), nil
	}
	resolved, err := resolveWorktreePath(repo, candidatePath)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read Go symbol candidate %q: %w", candidatePath, err)
	}
	return content, nil
}

func (r *reviewGoSymbolResolver) loadGoPackage(importPath string) (listedGoPackage, error) {
	if cached, ok := r.packages[importPath]; ok {
		return cached.value, cached.err
	}
	goListDir, err := r.goListDirectory()
	if err != nil {
		r.packages[importPath] = loadedGoPackage{err: err}
		return listedGoPackage{}, err
	}
	cmd := exec.CommandContext(r.ctx, "go", "list", "-mod=readonly", "-json", "--", importPath)
	cmd.Dir = goListDir
	cmd.Env = r.goListEnvironment(goListDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("resolve imported Go package %q without network access: %w: %s", importPath, err, strings.TrimSpace(stderr.String()))
		r.packages[importPath] = loadedGoPackage{err: err}
		return listedGoPackage{}, err
	}
	var pkg listedGoPackage
	if err := json.Unmarshal(stdout.Bytes(), &pkg); err != nil {
		err = fmt.Errorf("decode imported Go package %q metadata: %w", importPath, err)
		r.packages[importPath] = loadedGoPackage{err: err}
		return listedGoPackage{}, err
	}
	if strings.TrimSpace(pkg.Dir) == "" || strings.TrimSpace(pkg.Name) == "" {
		err = fmt.Errorf("imported Go package %q has incomplete metadata", importPath)
		r.packages[importPath] = loadedGoPackage{err: err}
		return listedGoPackage{}, err
	}
	r.packages[importPath] = loadedGoPackage{value: pkg}
	return pkg, nil
}

// goListDirectory returns the live worktree for worktree reviews and an
// isolated snapshot of the reviewed revision for commit-range reviews. This
// keeps go list and its source-file reads from observing unrelated dirty
// changes or later commits in the caller's checkout.
func (r *reviewGoSymbolResolver) goListDirectory() (string, error) {
	if r.rangeDef.isWorkingTree() {
		return r.repo, nil
	}
	if r.resolutionErr != nil {
		return "", r.resolutionErr
	}
	if r.resolutionDir != "" {
		return r.resolutionDir, nil
	}
	directory, err := os.MkdirTemp("", "hufu-reviewprep-revision-*")
	if err != nil {
		r.resolutionErr = fmt.Errorf("create Go symbol resolution snapshot: %w", err)
		return "", r.resolutionErr
	}
	if err := archiveGitRevision(r.ctx, r.repo, r.rangeDef.End, directory); err != nil {
		_ = os.RemoveAll(directory)
		r.resolutionErr = err
		return "", err
	}
	r.resolutionDir = directory
	return directory, nil
}

func (r *reviewGoSymbolResolver) goListEnvironment(directory string) []string {
	environment := offlineGoEnvironment()
	if r.rangeDef.isWorkingTree() {
		return environment
	}
	goWork := filepath.Join(directory, "go.work")
	if _, err := os.Stat(goWork); err == nil {
		return replaceEnvironmentValue(environment, "GOWORK", goWork)
	}
	return replaceEnvironmentValue(environment, "GOWORK", "off")
}

func (r *reviewGoSymbolResolver) close() {
	if r.resolutionDir != "" {
		_ = os.RemoveAll(r.resolutionDir)
	}
}

func archiveGitRevision(ctx context.Context, repo, revision, destination string) error {
	command := newGitCommand(ctx, repo, "archive", "--format=tar", revision)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Go symbol resolution archive: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Go symbol resolution archive for %q: %w", revision, err)
	}
	extractErr := extractGitArchive(stdout, destination)
	if extractErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if extractErr != nil {
		return fmt.Errorf("extract Go symbol resolution snapshot for %q: %w", revision, extractErr)
	}
	if waitErr != nil {
		return fmt.Errorf("archive Go symbol resolution revision %q: %w: %s", revision, waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractGitArchive(source io.Reader, destination string) error {
	reader := tar.NewReader(source)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}
		name := filepath.FromSlash(header.Name)
		if header.Typeflag == tar.TypeXGlobalHeader {
			// git archive emits a pax_global_header entry that carries no file
			// content; skip it instead of failing the snapshot extraction.
			continue
		}
		cleanName := filepath.Clean(name)
		if filepath.IsAbs(name) || cleanName == "." || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry path %q escapes the snapshot", header.Name)
		}
		target := filepath.Join(destination, cleanName)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create archived directory %q: %w", cleanName, err)
			}
		case tar.TypeReg:
			if err := writeArchivedFile(target, header, reader); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := createArchivedSymlink(target, cleanName, header.Linkname); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %d for %q", header.Typeflag, cleanName)
		}
	}
}

func writeArchivedFile(target string, header *tar.Header, source io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent directory for archived file %q: %w", header.Name, err)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, header.FileInfo().Mode().Perm())
	if err != nil {
		return fmt.Errorf("create archived file %q: %w", header.Name, err)
	}
	if _, err := io.Copy(file, source); err != nil {
		_ = file.Close()
		return fmt.Errorf("write archived file %q: %w", header.Name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close archived file %q: %w", header.Name, err)
	}
	return nil
}

func createArchivedSymlink(target, name, linkname string) error {
	linkPath := filepath.FromSlash(linkname)
	if filepath.IsAbs(linkPath) {
		return fmt.Errorf("archive symlink %q has absolute target %q", name, linkname)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(name), linkPath))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive symlink %q escapes the snapshot", name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent directory for archived symlink %q: %w", name, err)
	}
	if err := os.Symlink(linkPath, target); err != nil {
		return fmt.Errorf("create archived symlink %q: %w", name, err)
	}
	return nil
}

func offlineGoEnvironment() []string {
	environment := replaceEnvironmentValue(os.Environ(), "GOPROXY", "off")
	environment = replaceEnvironmentValue(environment, "GOSUMDB", "off")
	return replaceEnvironmentValue(environment, "GOTOOLCHAIN", "local")
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	updated := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			updated = append(updated, entry)
		}
	}
	return append(updated, prefix+value)
}

func listedPackageDeclaresGoSymbol(pkg listedGoPackage, symbol string) (bool, error) {
	files := make([]string, 0, len(pkg.GoFiles)+len(pkg.CgoFiles))
	files = append(files, pkg.GoFiles...)
	files = append(files, pkg.CgoFiles...)
	for _, name := range files {
		if filepath.Base(name) != name {
			return false, fmt.Errorf("go package %q returned unsafe source path %q", pkg.ImportPath, name)
		}
		filename := filepath.Join(pkg.Dir, name)
		content, err := os.ReadFile(filename)
		if err != nil {
			return false, fmt.Errorf("read imported Go package %q source %q: %w", pkg.ImportPath, name, err)
		}
		found, err := sourceDeclaresGoSymbol(filename, content, pkg.Name, symbol)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func gitQualifiedGoSymbolExists(ctx context.Context, repo, revision, packageName, symbol string) (bool, error) {
	paths, err := gitSymbolCandidatePaths(ctx, repo, revision, symbol)
	if err != nil {
		return false, err
	}
	for _, candidatePath := range paths {
		content, readErr := git(ctx, repo, "show", revision+":"+candidatePath)
		if readErr != nil {
			return false, fmt.Errorf("read Go symbol candidate %q: %w", candidatePath, readErr)
		}
		found, parseErr := sourceDeclaresGoSymbol(candidatePath, []byte(content), packageName, symbol)
		if parseErr != nil {
			return false, parseErr
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func worktreeQualifiedGoSymbolExists(ctx context.Context, repo, packageName, symbol string) (bool, error) {
	paths, err := gitSymbolCandidatePaths(ctx, repo, "", symbol)
	if err != nil {
		return false, err
	}
	for _, candidatePath := range paths {
		resolved, resolveErr := resolveWorktreePath(repo, candidatePath)
		if resolveErr != nil {
			return false, resolveErr
		}
		content, readErr := os.ReadFile(resolved)
		if readErr != nil {
			return false, fmt.Errorf("read Go symbol candidate %q: %w", candidatePath, readErr)
		}
		found, parseErr := sourceDeclaresGoSymbol(candidatePath, content, packageName, symbol)
		if parseErr != nil {
			return false, parseErr
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func gitSymbolCandidatePaths(ctx context.Context, repo, revision, symbol string) ([]string, error) {
	args := []string{"grep"}
	if revision == "" {
		args = append(args, "--untracked")
	}
	args = append(args, "-l", "-F", symbol)
	if revision != "" {
		args = append(args, revision)
	}
	args = append(args, "--", "*.go")
	cmd := newGitCommand(ctx, repo, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("find Go symbol candidates for %q: %w: %s", symbol, err, strings.TrimSpace(stderr.String()))
	}
	prefix := revision + ":"
	paths := make([]string, 0)
	for _, line := range strings.Split(stdout.String(), "\n") {
		candidatePath := strings.TrimSpace(line)
		if revision != "" {
			candidatePath = strings.TrimPrefix(candidatePath, prefix)
		}
		if candidatePath != "" {
			paths = append(paths, candidatePath)
		}
	}
	return paths, nil
}

func sourceDeclaresGoSymbol(filename string, content []byte, packageName, symbol string) (bool, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), filename, content, 0)
	if err != nil {
		return false, fmt.Errorf("parse Go symbol candidate %q: %w", filename, err)
	}
	if parsed.Name.Name != packageName {
		return false, nil
	}
	for _, declaration := range parsed.Decls {
		switch current := declaration.(type) {
		case *ast.FuncDecl:
			if current.Recv == nil && current.Name.Name == symbol {
				return true, nil
			}
		case *ast.GenDecl:
			for _, spec := range current.Specs {
				switch declared := spec.(type) {
				case *ast.TypeSpec:
					if declared.Name.Name == symbol {
						return true, nil
					}
				case *ast.ValueSpec:
					for _, name := range declared.Names {
						if name.Name == symbol {
							return true, nil
						}
					}
				}
			}
		}
	}
	return false, nil
}

func reviewTargetLabel(r reviewRange) string {
	if r.isWorkingTree() {
		return "working tree"
	}
	return r.End
}

func buildBatches(ctx context.Context, repo string, r reviewRange, paths []string, config Config) ([]*batch, error) {
	var batches []*batch
	for _, path := range paths {
		diff, err := reviewDiff(ctx, repo, r, path)
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

func reviewDiff(ctx context.Context, repo string, r reviewRange, path string) (string, error) {
	if r.isSelectedCommits() {
		var selectedDiff strings.Builder
		for _, commit := range r.Commits {
			diff, err := diffSelectedCommit(ctx, repo, commit, "--", path)
			if err != nil {
				return "", fmt.Errorf("diff selected commit %q: %w", commit, err)
			}
			selectedDiff.WriteString(diff)
		}
		return selectedDiff.String(), nil
	}
	if !r.isWorkingTree() {
		return git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", r.Start+".."+r.End, "--", path)
	}
	diff, err := git(ctx, repo, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", r.Start, "--", path)
	if err != nil || diff != "" {
		return diff, err
	}
	fullPath := filepath.Join(repo, filepath.FromSlash(path))
	if _, err := os.Lstat(fullPath); err != nil {
		return "", err
	}
	diff, err = gitWithExitCodeOne(ctx, repo, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", "--", os.DevNull, path)
	if err != nil {
		return "", err
	}
	if diff == "" {
		return fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n", path, path), nil
	}
	return diff, nil
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
	case isDocumentationPath(path):
		if isRoutineDocumentationPath(strings.ToLower(filepath.ToSlash(path))) {
			return "documentation"
		}
		return "documentation-risk"
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
	cmd := newGitCommand(ctx, dir, args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func gitWithExitCodeOne(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := newGitCommand(ctx, dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return stdout.String(), nil
	}
	return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
}

func newGitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	guardedArgs := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false",
	}
	guardedArgs = append(guardedArgs, args...)
	cmd := exec.CommandContext(ctx, "git", guardedArgs...)
	cmd.Dir = dir
	cmd.Env = sanitizedGitEnvironment()
	return cmd
}

func sanitizedGitEnvironment() []string {
	env := make([]string, 0, 12)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "HOME", "LANG", "PATH", "SYSTEMROOT", "TEMP", "TMP", "TMPDIR":
			env = append(env, entry)
		default:
			if strings.HasPrefix(key, "LC_") {
				env = append(env, entry)
			}
		}
	}
	return append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	)
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

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
