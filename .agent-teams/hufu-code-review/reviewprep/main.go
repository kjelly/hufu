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
	"sort"
	"strings"
)

const manifestSchemaVersion = 1

type actionRequest struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// Config intentionally contains only team-owned workset semantics. Hufu
// passes it as an opaque Action payload and does not interpret these fields.
type Config struct {
	Repository   string `json:"repository"`
	OutputDir    string `json:"output_dir"`
	ArtifactRoot string `json:"artifact_root"`
	Since        string `json:"since"`
	MaxCommits   int    `json:"max_commits"`
	MaxDiffBytes int    `json:"max_diff_bytes"`
	MaxDiffLines int    `json:"max_diff_lines"`
	MaxPaths     int    `json:"max_paths"`
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
	SchemaVersion int         `json:"schema_version"`
	Range         reviewRange `json:"range"`
	ChangedFiles  int         `json:"changed_files"`
	Items         []item      `json:"items"`
}

type reviewRange struct {
	Start       string `json:"start"`
	End         string `json:"end"`
	Since       string `json:"since"`
	CommitCount int    `json:"commit_count"`
}

type item struct {
	Key       string            `json:"key"`
	Lens      string            `json:"lens"`
	Bindings  map[string]string `json:"bindings"`
	Paths     []string          `json:"paths"`
	Inputs    []artifact        `json:"inputs,omitempty"`
	DiffPath  string            `json:"diff_path"`
	DiffSHA   string            `json:"diff_sha256"`
	DiffBytes int               `json:"diff_bytes"`
	DiffLines int               `json:"diff_lines"`
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
	var request actionRequest
	if err := json.NewDecoder(in).Decode(&request); err != nil {
		return fmt.Errorf("decode action request: %w", err)
	}
	if request.Type != "prepare_review_workset" && request.Type != "prepare" {
		return fmt.Errorf("unsupported action type %q", request.Type)
	}
	var config Config
	if err := json.Unmarshal([]byte(request.Payload), &config); err != nil {
		return fmt.Errorf("decode action payload: %w", err)
	}
	result, err := Prepare(ctx, config)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
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

	reviewRangeValue, err := resolveRange(ctx, repo, config.Since, config.MaxCommits)
	if err != nil {
		return actionResult{}, err
	}
	paths, err := changedPaths(ctx, repo, reviewRangeValue)
	if err != nil {
		return actionResult{}, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return actionResult{}, fmt.Errorf("create output directory: %w", err)
	}

	items, err := buildItems(ctx, repo, artifactRoot, outputDir, reviewRangeValue, paths, config)
	if err != nil {
		return actionResult{}, err
	}
	manifestPath := filepath.Join(outputDir, "workset-manifest.json")
	m := manifest{SchemaVersion: manifestSchemaVersion, Range: reviewRangeValue, ChangedFiles: len(paths), Items: items}
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
		"manifest_path": manifestArtifact.Path,
		"range_start":   reviewRangeValue.Start,
		"range_end":     reviewRangeValue.End,
		"commit_count":  reviewRangeValue.CommitCount,
		"changed_files": len(paths),
		"item_count":    len(items),
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
	if config.MaxCommits < 0 {
		return errors.New("max_commits must not be negative")
	}
	if config.MaxDiffBytes <= 0 || config.MaxDiffLines <= 0 || config.MaxPaths <= 0 {
		return errors.New("max_diff_bytes, max_diff_lines, and max_paths must be positive")
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

func resolveRange(ctx context.Context, repo, since string, maxCommits int) (reviewRange, error) {
	end, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return reviewRange{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	commitsText, err := git(ctx, repo, "rev-list", "--reverse", "--since="+since, "HEAD")
	if err != nil {
		return reviewRange{}, fmt.Errorf("list commits: %w", err)
	}
	commits := nonEmptyLines(commitsText)
	if maxCommits > 0 && len(commits) > maxCommits {
		commits = commits[len(commits)-maxCommits:]
	}
	r := reviewRange{End: strings.TrimSpace(end), Since: since, CommitCount: len(commits)}
	if len(commits) == 0 {
		return r, nil
	}
	start, err := git(ctx, repo, "rev-parse", commits[0]+"^")
	if err != nil {
		return reviewRange{}, fmt.Errorf("resolve parent of first selected commit: %w", err)
	}
	r.Start = strings.TrimSpace(start)
	verified, err := git(ctx, repo, "rev-list", "--count", r.Start+".."+r.End)
	if err != nil {
		return reviewRange{}, fmt.Errorf("verify commit count: %w", err)
	}
	if strings.TrimSpace(verified) != fmt.Sprintf("%d", r.CommitCount) {
		return reviewRange{}, fmt.Errorf("commit range count mismatch: expected %d, got %s", r.CommitCount, strings.TrimSpace(verified))
	}
	return r, nil
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
		seen[path] = struct{}{}
	}
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func buildItems(ctx context.Context, repo, artifactRoot, outputDir string, r reviewRange, paths []string, config Config) ([]item, error) {
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
			Bindings: map[string]string{"key": key, "lens": current.lens},
			Paths:    append([]string(nil), current.paths...),
			Inputs:   []artifact{diffArtifact},
			DiffPath: filepath.ToSlash(filepath.Join("batches", key, "diff.patch")),
			DiffSHA:  sha256Hex(current.diff.Bytes()), DiffBytes: current.diff.Len(), DiffLines: current.lines,
		})
	}
	return items, nil
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
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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
