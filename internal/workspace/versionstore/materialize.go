package versionstore

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// MaterializeRequest projects one snapshot onto a filesystem root.
type MaterializeRequest struct {
	Root       string
	SnapshotID SnapshotID
	// CurrentSnapshotID is the snapshot the root's managed paths currently
	// equal (the caller captured any drift first). Empty means the root has
	// no managed paths yet. Only paths managed by the current snapshot are
	// ever replaced or deleted; every other existing path is preserved, and
	// one that the target needs is a collision (I7).
	CurrentSnapshotID SnapshotID
	// ExcludeSubtrees are never read, written, or deleted (§9.5).
	ExcludeSubtrees []string
}

// MaterializeResult summarizes a completed materialization.
type MaterializeResult struct {
	FilesWritten int
	FilesDeleted int
}

type materializePlan struct {
	target      map[string]flatLeaf
	current     map[string]flatLeaf
	writes      []string
	deletes     []string
	verifiedNow map[string]bool
}

// Materialize makes the managed paths of req.Root equal the target snapshot.
// It is re-entrant: running it again after a partial failure converges on the
// same result. Writes are temp-file + rename inside the root (never following
// symlinks), deletions happen only after every write succeeded, and the
// result is verified before success is reported.
func (s *Store) Materialize(ctx context.Context, req MaterializeRequest) (MaterializeResult, error) {
	if err := s.requireWritable(); err != nil {
		return MaterializeResult{}, err
	}
	target, err := s.materializableSnapshot(ctx, req.SnapshotID)
	if err != nil {
		return MaterializeResult{}, err
	}
	rootPath, err := CanonicalRoot(req.Root)
	if err != nil {
		return MaterializeResult{}, fmt.Errorf("materialize: %w", err)
	}
	subtrees, err := normalizeSubtrees(req.ExcludeSubtrees)
	if err != nil {
		return MaterializeResult{}, fmt.Errorf("materialize: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	plan := materializePlan{verifiedNow: map[string]bool{}}
	if plan.target, err = s.flattenTree(ctx, target.RootTreeHash); err != nil {
		return MaterializeResult{}, fmt.Errorf("materialize: read target: %w", err)
	}
	plan.current = map[string]flatLeaf{}
	if req.CurrentSnapshotID != "" {
		current, currentErr := s.GetSnapshot(ctx, req.CurrentSnapshotID)
		if currentErr != nil {
			return MaterializeResult{}, fmt.Errorf("materialize: %w", currentErr)
		}
		if plan.current, err = s.flattenTree(ctx, current.RootTreeHash); err != nil {
			return MaterializeResult{}, fmt.Errorf("materialize: read current: %w", err)
		}
	}
	if err = s.checkTargetObjects(ctx, rootPath, plan.target, subtrees); err != nil {
		return MaterializeResult{}, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return MaterializeResult{}, fmt.Errorf("materialize: open root: %w", err)
	}
	defer func() { _ = root.Close() }()
	m := &materializer{store: s, root: root, plan: &plan}
	if err = m.planChanges(ctx); err != nil {
		return MaterializeResult{}, err
	}
	if err = m.apply(ctx); err != nil {
		return MaterializeResult{}, err
	}
	if err = m.verify(ctx); err != nil {
		return MaterializeResult{}, err
	}
	return MaterializeResult{FilesWritten: len(plan.writes), FilesDeleted: m.deleted}, nil
}

func (s *Store) materializableSnapshot(ctx context.Context, id SnapshotID) (Snapshot, error) {
	snapshot, err := s.GetSnapshot(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	switch snapshot.State {
	case StatePending, StatePublished:
	default:
		return Snapshot{}, fmt.Errorf("%w: snapshot %s is %s", ErrWorkspaceSnapshotUnavailable, id, snapshot.State)
	}
	if !snapshot.Materializable {
		return Snapshot{}, fmt.Errorf("%w: %s contains a symlink that escapes the workspace", ErrSnapshotNotMaterializable, id)
	}
	return snapshot, nil
}

// checkTargetObjects verifies, before the filesystem is touched, that every
// object the target needs exists, that no target path lies in an excluded
// subtree, and that every symlink stays inside the root.
func (s *Store) checkTargetObjects(ctx context.Context, root string, target map[string]flatLeaf, subtrees []string) error {
	for rel, entry := range target {
		if err := ctx.Err(); err != nil {
			return err
		}
		if underAny(rel, subtrees) {
			return fmt.Errorf("%w: snapshot path %q lies in an excluded subtree", ErrMaterializationCollision, rel)
		}
		switch entry.Kind {
		case EntryBlob:
			ok, err := s.hasObject(ObjectBlob, entry.Hash)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: blob for %q", ErrCASObjectMissing, rel)
			}
		case EntrySymlink:
			linkTarget, err := s.readLink(entry.Hash)
			if err != nil {
				return fmt.Errorf("materialize %q: %w", rel, err)
			}
			if symlinkEscapesLexically(root, rel, linkTarget) {
				return fmt.Errorf("%w: %q -> %q escapes the workspace", ErrSnapshotNotMaterializable, rel, linkTarget)
			}
		}
	}
	return nil
}

// symlinkEscapesLexically judges a link target without touching the
// filesystem (the link and its target may not exist yet).
func symlinkEscapesLexically(root, rel, target string) bool {
	resolved := target
	if !filepath.IsAbs(target) {
		resolved = filepath.Join(root, filepath.Dir(filepath.FromSlash(rel)), target)
	}
	resolved = filepath.Clean(resolved)
	return resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator))
}

type materializer struct {
	store   *Store
	root    *os.Root
	plan    *materializePlan
	deleted int
	// removedParents collects directories that may have become empty.
	removedParents map[string]bool
}

// planChanges decides which target paths need writing and which current
// paths need deleting.
func (m *materializer) planChanges(ctx context.Context) error {
	targetPaths := sortedKeys(m.plan.target)
	for _, rel := range targetPaths {
		want := m.plan.target[rel]
		if have, ok := m.plan.current[rel]; ok && have == want {
			continue
		}
		matches, err := m.diskMatches(ctx, rel, want)
		if err != nil {
			return err
		}
		if matches {
			m.plan.verifiedNow[rel] = true
			continue
		}
		m.plan.writes = append(m.plan.writes, rel)
	}
	for _, rel := range sortedKeys(m.plan.current) {
		if _, kept := m.plan.target[rel]; !kept {
			m.plan.deletes = append(m.plan.deletes, rel)
		}
	}
	return nil
}

// apply runs the write phase and then the delete phase.
func (m *materializer) apply(ctx context.Context) error {
	m.removedParents = map[string]bool{}
	pendingDeletes := make(map[string]bool, len(m.plan.deletes))
	for _, rel := range m.plan.deletes {
		pendingDeletes[rel] = true
	}
	for _, rel := range m.plan.writes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.clearPathFor(rel, pendingDeletes); err != nil {
			return err
		}
		if err := m.writeLeaf(ctx, rel, m.plan.target[rel]); err != nil {
			return err
		}
	}
	for _, rel := range m.plan.deletes {
		if !pendingDeletes[rel] {
			continue
		}
		if err := m.deleteManaged(rel); err != nil {
			return err
		}
		delete(pendingDeletes, rel)
	}
	return m.removeEmptiedDirs()
}

// clearPathFor makes room for a target leaf: managed paths that block it
// (a managed file where a directory is needed, or managed files below a path
// that becomes a file) are deleted early; anything unmanaged is a collision.
func (m *materializer) clearPathFor(rel string, pendingDeletes map[string]bool) error {
	segments := strings.Split(rel, "/")
	for index := 1; index < len(segments); index++ {
		ancestor := strings.Join(segments[:index], "/")
		if pendingDeletes[ancestor] {
			if err := m.deleteManaged(ancestor); err != nil {
				return err
			}
			delete(pendingDeletes, ancestor)
		}
	}
	info, err := m.root.Lstat(filepath.FromSlash(rel))
	if err != nil || !info.IsDir() {
		return nil
	}
	for _, pending := range sortedBoolKeys(pendingDeletes) {
		if strings.HasPrefix(pending, rel+"/") {
			if err = m.deleteManaged(pending); err != nil {
				return err
			}
			delete(pendingDeletes, pending)
		}
	}
	return m.removeEmptiedDirs()
}

func sortedKeys(values map[string]flatLeaf) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// parentDirs lists the ancestors of rel, deepest first.
func parentDirs(rel string) []string {
	var dirs []string
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		dirs = append(dirs, dir)
	}
	return dirs
}
