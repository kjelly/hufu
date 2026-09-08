package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// WorkspaceFileState is one file's observed identity at snapshot time
// (docs/hufu-external-coding-agent-runtime-spec.md §11.2). Path is always a
// workspace-relative, forward-slash-normalized path.
type WorkspaceFileState struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Mode   uint32 `json:"mode"`
}

// WorkspaceSnapshot is the durable-safe identity of a captured workspace
// state. The full per-file manifest is not part of this exported shape —
// only a digest/reference travels into events (§11.2) — but Diff needs the
// actual per-file state, so a snapshot retains it in memory for the
// lifetime of the attempt that captured it.
type WorkspaceSnapshot struct {
	ID          string
	Root        string
	CapturedAt  time.Time
	ManifestRef string
	Digest      string

	files map[string]WorkspaceFileState
}

// fileState looks up one path's observed state in this snapshot.
func (s WorkspaceSnapshot) fileState(path string) (WorkspaceFileState, error) {
	state, ok := s.files[path]
	if !ok {
		return WorkspaceFileState{}, fmt.Errorf("workspace snapshot: %q was not observed", path)
	}
	return state, nil
}

// WorkspaceDelta is the observed change between two snapshots of the same
// root, ordered by path for deterministic comparison/serialization.
type WorkspaceDelta struct {
	Added    []WorkspaceFileState `json:"added,omitempty"`
	Modified []WorkspaceFileState `json:"modified,omitempty"`
	Deleted  []string             `json:"deleted,omitempty"`
}

// WorkspaceSnapshotter captures and diffs workspace state
// (docs/hufu-external-coding-agent-runtime-spec.md §11.1). Snapshot takes a
// plain root path rather than *PreparedExecutionWorld: that type does not
// exist until the ExecutionWorld phase (§10, PR-06). When it lands, callers
// pass prepared.Root/CWD here unchanged; this signature is expected to gain
// a *PreparedExecutionWorld overload (or replace this one) at that point,
// not to be redesigned.
type WorkspaceSnapshotter interface {
	Snapshot(ctx context.Context, root string) (WorkspaceSnapshot, error)
	Diff(ctx context.Context, before, after WorkspaceSnapshot) (WorkspaceDelta, error)
}

// workspaceSnapshotMaxFiles bounds a single snapshot (§11.4 "enforce a
// maximum file count / total metadata budget"). It is deliberately generous
// for real repositories while still bounding a runaway/unbounded workspace.
const workspaceSnapshotMaxFiles = 200000

var workspaceSnapshotSeq atomic.Uint64

func newWorkspaceSnapshotID() string {
	return fmt.Sprintf("wss-%d-%d", time.Now().UnixNano(), workspaceSnapshotSeq.Add(1))
}

// defaultWorkspaceSnapshotter is the only WorkspaceSnapshotter implementation.
// It transparently prefers Git-assisted candidate discovery (§11.3) when the
// root is a Git working tree and falls back to a full directory walk (§11.4)
// otherwise — the same safety rules (symlink escape, file-count budget,
// actual-byte hashing) apply to both paths.
type defaultWorkspaceSnapshotter struct {
	maxFiles int
}

// NewWorkspaceSnapshotter returns the default WorkspaceSnapshotter.
func NewWorkspaceSnapshotter() WorkspaceSnapshotter {
	return &defaultWorkspaceSnapshotter{maxFiles: workspaceSnapshotMaxFiles}
}

func (s *defaultWorkspaceSnapshotter) Snapshot(ctx context.Context, root string) (WorkspaceSnapshot, error) {
	cleanRoot, err := resolveSnapshotRoot(root)
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: %w", err)
	}
	maxFiles := s.maxFiles
	if maxFiles <= 0 {
		maxFiles = workspaceSnapshotMaxFiles
	}

	var files map[string]WorkspaceFileState
	if candidates, ok := gitCandidateFiles(ctx, cleanRoot); ok {
		files, err = hashCandidateFiles(cleanRoot, candidates, maxFiles)
	} else {
		files, err = walkAndHashWorkspace(ctx, cleanRoot, maxFiles)
	}
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: %w", err)
	}

	return WorkspaceSnapshot{
		ID: newWorkspaceSnapshotID(), Root: cleanRoot, CapturedAt: time.Now(),
		Digest: workspaceManifestDigest(files), files: files,
	}, nil
}

func (s *defaultWorkspaceSnapshotter) Diff(_ context.Context, before, after WorkspaceSnapshot) (WorkspaceDelta, error) {
	var delta WorkspaceDelta
	for path, afterState := range after.files {
		beforeState, existed := before.files[path]
		if !existed {
			delta.Added = append(delta.Added, afterState)
			continue
		}
		if beforeState.SHA256 != afterState.SHA256 || beforeState.Bytes != afterState.Bytes || beforeState.Mode != afterState.Mode {
			delta.Modified = append(delta.Modified, afterState)
		}
	}
	for path := range before.files {
		if _, still := after.files[path]; !still {
			delta.Deleted = append(delta.Deleted, path)
		}
	}
	sort.Slice(delta.Added, func(i, j int) bool { return delta.Added[i].Path < delta.Added[j].Path })
	sort.Slice(delta.Modified, func(i, j int) bool { return delta.Modified[i].Path < delta.Modified[j].Path })
	sort.Strings(delta.Deleted)
	return delta, nil
}

func resolveSnapshotRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", fmt.Errorf("workspace root is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root %q is not a directory", root)
	}
	return resolved, nil
}

// isWithinRoot reports whether resolved (already symlink-evaluated) is root
// itself or a descendant of it.
func isWithinRoot(root, resolved string) bool {
	if resolved == root {
		return true
	}
	return strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// workspaceInternalDirs are skipped by the directory walk: they are Hufu's
// own bookkeeping, never a provider's or worker's deliverable, and walking
// them wastes the file-count budget on churn this attempt did not produce.
var workspaceInternalDirs = map[string]bool{
	".git":     true,
	tasksDir:   true,
	sharedDir:  true,
	statusDir:  true,
	historyDir: true,
	logsDir:    true,
}

func walkAndHashWorkspace(ctx context.Context, root string, maxFiles int) (map[string]WorkspaceFileState, error) {
	files := make(map[string]WorkspaceFileState)
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if workspaceInternalDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			resolved, evalErr := filepath.EvalSymlinks(path)
			if evalErr != nil {
				return fmt.Errorf("resolve symlink %q: %w", rel, evalErr)
			}
			if !isWithinRoot(root, resolved) {
				return fmt.Errorf("symlink %q escapes the workspace root", rel)
			}
		}
		count++
		if count > maxFiles {
			return fmt.Errorf("exceeds the maximum snapshot file budget (%d)", maxFiles)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		sum, size, hashErr := hashFileContents(path)
		if hashErr != nil {
			return fmt.Errorf("hash %q: %w", rel, hashErr)
		}
		files[rel] = WorkspaceFileState{Path: rel, SHA256: sum, Bytes: size, Mode: uint32(info.Mode().Perm())}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func hashCandidateFiles(root string, candidates []string, maxFiles int) (map[string]WorkspaceFileState, error) {
	files := make(map[string]WorkspaceFileState, len(candidates))
	if len(candidates) > maxFiles {
		return nil, fmt.Errorf("exceeds the maximum snapshot file budget (%d)", maxFiles)
	}
	for _, rel := range candidates {
		rel = filepath.ToSlash(rel)
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				// A candidate git reported can race with a concurrent delete;
				// it is simply absent from this snapshot, not an error.
				continue
			}
			return nil, fmt.Errorf("stat %q: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, evalErr := filepath.EvalSymlinks(full)
			if evalErr != nil {
				return nil, fmt.Errorf("resolve symlink %q: %w", rel, evalErr)
			}
			if !isWithinRoot(root, resolved) {
				return nil, fmt.Errorf("symlink %q escapes the workspace root", rel)
			}
			full = resolved
			info, err = os.Stat(full)
			if err != nil {
				return nil, fmt.Errorf("stat %q: %w", rel, err)
			}
		}
		if info.IsDir() {
			continue
		}
		sum, size, err := hashFileContents(full)
		if err != nil {
			return nil, fmt.Errorf("hash %q: %w", rel, err)
		}
		files[rel] = WorkspaceFileState{Path: rel, SHA256: sum, Bytes: size, Mode: uint32(info.Mode().Perm())}
	}
	return files, nil
}

func hashFileContents(path string) (sha256Hex string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// hashWorkspaceFile hashes one workspace-relative path from disk, re-validating
// containment even for an already-sanitized relative path: a symlink
// component encountered while resolving it could still escape root.
func hashWorkspaceFile(root, relPath string) (sha256Hex string, size int64, err error) {
	if strings.TrimSpace(root) == "" {
		return "", 0, fmt.Errorf("workspace root is unavailable")
	}
	full := filepath.Join(root, filepath.FromSlash(relPath))
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", 0, fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", 0, err
	}
	if !isWithinRoot(resolvedRoot, resolved) {
		return "", 0, fmt.Errorf("path %q escapes the authorized workspace", relPath)
	}
	return hashFileContents(resolved)
}

func workspaceManifestDigest(files map[string]WorkspaceFileState) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, path := range paths {
		state := files[path]
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", state.Path, state.SHA256, state.Bytes, state.Mode)
	}
	return hex.EncodeToString(h.Sum(nil))
}
