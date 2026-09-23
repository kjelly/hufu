package versionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GCRequest configures one mark-and-sweep (§28).
type GCRequest struct {
	// KeepRecent is the number of published snapshots kept per branch in
	// addition to the head.
	KeepRecent int
	// Grace protects objects, temp files, and pending snapshots younger than
	// this from being collected (an in-flight capture writes objects before
	// its row exists).
	Grace time.Duration
	// Pinned are extra roots supplied by the caller (for example snapshots
	// referenced by session labels).
	Pinned []SnapshotID
	// Apply performs the sweep; the default is a dry run.
	Apply bool
}

// GCResult reports what a GC pass found (and, with Apply, removed).
type GCResult struct {
	SnapshotsPrunable []SnapshotID
	ObjectsPrunable   int
	BytesReclaimable  int64
	TempFilesPrunable int
	Applied           bool
}

type objectKey struct {
	kind ObjectKind
	hash string
}

// GC marks every object reachable from the roots — all branch heads of every
// workspace sharing this store, snapshots referenced by incomplete
// operations or the materialized projection, pinned snapshots, each
// branch's KeepRecent most recent published snapshots, and pending snapshots
// younger than Grace — by walking trees only (D4: never the parent chain).
// Unrooted published snapshots become pruned tombstones; unreachable
// objects and temp files older than Grace are swept. Callers hold the
// project lock when Apply is set.
func (s *Store) GC(ctx context.Context, req GCRequest) (GCResult, error) {
	if req.Apply {
		if err := s.requireWritable(); err != nil {
			return GCResult{}, err
		}
	}
	now := s.now()
	roots, prunable, err := s.gcRoots(ctx, req, now)
	if err != nil {
		return GCResult{}, err
	}
	reachable := make(map[objectKey]bool)
	for id := range roots {
		snapshot, getErr := s.GetSnapshot(ctx, id)
		if getErr != nil {
			return GCResult{}, getErr
		}
		if err = s.markTree(ctx, snapshot.RootTreeHash, reachable); err != nil {
			return GCResult{}, fmt.Errorf("mark snapshot %s: %w", id, err)
		}
	}
	result := GCResult{SnapshotsPrunable: prunable, Applied: req.Apply}
	cutoff := now.Add(-req.Grace)
	var sweep []string
	err = filepath.WalkDir(s.layout.objectsDir(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		key, ok := s.objectKeyFromPath(path)
		if !ok || reachable[key] {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.ModTime().After(cutoff) {
			return infoErr
		}
		result.ObjectsPrunable++
		result.BytesReclaimable += info.Size()
		sweep = append(sweep, path)
		return nil
	})
	if err != nil {
		return GCResult{}, fmt.Errorf("scan objects: %w", err)
	}
	tmpFiles, err := s.staleTempFiles(cutoff)
	if err != nil {
		return GCResult{}, err
	}
	result.TempFilesPrunable = len(tmpFiles)
	if !req.Apply {
		return result, nil
	}
	if err = s.pruneSnapshots(ctx, prunable); err != nil {
		return result, err
	}
	for _, path := range append(sweep, tmpFiles...) {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return result, fmt.Errorf("remove %s: %w", path, removeErr)
		}
	}
	return result, nil
}

// gcRoots returns the rooted snapshots and the published snapshots to prune.
func (s *Store) gcRoots(ctx context.Context, req GCRequest, now time.Time) (map[SnapshotID]bool, []SnapshotID, error) {
	roots := make(map[SnapshotID]bool)
	heads, err := s.ListBranchHeads(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, head := range heads {
		roots[head.SnapshotID] = true
	}
	for _, id := range req.Pinned {
		roots[id] = true
	}
	extra, err := s.referencedSnapshots(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range extra {
		roots[id] = true
	}
	snapshots, err := s.ListSnapshots(ctx, SnapshotFilter{})
	if err != nil {
		return nil, nil, err
	}
	kept := make(map[string]int)
	for index := len(snapshots) - 1; index >= 0; index-- {
		snapshot := snapshots[index]
		switch snapshot.State {
		case StatePublished:
			key := snapshot.WorkspaceID + "\x00" + snapshot.BranchID
			if kept[key] < req.KeepRecent {
				kept[key]++
				roots[snapshot.ID] = true
			}
		case StatePending:
			if snapshot.CreatedAt.After(now.Add(-req.Grace)) {
				roots[snapshot.ID] = true
			}
		}
	}
	var prunable []SnapshotID
	for _, snapshot := range snapshots {
		if snapshot.State == StatePublished && !roots[snapshot.ID] {
			prunable = append(prunable, snapshot.ID)
		}
	}
	return roots, prunable, nil
}

// referencedSnapshots lists snapshots named by incomplete operations and by
// any subject's materialized projection.
func (s *Store) referencedSnapshots(ctx context.Context) ([]SnapshotID, error) {
	var ids []SnapshotID
	ops, err := s.IncompleteOperations(ctx)
	if err != nil {
		return nil, err
	}
	for _, op := range ops {
		for _, id := range []SnapshotID{op.FromSnapshotID, op.ToSnapshotID} {
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT materialized_snapshot_id FROM subject_state WHERE materialized_snapshot_id IS NOT NULL")
	if err != nil {
		return nil, fmt.Errorf("read materialized snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id sql.NullString
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, SnapshotID(id.String))
	}
	return ids, rows.Err()
}

// markTree adds every object reachable from a root tree. An already missing
// object is not an error here (doctor reports it); marking stops below it.
func (s *Store) markTree(ctx context.Context, hash string, reachable map[objectKey]bool) error {
	key := objectKey{kind: ObjectTree, hash: hash}
	if reachable[key] {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reachable[key] = true
	tree, err := s.readTree(hash)
	if errors.Is(err, ErrCASObjectMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range tree.Entries {
		if entry.Kind == EntryTree {
			if err = s.markTree(ctx, entry.ObjectHash, reachable); err != nil {
				return err
			}
			continue
		}
		if kind, ok := entry.Kind.objectKind(); ok {
			reachable[objectKey{kind: kind, hash: entry.ObjectHash}] = true
		}
	}
	return nil
}

func (s *Store) objectKeyFromPath(path string) (objectKey, bool) {
	rel, err := filepath.Rel(s.layout.objectsDir(), path)
	if err != nil {
		return objectKey{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 || parts[1] != hashAlgorithm {
		return objectKey{}, false
	}
	key := objectKey{kind: ObjectKind(parts[0]), hash: parts[2] + parts[3]}
	return key, key.kind.valid() && validHash(key.hash)
}

func (s *Store) staleTempFiles(cutoff time.Time) ([]string, error) {
	entries, err := os.ReadDir(s.layout.tmpDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan temp files: %w", err)
	}
	var stale []string
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr == nil && info.ModTime().Before(cutoff) {
			stale = append(stale, filepath.Join(s.layout.tmpDir(), entry.Name()))
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func (s *Store) pruneSnapshots(ctx context.Context, ids []SnapshotID) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET state='pruned', pruned_at=? WHERE id=? AND state='published'
AND id NOT IN (SELECT snapshot_id FROM branch_heads)`, s.nowMillis(), string(id)); err != nil {
				return fmt.Errorf("prune snapshot %s: %w", id, err)
			}
		}
		return nil
	})
}

// StoreUsage counts CAS objects and bytes for status output.
func (s *Store) StoreUsage() (objects int, bytes int64, err error) {
	err = filepath.WalkDir(s.layout.objectsDir(), func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		objects++
		bytes += info.Size()
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return objects, bytes, err
}
