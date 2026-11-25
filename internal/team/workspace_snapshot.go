package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// WorkspaceFileState is one file's observed identity at snapshot time
// (docs/architecture/execution-runtime.md). Path is always a
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

	rootInfo fs.FileInfo
	files    map[string]WorkspaceFileState
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
// (docs/architecture/execution-runtime.md). Snapshot takes a
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
	cleanRoot, rootInfo, err := resolveSnapshotDirectory(root)
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: %w", err)
	}
	directory, err := os.Open(cleanRoot)
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: open root: %w", err)
	}
	defer func() { _ = directory.Close() }()
	opened, err := directory.Stat()
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: stat root: %w", err)
	}
	if !os.SameFile(rootInfo, opened) {
		return WorkspaceSnapshot{}, errWorkspaceSnapshotRootChanged
	}
	return s.snapshotPinned(ctx, root, cleanRoot, directory, nil)
}

// Linux exec changes directory before applying close-on-exec, so both a
// child process and the parent can use the held directory without publishing
// a descriptor in the child's environment or descriptor table.
func pinnedDirectoryPath(directory *os.File, fallback string) string {
	if runtime.GOOS == "linux" && directory != nil {
		return fmt.Sprintf("/proc/self/fd/%d", directory.Fd())
	}
	return fallback
}

func (s *defaultWorkspaceSnapshotter) snapshotPinned(ctx context.Context, root, cleanRoot string, directory *os.File, ignoredPaths []string) (WorkspaceSnapshot, error) {
	rootInfo, err := directory.Stat()
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: %w", err)
	}
	verifyRoot := func() error {
		currentRoot, currentInfo, err := resolveSnapshotDirectory(root)
		if err != nil {
			return fmt.Errorf("workspace snapshot: recheck root: %w", err)
		}
		if currentRoot != cleanRoot || !os.SameFile(rootInfo, currentInfo) {
			return errWorkspaceSnapshotRootChanged
		}
		return nil
	}
	if err := verifyRoot(); err != nil {
		return WorkspaceSnapshot{}, err
	}
	ignored, err := normalizeWorkspaceSnapshotIgnoredPaths(ignoredPaths)
	if err != nil {
		return WorkspaceSnapshot{}, err
	}
	maxFiles := s.maxFiles
	if maxFiles <= 0 {
		maxFiles = workspaceSnapshotMaxFiles
	}

	var files map[string]WorkspaceFileState
	scanRoot := pinnedDirectoryPath(directory, cleanRoot)
	if candidates, ok := gitCandidateFiles(ctx, scanRoot); ok {
		files, err = hashCandidateFiles(scanRoot, candidates, maxFiles, ignored)
	} else {
		// The final dot makes WalkDir enter the directory denoted by the
		// procfs descriptor instead of visiting the descriptor link.
		files, err = walkAndHashWorkspace(ctx, scanRoot+string(filepath.Separator)+".", maxFiles, ignored)
	}
	if err != nil {
		return WorkspaceSnapshot{}, fmt.Errorf("workspace snapshot: %w", err)
	}
	if err := verifyRoot(); err != nil {
		return WorkspaceSnapshot{}, err
	}

	return WorkspaceSnapshot{
		ID: newWorkspaceSnapshotID(), Root: cleanRoot, CapturedAt: time.Now(),
		Digest: workspaceManifestDigest(files), rootInfo: rootInfo, files: files,
	}, nil
}

func normalizeWorkspaceSnapshotIgnoredPaths(paths []string) (map[string]bool, error) {
	ignored := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("workspace snapshot: ignored path %q is not workspace-relative", path)
		}
		ignored[clean] = true
	}
	return ignored, nil
}

func (s *defaultWorkspaceSnapshotter) Diff(_ context.Context, before, after WorkspaceSnapshot) (WorkspaceDelta, error) {
	if before.Root != after.Root || !os.SameFile(before.rootInfo, after.rootInfo) {
		return WorkspaceDelta{}, errWorkspaceSnapshotRootChanged
	}
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

var errWorkspaceSnapshotRootChanged = errors.New("workspace snapshot: root directory identity changed")

func resolveSnapshotRoot(root string) (string, error) {
	resolved, _, err := resolveSnapshotDirectory(root)
	return resolved, err
}

func resolveSnapshotDirectory(root string) (string, fs.FileInfo, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", nil, fmt.Errorf("workspace root is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", nil, fmt.Errorf("workspace root %q is not a directory", root)
	}
	return resolved, info, nil
}

// isWithinRoot reports whether resolved (already symlink-evaluated) is root
// itself or a descendant of it.
func isWithinRoot(root, resolved string) bool {
	if resolved == root {
		return true
	}
	return strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// hufuControlWorkspaceSnapshotPaths returns only the checkpoint paths owned by
// the explicitly identified Hufu control workspace: runtime.sqlite with its
// SQLite sidecars, which change during every run. A project file with one of
// these names is not special unless it is exactly this derived path.
func hufuControlWorkspaceSnapshotPaths(root, controlWorkspace string) ([]string, error) {
	if strings.TrimSpace(controlWorkspace) == "" {
		return nil, nil
	}
	controlRoot, err := resolveSnapshotRoot(controlWorkspace)
	if err != nil {
		return nil, fmt.Errorf("execution world: resolve control workspace: %w", err)
	}
	if !isWithinRoot(root, controlRoot) {
		return nil, nil
	}
	rel, err := filepath.Rel(root, controlRoot)
	if err != nil {
		return nil, fmt.Errorf("execution world: derive control workspace path: %w", err)
	}
	var paths []string
	for _, name := range runtimestore.Files {
		paths = append(paths, filepath.ToSlash(filepath.Join(rel, name)))
	}
	return paths, nil
}

// Capture the link's stored destination without reading or resolving it.
// This observes directory links, dangling links, and retargeting equally.
func workspaceSymlinkState(path, rel string) (WorkspaceFileState, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return WorkspaceFileState{}, fmt.Errorf("read symlink %q: %w", rel, err)
	}
	h := sha256.New()
	h.Write([]byte("workspace-snapshot:symlink:"))
	h.Write([]byte(target))
	return WorkspaceFileState{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: uint32(fs.ModeSymlink)}, nil
}

// specialFileState represents a FIFO, socket, or device. Opening one could
// block forever (a FIFO without a writer) or have side effects, so its
// content is never read; a placeholder identity derived only from its type
// still lets Diff detect it appearing, disappearing, or changing type.
func specialFileState(rel string, mode fs.FileMode) WorkspaceFileState {
	h := sha256.New()
	h.Write([]byte("workspace-snapshot:special-file:"))
	h.Write([]byte(mode.Type().String()))
	return WorkspaceFileState{Path: rel, SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: 0, Mode: uint32(mode.Type() | mode.Perm())}
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

// isWorkspaceInternalPath reports whether rel (forward-slash, workspace-
// relative) falls under any workspaceInternalDirs entry at any depth —
// gitCandidateFiles' equivalent of walkAndHashWorkspace's fs.SkipDir on a
// matching directory name. Exact Hufu checkpoint paths are supplied
// separately by the execution-world boundary.
//
// **Fixed 2026-09-08** (found running the real §38 smoke suite against a
// genuine account for the first time, against a real git-backed scratch
// repository — every fake-server unit test's workspace is a plain temp dir
// with no .git, so none of them ever exercised the git-assisted snapshot
// path at all): gitCandidateFiles previously had no such exclusion, so
// Hufu's own logs/event_store.jsonl (created by the very same Coordinator
// running the attempt) was a legitimate git "untracked file" candidate.
// Every append Hufu made to its own event log during a real attempt then
// showed up as an external "Modified" delta entry — indistinguishable from
// a provider write — and a read-only task (empty WritableRoots) failed
// closed on its own coordinator's bookkeeping. Confirmed via a synthetic
// reproduction before this fix: two live Snapshot calls around one raw
// append to logs/event_store.jsonl in a git-initialized temp dir reported
// exactly that file Modified.
func isWorkspaceInternalPath(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if workspaceInternalDirs[segment] {
			return true
		}
	}
	return false
}

func isWorkspaceBookkeepingPath(rel string, ignored map[string]bool) bool {
	return ignored[rel] || isWorkspaceInternalPath(rel)
}

func walkAndHashWorkspace(ctx context.Context, root string, maxFiles int, ignored map[string]bool) (map[string]WorkspaceFileState, error) {
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
		if isWorkspaceBookkeepingPath(rel, ignored) {
			return nil
		}
		count++
		if count > maxFiles {
			return fmt.Errorf("exceeds the maximum snapshot file budget (%d)", maxFiles)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, err := workspaceSymlinkState(path, rel)
			if err == nil {
				files[rel] = link
			}
			return err
		}
		if !d.Type().IsRegular() {
			files[rel] = specialFileState(rel, d.Type())
			return nil
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

func hashCandidateFiles(root string, candidates []string, maxFiles int, ignored map[string]bool) (map[string]WorkspaceFileState, error) {
	files := make(map[string]WorkspaceFileState, len(candidates))
	directories := map[string]bool{".": true}
	if len(candidates) > maxFiles {
		return nil, fmt.Errorf("exceeds the maximum snapshot file budget (%d)", maxFiles)
	}
	for _, rel := range candidates {
		rel = filepath.ToSlash(rel)
		if isWorkspaceBookkeepingPath(rel, ignored) {
			continue
		}
		visible, err := observeGitCandidateParent(root, filepath.Dir(rel), directories, files)
		if err != nil {
			return nil, err
		}
		if !visible {
			continue
		}
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
			link, err := workspaceSymlinkState(full, rel)
			if err != nil {
				return nil, err
			}
			files[rel] = link
			continue
		}
		if info.IsDir() {
			continue
		}
		if !info.Mode().IsRegular() {
			files[rel] = specialFileState(rel, info.Mode())
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

// A tracked path can appear in Git's index after a parent directory was
// replaced by a link. Observe the link at that parent and skip its subtree.
func observeGitCandidateParent(root, rel string, directories map[string]bool, files map[string]WorkspaceFileState) (bool, error) {
	if visible, exists := directories[rel]; exists {
		return visible, nil
	}
	parentVisible, err := observeGitCandidateParent(root, filepath.Dir(rel), directories, files)
	if err != nil {
		return false, err
	}
	directories[rel] = false
	if !parentVisible {
		return false, nil
	}
	path := filepath.Join(root, rel)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat candidate directory %q: %w", rel, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		link, err := workspaceSymlinkState(path, filepath.ToSlash(rel))
		if err == nil {
			files[link.Path] = link
		}
		return false, err
	}
	directories[rel] = info.IsDir()
	return directories[rel], nil
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
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("path %q is not a regular file", relPath)
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
