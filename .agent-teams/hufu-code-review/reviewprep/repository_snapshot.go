package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/actionstate"
)

const maxSnapshotInventoryBytes = 32 * 1024 * 1024 // bounded read by reviewgate

// These wire types use json v1 and exported, non-generic helpers because the
// trusted-static action interpreter does not expose json/v2 or Go iterators.
type snapshotFile struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	Lines       int    `json:"lines"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

type repositorySnapshotManifest struct {
	ID               string         `json:"snapshot_id"`
	Revision         string         `json:"revision"`
	ContentSHA256    string         `json:"content_sha256"`
	Files            []snapshotFile `json:"files"`
	Batch            int            `json:"batch"`
	BatchCount       int            `json:"batch_count"`
	WorksetsPerBatch int            `json:"worksets_per_batch"`
	FileCount        int            `json:"file_count"`
	Plan             *snapshotPlan  `json:"workset_plan,omitempty"`
	Inventory        *artifact      `json:"inventory,omitempty"`
}

// The full plan lives in the receipt-bound inventory artifact. Source hashes
// refer to the selected page's actual artifacts in page-local workset order.
type snapshotPlan struct {
	Worksets          []snapshotPlannedWorkset `json:"worksets"`
	BatchSourceSHA256 []string                 `json:"batch_source_sha256"`
}

type snapshotPlannedWorkset struct {
	Sources []snapshotPlannedSource `json:"sources"`
}

type snapshotPlannedSource struct {
	Path  string         `json:"path"`
	Spans []snapshotSpan `json:"spans"`
}

type snapshotSpan struct {
	Start     int64 `json:"start"`
	End       int64 `json:"end"`
	StartLine int   `json:"start_line"`
	EndLine   int   `json:"end_line"`
}

func (r reviewRange) isRepositorySnapshot() bool { return r.Source == "repository_snapshot" }

func repositoryTreePaths(ctx context.Context, repo, revision string) ([]string, error) {
	output, err := git(ctx, repo, "ls-tree", "-r", "--name-only", "-z", revision)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, name := range strings.Split(output, "\x00") {
		if name == "" {
			continue
		}
		clean, err := cleanSnapshotPath(name)
		if err != nil || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n\t") {
			return nil, fmt.Errorf("unsupported snapshot path %q", name)
		}
		paths = append(paths, clean)
	}
	sort.Strings(paths)
	return paths, nil
}

// Identity hashes the entire tracked inventory, including excluded files and
// modes. Dispositions and pagination do not affect content identity.
func repositorySnapshotInventory(ctx context.Context, repo string, snapshot *reviewSourceSnapshot) (*repositorySnapshotManifest, error) {
	output, err := git(ctx, repo, "ls-tree", "-r", "-z", snapshot.revision)
	if err != nil {
		return nil, err
	}
	result := &repositorySnapshotManifest{Revision: snapshot.revision, Files: []snapshotFile{}}
	digest := sha256.New()
	for _, row := range strings.Split(output, "\x00") {
		if row == "" {
			continue
		}
		metadata, name, ok := strings.Cut(row, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || !utf8.ValidString(name) || strings.ContainsAny(name, "\r\n\t") {
			return nil, fmt.Errorf("unsupported tree entry")
		}
		clean, err := cleanSnapshotPath(name)
		if err != nil {
			return nil, err
		}
		file := snapshotFile{Path: clean, Mode: fields[0], Disposition: "expected"}
		switch fields[0] {
		case "160000":
			file.SHA256 = sha256Hex([]byte(fields[2]))
			file.Disposition, file.Reason = "excluded", "submodule"
		case "120000":
			// Unsafe external links may be intentionally omitted by git archive.
			// Hash the tracked link blob without dereferencing any target.
			target, err := git(ctx, repo, "show", snapshot.revision+":"+clean)
			if err != nil {
				return nil, err
			}
			file.SHA256 = sha256Hex([]byte(target))
			file.Bytes = int64(len(target))
			file.Disposition, file.Reason = "excluded", "symlink"
		case "100644", "100755":
			if err := inspectSnapshotFile(snapshot.root, &file); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported snapshot mode %s", fields[0])
		}
		identity, err := json.Marshal([]any{file.Path, file.Mode, file.SHA256, file.Bytes})
		if err != nil {
			return nil, err
		}
		if _, err := digest.Write(append(identity, '\n')); err != nil {
			return nil, err
		}
		result.Files = append(result.Files, file)
	}
	result.ContentSHA256 = hex.EncodeToString(digest.Sum(nil))
	result.ID = "sha256:" + result.ContentSHA256
	result.FileCount = len(result.Files)
	return result, nil
}

func inspectSnapshotFile(root string, file *snapshotFile) error {
	handle, err := os.Open(filepath.Join(root, filepath.FromSlash(file.Path)))
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	prefix := []byte{}
	last := byte('\n')
	binary := false
	carry := []byte{}
	for {
		count, readErr := handle.Read(buffer)
		if count > 0 {
			data := buffer[:count]
			if bytes.IndexByte(data, 0) >= 0 {
				binary = true
			}
			text := append(carry, data...)
			for len(text) > 0 {
				if !utf8.FullRune(text) {
					break
				}
				r, size := utf8.DecodeRune(text)
				if r == utf8.RuneError && size == 1 {
					binary = true
				}
				text = text[size:]
			}
			carry = append([]byte{}, text...)
			if len(prefix) == 0 {
				prefix = append(prefix, data...)
			}
			if _, err := hash.Write(data); err != nil {
				return err
			}
			file.Bytes += int64(count)
			file.Lines += bytes.Count(data, []byte{'\n'})
			last = data[len(data)-1]
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if file.Bytes > 0 && last != '\n' {
		file.Lines++
	}
	file.SHA256 = hex.EncodeToString(hash.Sum(nil))
	if len(carry) > 0 {
		binary = true
	}
	switch {
	case strings.Contains("/"+file.Path+"/", "/vendor/"), strings.Contains("/"+file.Path+"/", "/node_modules/"), strings.Contains("/"+file.Path+"/", "/third_party/"):
		file.Disposition, file.Reason = "excluded", "dependency"
	case binary:
		file.Disposition, file.Reason = "excluded", "binary"
	case strings.HasSuffix(file.Path, ".go") && generatedSnapshotSource(prefix):
		file.Disposition, file.Reason = "excluded", "generated"
	case strings.HasPrefix(file.Path, "docs/") || (strings.HasSuffix(file.Path, ".md") && !strings.HasPrefix(file.Path, ".agent-teams/") && filepath.Base(file.Path) != "AGENTS.md"):
		file.Disposition, file.Reason = "excluded", "documentation_outside_code_audit"
	}
	return nil
}

func generatedSnapshotSource(prefix []byte) bool {
	for _, line := range strings.Split(string(prefix), "\n") {
		if strings.HasPrefix(line, "package ") {
			break
		}
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}

func eligibleSnapshotPaths(identity *repositorySnapshotManifest) []string {
	paths := []string{}
	for _, file := range identity.Files {
		if file.Disposition != "excluded" {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

// Every inventory unit is grouped by package/directory and lens. The payload is
// a file list. Assigned source ranges are carried by the source bundle.
func buildSnapshotBatches(files []snapshotFile, config Config) []*batch {
	batches := []*batch{}
	var current *batch
	group := ""
	var sourceBytes int64
	for _, file := range files {
		if file.Disposition == "excluded" {
			continue
		}
		name := file.Path
		lens := lensForPath(name)
		if isDocumentationPath(name) {
			lens = "documentation-risk"
		}
		nextGroup := filepath.ToSlash(filepath.Dir(name)) + "/" + lens
		projected := sourceBytes + file.Bytes + 96*1024
		if current == nil || group != nextGroup || len(current.paths) >= config.MaxPaths || projected > int64(config.MaxSourceSnapshotBytes) || estimateReviewInputTokens(int(projected)) > config.MaxReviewInputTokens {
			current = &batch{lens: lens, pathSeen: make(map[string]struct{})}
			batches = append(batches, current)
			group, sourceBytes = nextGroup, 0
		}
		unit := []byte(fmt.Sprintf("snapshot_file %s\n", name))
		current.add(name, unit, 1)
		sourceBytes += file.Bytes
	}
	return batches
}

func planSnapshotWorksets(ctx context.Context, artifactRoot, primaryDir string, snapshot *reviewSourceSnapshot, config Config, verification *documentationVerification, batchNumber, perBatch int) (*snapshotPlan, []*batch, error) {
	candidates := buildSnapshotBatches(snapshot.identity.Files, config)
	annotateBatches(candidates, "primary")
	plan := &snapshotPlan{Worksets: []snapshotPlannedWorkset{}, BatchSourceSHA256: []string{}}
	selected := []*batch{}
	next := func() (int, error) {
		return len(plan.Worksets) % perBatch, ctx.Err()
	}
	accept := func(current *batch, primary []sourceEvidence) error {
		workset := snapshotPlannedWorkset{Sources: []snapshotPlannedSource{}}
		for _, evidence := range primary {
			if evidence.Status != "present" || evidence.Truncated || evidence.Omitted {
				return fmt.Errorf("snapshot primary source %q is incomplete", evidence.Path)
			}
			source := snapshotPlannedSource{Path: evidence.Path, Spans: []snapshotSpan{}}
			for _, span := range evidence.AvailableSpans {
				source.Spans = append(source.Spans, snapshotSpan{Start: span.Start, End: span.End, StartLine: span.StartLine, EndLine: span.EndLine})
			}
			workset.Sources = append(workset.Sources, source)
		}
		if len(plan.Worksets)/perBatch+1 == batchNumber {
			selected = append(selected, current)
			plan.BatchSourceSHA256 = append(plan.BatchSourceSHA256, sha256Hex(current.sourceBundle))
		}
		plan.Worksets = append(plan.Worksets, workset)
		return nil
	}
	for index, candidate := range candidates {
		if err := visitBoundReviewBatch(candidate, artifactRoot, primaryDir, snapshot, config, "primary", verification, next, accept); err != nil {
			return nil, nil, err
		}
		// Retain metadata for every page and full evidence for the requested
		// page only, including when a coarse candidate fitted without a split.
		candidates[index] = nil
	}
	return plan, selected, nil
}

func prepareRepositorySnapshot(ctx context.Context, repo, artifactRoot, outputDir string, resolution rangeResolution, config Config) (actionResult, error) {
	perBatch := config.MaxWorksetItems - 2 // reserve the two required no-op routes
	if perBatch > 30 {
		perBatch = 30
	}
	if perBatch < 1 {
		return actionResult{}, fmt.Errorf("snapshot requires at least three workset slots")
	}
	batchNumber := config.Scope.Batch
	if batchNumber == 0 {
		batchNumber = 1
	}
	snapshot, err := createReviewSourceSnapshot(ctx, repo, resolution.Range, nil)
	if err != nil {
		return actionResult{}, err
	}
	defer snapshot.Close()
	identity, err := repositorySnapshotInventory(ctx, repo, snapshot)
	if err != nil {
		return actionResult{}, err
	}
	snapshot.identity = identity
	verification := documentationVerification{Passed: true, CheckedFiles: []string{}, ReviewedRevision: resolution.Range.End}
	plan, batches, err := planSnapshotWorksets(ctx, artifactRoot, filepath.Join(outputDir, "primary"), snapshot, config, &verification, batchNumber, perBatch)
	if err != nil {
		return actionResult{}, err
	}
	count := (len(plan.Worksets) + perBatch - 1) / perBatch
	if count == 0 {
		count = 1
	}
	if batchNumber > count {
		return actionResult{}, fmt.Errorf("snapshot batch %d exceeds batch_count %d", batchNumber, count)
	}
	selected := make(map[string]bool)
	for _, workset := range batches {
		for _, name := range workset.paths {
			selected[name] = true
		}
	}
	identity.Batch, identity.BatchCount, identity.WorksetsPerBatch = batchNumber, count, perBatch
	identity.Plan = plan
	selectedPaths := []string{}
	for index := range identity.Files {
		file := &identity.Files[index]
		if file.Disposition == "excluded" || file.Disposition == "unreviewed" {
			continue
		}
		if selected[file.Path] {
			file.Disposition = "selected"
			selectedPaths = append(selectedPaths, file.Path)
		} else {
			file.Disposition, file.Reason = "unreviewed", "outside_selected_batch"
		}
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return actionResult{}, err
	}
	if len(data)+1 > maxSnapshotInventoryBytes {
		return actionResult{}, fmt.Errorf("snapshot_inventory_too_large: %d bytes; inventory limit is %d", len(data)+1, maxSnapshotInventoryBytes)
	}
	// Empty eligible inventories retain their exclusions through the no-op routes.
	worksets := []routedWorkset{
		{name: "primary", description: "primary review workset manifest", paths: selectedPaths, batches: batches, verification: verification},
		{name: "documentation", description: "documentation review workset manifest", verification: verification},
		{name: "documentation-escalation", description: "documentation escalation workset manifest", verification: verification},
	}
	for index := range worksets {
		route := &worksets[index]
		if len(route.batches) == 0 {
			route.batches, err = boundReviewBatches(artifactRoot, filepath.Join(outputDir, route.name), []*batch{noopBatch(route.name)}, snapshot, config, route.name, &verification)
			if err != nil {
				return actionResult{}, err
			}
		}
	}
	result, err := publishRoutedReview(artifactRoot, outputDir, selectedPaths, resolution, snapshot, config, worksets, verification)
	if err != nil {
		return actionResult{}, err
	}
	inventoryPath := filepath.Join(outputDir, "repository-snapshot.json")
	if err := os.WriteFile(inventoryPath, append(data, '\n'), 0o644); err != nil {
		return actionResult{}, err
	}
	inventoryArtifact, err := fileArtifact(artifactRoot, inventoryPath, "repository_snapshot_inventory")
	if err != nil {
		return actionResult{}, err
	}
	inventoryArtifact.Description = "complete repository snapshot inventory, exclusions, and workset plan"
	result.Artifacts = append(result.Artifacts, inventoryArtifact)
	// Keep full membership in the sealed artifact and workset manifests. The
	// receipt-bound output carries only identity, counts and its exact reference.
	scope := result.Outputs["scope"].(scopeAttestation)
	compact := *identity
	compact.Files = nil
	compact.Plan = nil
	compact.Inventory = &inventoryArtifact
	scope.RepositorySnapshot = &compact
	result.Outputs["scope"] = scope
	encoded, err := json.Marshal(result)
	if err != nil {
		return actionResult{}, err
	}
	if len(encoded) > 900*1024 {
		return actionResult{}, fmt.Errorf("snapshot_inventory_too_large: %d bytes; split the repository audit into smaller repositories/modules before execution", len(encoded))
	}
	return result, nil
}

func snapshotWorksetBindings(snapshot *reviewSourceSnapshot, key, lens string) map[string]string {
	bindings := map[string]string{"key": key, "lens": lens, "review_revision": snapshot.revision}
	if snapshot.identity == nil {
		return bindings
	}
	bindings["review_mode"] = "repository_snapshot"
	bindings["snapshot_id"] = snapshot.identity.ID
	bindings["content_sha256"] = snapshot.identity.ContentSHA256
	return bindings
}

func compactScopeContext(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var scope scopeAttestation
	if json.Unmarshal(data, &scope) != nil {
		return nil
	}
	if scope.RepositorySnapshot == nil {
		return scope
	}
	identity := *scope.RepositorySnapshot
	identity.Files = []snapshotFile{}
	scope.RepositorySnapshot = &identity
	return scope
}

// A completed producer from this invocation or a runtime-authorized recovery
// origin pins the verifier. No model-supplied source identity is used.
func frozenRepositorySnapshot(ctx context.Context, requested resolverScope) (*repositorySnapshotManifest, error) {
	workspace := os.Getenv("HUFU_WORKSPACE")
	if workspace == "" {
		return nil, nil
	} // direct helper callers have no runtime state
	data, err := actionstate.ActionSources(ctx)
	if err != nil {
		return nil, err
	}
	var current struct {
		Version int              `json:"version"`
		Tasks   []supplementTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	if current.Version != 1 {
		return nil, fmt.Errorf("unsupported source view version")
	}
	var frozen *repositorySnapshotManifest
	for _, task := range current.Tasks {
		if task.ContractID != "produce-workset" {
			continue
		}
		if frozen != nil || !task.VerifiedOutputs || task.Result == nil {
			return nil, fmt.Errorf("producer is missing, stale, or ambiguous")
		}

		var scope scopeAttestation
		if err := json.Unmarshal(task.Result.Outputs["scope"], &scope); err != nil {
			return nil, err
		}
		if scope.Requested != requested || scope.RequestedInputHash != scopeInputDigest(requested) || !scope.Satisfied || scope.RepositorySnapshot == nil || scope.RepositorySnapshot.ID == "" || scope.RepositorySnapshot.Revision != scope.Resolved.Head {
			return nil, fmt.Errorf("producer snapshot scope mismatch")
		}
		frozen = scope.RepositorySnapshot
	}
	if frozen == nil {
		return nil, fmt.Errorf("accepted producer snapshot missing")
	}
	return frozen, nil
}
