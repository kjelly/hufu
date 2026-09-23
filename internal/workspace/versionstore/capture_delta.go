package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

// CaptureFromDelta records req.Parent's tree with delta applied as a new
// pending snapshot (docs/archive/implementation-plans/workspace-versioning.md
// §15). Only the changed paths are read, and only their ancestor trees are
// rewritten; every other subtree keeps its hash.
//
// It is a library-only primitive in v1 (D1): the runtime always uses full
// capture, because a delta only describes the subject root when nothing else
// wrote it during the observation. The caller must also guarantee that the
// delta reports type changes (§15.2). A changed regular file whose bytes no
// longer hash to the observed SHA-256, a deleted path that exists again, or a
// changed path that is gone fails with ErrWorkspaceChangedAfterObservation
// instead of capturing later bytes. Paths the inclusion policy excludes are
// skipped, as a full capture would; a delta that edits .hufuignore or a
// .gitignore is refused. The stat cache is left untouched.
func (s *Store) CaptureFromDelta(ctx context.Context, req CaptureRequest, delta ObservedDelta) (Snapshot, CaptureStats, error) {
	if err := s.requireWritable(); err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	if req.Parent == "" {
		return Snapshot{}, CaptureStats{}, errors.New("capture from delta: a published parent snapshot is required")
	}
	plan, err := newCapturePlan(ctx, s.db, req, s.now())
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	parent, err := s.GetSnapshot(ctx, req.Parent)
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	changed := append(append([]ObservedFile(nil), delta.Added...), delta.Modified...)
	if err = rejectPolicyChanges(changed, delta.Deleted); err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	filter, err := newDeltaFilter(ctx, plan, changed)
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	root, err := os.OpenRoot(plan.root)
	if err != nil {
		return Snapshot{}, CaptureStats{}, fmt.Errorf("open workspace root: %w", err)
	}
	defer func() { _ = root.Close() }()
	tree, err := s.decodeDeltaTree(parent.RootTreeHash)
	if err != nil {
		return Snapshot{}, CaptureStats{}, fmt.Errorf("load parent tree: %w", err)
	}
	run := &captureRun{store: s, plan: plan, root: root}
	if err = run.applyDeletions(tree, filter, delta.Deleted); err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	if err = run.applyChanges(ctx, tree, filter, changed); err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	rootHash, err := run.storeDeltaTree(ctx, tree, true)
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	if err = run.revalidate(); err != nil {
		return Snapshot{}, CaptureStats{}, fmt.Errorf("%w: %v", ErrWorkspaceChangedAfterObservation, err)
	}
	leaves, err := s.summarizeTree(ctx, plan, rootHash)
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	snapshot, err := s.pendingSnapshot(plan, rootHash, leaves)
	if err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	if err = s.withTx(ctx, func(tx *sql.Tx) error { return insertSnapshot(ctx, tx, snapshot) }); err != nil {
		return Snapshot{}, CaptureStats{}, err
	}
	run.stats.Duration = s.now().Sub(plan.started)
	return snapshot, run.stats, nil
}

// applyDeletions removes deleted paths from the tree. Ignore rules do not
// apply: a path that is gone is gone from a full capture too. A deleted path
// may exist again only as a directory (a file replaced by a directory, whose
// files the delta lists as added).
func (r *captureRun) applyDeletions(tree *deltaTree, filter deltaFilter, deleted []string) error {
	for _, rel := range deleted {
		segments, err := deltaPathSegments(rel)
		if err != nil {
			return err
		}
		if filter.structurallyExcluded(rel) {
			continue
		}
		info, statErr := r.root.Lstat(filepath.FromSlash(rel))
		switch {
		case statErr == nil && !info.IsDir():
			return fmt.Errorf("%w: deleted path %q exists", ErrWorkspaceChangedAfterObservation, rel)
		case statErr == nil:
		case !errors.Is(statErr, fs.ErrNotExist) && !errors.Is(statErr, syscall.ENOTDIR):
			return fmt.Errorf("stat %q: %w", rel, statErr)
		}
		if err = r.store.removeDeltaEntry(tree, segments); err != nil {
			return err
		}
	}
	return nil
}

func (r *captureRun) applyChanges(ctx context.Context, tree *deltaTree, filter deltaFilter, changed []ObservedFile) error {
	realDirs := map[string]bool{}
	for _, observed := range changed {
		segments, err := deltaPathSegments(observed.Path)
		if err != nil {
			return err
		}
		if filter.excluded(observed.Path) {
			continue
		}
		if err = r.requireRealParents(observed.Path, realDirs); err != nil {
			return err
		}
		captured, gone, err := r.captureLeaf(ctx, observed.Path)
		if err != nil {
			return err
		}
		if gone {
			return fmt.Errorf("%w: %q is no longer a file", ErrWorkspaceChangedAfterObservation, observed.Path)
		}
		if captured.entry.Kind == EntryBlob && !strings.EqualFold(captured.entry.ObjectHash, observed.SHA256) {
			return fmt.Errorf("%w: %q no longer has the observed content", ErrWorkspaceChangedAfterObservation, observed.Path)
		}
		r.leaves = append(r.leaves, captured)
		if err = r.store.setDeltaEntry(tree, segments, captured.entry); err != nil {
			return err
		}
	}
	return nil
}

// requireRealParents fails when a changed path lies below a symlink: a full
// capture records the symlink itself, never paths through it.
func (r *captureRun) requireRealParents(rel string, checked map[string]bool) error {
	dirs := parentDirs(rel)
	for index := len(dirs) - 1; index >= 0; index-- {
		dir := dirs[index]
		if checked[dir] {
			continue
		}
		info, err := r.root.Lstat(filepath.FromSlash(dir))
		if err != nil {
			return fmt.Errorf("%w: parent %q of %q: %v", ErrWorkspaceChangedAfterObservation, dir, rel, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %q lies below %q, which is not a directory", ErrWorkspaceChangedAfterObservation, rel, dir)
		}
		checked[dir] = true
	}
	return nil
}

// rejectPolicyChanges fails a delta that edits an ignore file: the unchanged
// paths it would include or exclude are not in the delta, so only a full
// capture can apply the new policy.
func rejectPolicyChanges(changed []ObservedFile, deleted []string) error {
	paths := append([]string(nil), deleted...)
	for _, observed := range changed {
		paths = append(paths, observed.Path)
	}
	for _, rel := range paths {
		if rel == HufuignoreFile || path.Base(rel) == ".gitignore" {
			return fmt.Errorf("capture from delta: %q changes the inclusion policy; use a full capture", rel)
		}
	}
	return nil
}

// deltaPathSegments validates a caller-supplied root-relative path.
func deltaPathSegments(rel string) ([]string, error) {
	if !utf8.ValidString(rel) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedPathName, rel)
	}
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("capture from delta: %q is not a clean root-relative path", rel)
	}
	segments := strings.Split(rel, "/")
	for _, segment := range segments {
		if err := validateEntryName(segment); err != nil {
			return nil, fmt.Errorf("capture from delta %q: %w", rel, err)
		}
	}
	return segments, nil
}

// deltaFilter applies the inclusion policy of a full capture to the paths of
// one delta, without listing the whole subject root.
type deltaFilter struct {
	policy  inclusionPolicy
	gitMode bool
	nested  nestedRepoFinder
	ignore  *hufuIgnore
	ignored map[string]bool
}

func newDeltaFilter(ctx context.Context, plan capturePlan, changed []ObservedFile) (deltaFilter, error) {
	filter := deltaFilter{policy: plan.policy, gitMode: isGitWorkTree(ctx, plan.root)}
	if !filter.gitMode {
		ignore, err := loadHufuignore(plan.root, plan.policy.requireHufuignore)
		if err != nil {
			return deltaFilter{}, err
		}
		filter.ignore = ignore
		return filter, nil
	}
	filter.nested = nestedRepoFinder{root: plan.root, known: map[string]bool{}}
	var candidates []string
	for _, observed := range changed {
		if _, err := deltaPathSegments(observed.Path); err == nil && !filter.structurallyExcluded(observed.Path) {
			candidates = append(candidates, observed.Path)
		}
	}
	ignored, err := gitIgnored(ctx, plan.root, candidates)
	if err != nil {
		return deltaFilter{}, err
	}
	filter.ignored = ignored
	return filter, nil
}

// structurallyExcluded covers the exclusions that do not depend on ignore
// rules: .git, materialize temp files, excluded subtrees, and (Git mode)
// nested repositories and submodules.
func (f deltaFilter) structurallyExcluded(rel string) bool {
	if hasGitSegment(rel) || isMaterializeTemp(rel) || underAny(rel, f.policy.excludeSubtrees) {
		return true
	}
	if f.gitMode {
		_, nested := f.nested.enclosingRepo(rel)
		return nested
	}
	return false
}

func (f deltaFilter) excluded(rel string) bool {
	if f.structurallyExcluded(rel) {
		return true
	}
	if f.gitMode {
		return f.ignored[rel]
	}
	return f.ignore.matches(rel, false)
}

// gitIgnored returns the paths Git ignores. Tracked files are never
// reported, matching the candidate list of a full Git-mode capture.
func gitIgnored(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-z", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil // nothing is ignored
	}
	if err != nil {
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	ignored := map[string]bool{}
	for _, rel := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if rel != "" {
			ignored[rel] = true
		}
	}
	return ignored, nil
}

// deltaTree is a parent tree decoded only along changed paths. entries holds
// leaves and still-encoded subtrees; children holds decoded subtrees.
type deltaTree struct {
	hash     string
	entries  map[string]TreeEntry
	children map[string]*deltaTree
	dirty    bool
}

func newDeltaTree() *deltaTree {
	return &deltaTree{entries: map[string]TreeEntry{}, children: map[string]*deltaTree{}}
}

func (s *Store) decodeDeltaTree(hash string) (*deltaTree, error) {
	tree, err := s.readTree(hash)
	if err != nil {
		return nil, err
	}
	node := newDeltaTree()
	node.hash = hash
	for _, entry := range tree.Entries {
		node.entries[entry.Name] = entry
	}
	return node, nil
}

// subtree returns the decoded directory name of node. With create it makes
// a new directory, replacing a leaf of that name; without it, it returns nil
// when name is not a directory.
func (s *Store) subtree(node *deltaTree, name string, create bool) (*deltaTree, error) {
	if child := node.children[name]; child != nil {
		return child, nil
	}
	if entry, ok := node.entries[name]; ok && entry.Kind == EntryTree {
		child, err := s.decodeDeltaTree(entry.ObjectHash)
		if err != nil {
			return nil, err
		}
		delete(node.entries, name)
		node.children[name] = child
		return child, nil
	}
	if !create {
		return nil, nil
	}
	delete(node.entries, name)
	child := newDeltaTree()
	child.dirty = true
	node.children[name] = child
	return child, nil
}

// setDeltaEntry records entry at segments, replacing whatever was there.
func (s *Store) setDeltaEntry(root *deltaTree, segments []string, entry TreeEntry) error {
	node := root
	for _, name := range segments[:len(segments)-1] {
		node.dirty = true
		child, err := s.subtree(node, name, true)
		if err != nil {
			return err
		}
		node = child
	}
	node.dirty = true
	name := segments[len(segments)-1]
	delete(node.children, name)
	node.entries[name] = entry
	return nil
}

// removeDeltaEntry removes the leaf or subtree at segments; a path the tree
// does not have is a no-op.
func (s *Store) removeDeltaEntry(root *deltaTree, segments []string) error {
	ancestors := []*deltaTree{root}
	node := root
	for _, name := range segments[:len(segments)-1] {
		child, err := s.subtree(node, name, false)
		if err != nil || child == nil {
			return err
		}
		ancestors = append(ancestors, child)
		node = child
	}
	name := segments[len(segments)-1]
	_, isLeaf := node.entries[name]
	_, isDir := node.children[name]
	if !isLeaf && !isDir {
		return nil
	}
	delete(node.entries, name)
	delete(node.children, name)
	for _, ancestor := range ancestors {
		ancestor.dirty = true
	}
	return nil
}

// storeDeltaTree writes the changed trees bottom-up and reuses the hash of
// every unchanged one. A directory left empty is dropped (a full capture
// never records empty directories); the root is always written.
func (r *captureRun) storeDeltaTree(ctx context.Context, node *deltaTree, isRoot bool) (string, error) {
	if !node.dirty {
		return node.hash, nil
	}
	tree := TreeObject{Entries: make([]TreeEntry, 0, len(node.entries)+len(node.children))}
	for _, entry := range node.entries {
		tree.Entries = append(tree.Entries, entry)
	}
	for name, child := range node.children {
		hash, err := r.storeDeltaTree(ctx, child, false)
		if err != nil {
			return "", err
		}
		if hash != "" {
			tree.Entries = append(tree.Entries, TreeEntry{Name: name, Kind: EntryTree, ObjectHash: hash})
		}
	}
	if len(tree.Entries) == 0 && !isRoot {
		return "", nil
	}
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Name < tree.Entries[j].Name })
	written, err := r.putTreeObject(ctx, tree)
	if err != nil {
		return "", err
	}
	if written.New {
		r.stats.NewCASBytes += written.Size
	}
	return written.Hash, nil
}

// summarizeTree lists the leaves of rootHash in path order, as a full capture
// would have produced them, and enforces the file and byte limits.
func (s *Store) summarizeTree(ctx context.Context, plan capturePlan, rootHash string) ([]leaf, error) {
	flat, err := s.flattenTree(ctx, rootHash)
	if err != nil {
		return nil, err
	}
	if len(flat) > plan.limits.MaxFiles {
		return nil, fmt.Errorf("%w: more than %d files", ErrCaptureLimitExceeded, plan.limits.MaxFiles)
	}
	leaves := make([]leaf, 0, len(flat))
	var logicalBytes int64
	for rel, entry := range flat {
		captured := leaf{path: rel, entry: TreeEntry{Name: path.Base(rel), Kind: entry.Kind, ObjectHash: entry.Hash, Mode: entry.Mode, Size: entry.Size}}
		switch entry.Kind {
		case EntryBlob:
			logicalBytes += entry.Size
		case EntrySymlink:
			target, linkErr := s.readLink(entry.Hash)
			if linkErr != nil {
				return nil, linkErr
			}
			captured.escaping = symlinkEscapes(plan.root, rel, target)
		}
		leaves = append(leaves, captured)
	}
	if limit := plan.limits.MaxLogicalBytes; limit > 0 && logicalBytes > limit {
		return nil, fmt.Errorf("%w: more than %d logical bytes", ErrCaptureLimitExceeded, limit)
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].path < leaves[j].path })
	return leaves, nil
}
