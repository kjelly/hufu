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
			file.SHA256, file.Bytes = sha256Hex([]byte(target)), int64(len(target))
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
// a file list, never a fabricated patch. Complete source is in the source bundle.
func buildSnapshotBatches(ctx context.Context, repo string, r reviewRange, paths []string, config Config) ([]*batch, error) {
	batches := []*batch{}
	var current *batch
	group := ""
	sourceBytes := 0
	for _, name := range paths {
		content, err := git(ctx, repo, "show", r.End+":"+name)
		if err != nil {
			return nil, err
		}
		lens := lensForPath(name)
		if isDocumentationPath(name) {
			lens = "documentation-risk"
		}
		nextGroup := filepath.ToSlash(filepath.Dir(name)) + "/" + lens
		if current == nil || group != nextGroup || len(current.paths) >= config.MaxPaths || sourceBytes+len(content)+96*1024 > config.MaxSourceSnapshotBytes || estimateReviewInputTokens(sourceBytes+len(content)+96*1024) > config.MaxReviewInputTokens {
			current = &batch{lens: lens, pathSeen: make(map[string]struct{})}
			batches = append(batches, current)
			group, sourceBytes = nextGroup, 0
		}
		unit := []byte(fmt.Sprintf("snapshot_file %s\n", name))
		current.add(name, unit, 1)
		sourceBytes += len(content)
	}
	return batches, nil
}

func prepareRepositorySnapshot(ctx context.Context, repo, artifactRoot, outputDir string, resolution rangeResolution, config Config) (actionResult, error) {
	// Enforce the same review/critic bound after source-budget refinement.
	if config.MaxWorksetItems > 32 {
		config.MaxWorksetItems = 32
	}
	paths, err := repositoryTreePaths(ctx, repo, resolution.Range.End)
	if err != nil {
		return actionResult{}, err
	}
	snapshot, err := createReviewSourceSnapshot(ctx, repo, resolution.Range, paths)
	if err != nil {
		return actionResult{}, err
	}
	defer snapshot.Close()
	identity, err := repositorySnapshotInventory(ctx, repo, snapshot)
	if err != nil {
		return actionResult{}, err
	}
	snapshot.identity = identity
	candidates := []string{}
	for index := range identity.Files {
		file := &identity.Files[index]
		if file.Disposition == "excluded" {
			continue
		}
		// Leave room for framing and bounded caller/test context. Large files stay
		// visible as unfinished coverage instead of being truncated or excluded.
		if file.Bytes > int64(maxSnapshotContentBytes) || file.Bytes+96*1024 > int64(config.MaxSourceSnapshotBytes) || estimateReviewInputTokens(int(file.Bytes)+96*1024) > config.MaxReviewInputTokens {
			file.Disposition, file.Reason = "unreviewed", "source_exceeds_item_budget"
		} else {
			candidates = append(candidates, file.Path)
		}
	}
	batches, err := buildSnapshotBatches(ctx, repo, resolution.Range, candidates, config)
	if err != nil {
		return actionResult{}, err
	}
	perBatch := config.MaxWorksetItems - 2 // reserve the two required no-op routes
	if perBatch > 30 {
		perBatch = 30
	} // at most 32 sources plus their critics fit synthesis
	if perBatch < 1 {
		return actionResult{}, fmt.Errorf("snapshot requires at least three workset slots")
	}
	batchNumber := config.Scope.Batch
	if batchNumber == 0 {
		batchNumber = 1
	}
	count := (len(batches) + perBatch - 1) / perBatch
	if count == 0 {
		count = 1
	}
	if batchNumber > count {
		return actionResult{}, fmt.Errorf("snapshot batch %d exceeds batch_count %d", batchNumber, count)
	}
	selected := make(map[string]bool)
	start, end := (batchNumber-1)*perBatch, batchNumber*perBatch
	if end > len(batches) {
		end = len(batches)
	}
	if start > len(batches) {
		start = len(batches)
	}
	for _, workset := range batches[start:end] {
		for _, name := range workset.paths {
			selected[name] = true
		}
	}
	identity.Batch, identity.BatchCount, identity.WorksetsPerBatch = batchNumber, count, perBatch
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
	// An all-oversized inventory is still an honest, empty review with coverage
	// gaps; route noops preserve the inventory rather than declaring it clean.
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return actionResult{}, err
	}
	data, err := json.MarshalIndent(identity, "", "  ")
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
	inventoryArtifact.Description = "complete repository snapshot inventory and exclusions"
	result, err := prepareRoutedReview(ctx, repo, artifactRoot, outputDir, selectedPaths, resolution, selectedCommitDiffPlan{}, snapshot, config)
	if err != nil {
		return actionResult{}, err
	}
	result.Artifacts = append(result.Artifacts, inventoryArtifact)
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

// Only a current, completed producer from the same frozen invocation can pin
// the verifier. No model-supplied path, revision, or reconstructed receipt is used.
func frozenRepositorySnapshot(requested resolverScope) (*repositorySnapshotManifest, error) {
	workspace := os.Getenv("HUFU_WORKSPACE")
	if workspace == "" {
		return nil, nil
	} // direct helper callers have no runtime state
	runID, actionID := os.Getenv("HUFU_RUN_ID"), os.Getenv("HUFU_ACTION_INVOCATION_ID")
	root := filepath.Clean(workspace)
	for count := 0; count < 5; count++ {
		root = filepath.Dir(root)
	}
	if !filepath.IsAbs(workspace) || runID == "" || actionID == "" || filepath.Join(root, "runtime", "runs", runID, "actions", actionID) != workspace {
		return nil, fmt.Errorf("unsupported verifier action workspace")
	}
	data, err := actionstate.SessionCheckpoint(root)
	if err != nil {
		return nil, err
	}
	var current struct {
		SnapshotID string `json:"active_run_input_snapshot_id"`
		Tasks      []struct {
			ID         string `json:"ID"`
			Status     string `json:"Status"`
			ContractID string `json:"contract_id"`
			SnapshotID string `json:"run_input_snapshot_id"`
			Receipt    *struct {
				RunID  string `json:"run_id"`
				TaskID string `json:"task_id"`
			} `json:"execution_receipt"`
			Result *struct {
				Status  string                     `json:"status"`
				Outputs map[string]json.RawMessage `json:"runtime_outputs"`
			} `json:"typed_result"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	var frozen *repositorySnapshotManifest
	for _, task := range current.Tasks {
		if task.ContractID != "produce-workset" {
			continue
		}
		if frozen != nil || task.Status != "done" || task.Result == nil || task.Result.Status != "success" || current.SnapshotID == "" || task.SnapshotID != current.SnapshotID || task.Receipt == nil || task.Receipt.RunID != runID || task.Receipt.TaskID != task.ID {
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
