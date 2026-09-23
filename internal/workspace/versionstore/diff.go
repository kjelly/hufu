package versionstore

import (
	"context"
	"path"
	"sort"
)

// PathChange is one path-level difference between two snapshots.
type PathChange struct {
	Path     string
	FromKind EntryKind
	ToKind   EntryKind
	FromHash string
	ToHash   string
	FromMode uint32
	ToMode   uint32
}

// WorkspaceVersionDelta lists changes from one snapshot to another. A path
// whose kind changes (file↔symlink or leaf↔directory) is in TypeChanged; the
// leaves inside a directory that appears or disappears are also listed in
// Added or Deleted.
type WorkspaceVersionDelta struct {
	Added       []PathChange
	Modified    []PathChange
	Deleted     []PathChange
	TypeChanged []PathChange
}

// Empty reports whether the delta has no changes.
func (d WorkspaceVersionDelta) Empty() bool {
	return len(d.Added)+len(d.Modified)+len(d.Deleted)+len(d.TypeChanged) == 0
}

// Diff compares two snapshots. Subtrees with equal hashes are skipped
// without being read, so a local change in a large workspace only reads the
// trees along the changed paths.
func (s *Store) Diff(ctx context.Context, from, to SnapshotID) (WorkspaceVersionDelta, error) {
	fromHash, err := s.diffableRoot(ctx, from)
	if err != nil {
		return WorkspaceVersionDelta{}, err
	}
	toHash, err := s.diffableRoot(ctx, to)
	if err != nil {
		return WorkspaceVersionDelta{}, err
	}
	return s.DiffTrees(ctx, fromHash, toHash)
}

func (s *Store) diffableRoot(ctx context.Context, id SnapshotID) (string, error) {
	snapshot, err := s.GetSnapshot(ctx, id)
	if err != nil {
		return "", err
	}
	if snapshot.State == StatePruned || snapshot.State == StateOrphaned {
		return "", ErrWorkspaceSnapshotUnavailable
	}
	return snapshot.RootTreeHash, nil
}

// DiffTrees compares two root trees by hash.
func (s *Store) DiffTrees(ctx context.Context, fromHash, toHash string) (WorkspaceVersionDelta, error) {
	var delta WorkspaceVersionDelta
	if err := s.diffNodes(ctx, "", fromHash, toHash, &delta); err != nil {
		return WorkspaceVersionDelta{}, err
	}
	for _, list := range []*[]PathChange{&delta.Added, &delta.Modified, &delta.Deleted, &delta.TypeChanged} {
		sort.Slice(*list, func(i, j int) bool { return (*list)[i].Path < (*list)[j].Path })
	}
	return delta, nil
}

func (s *Store) diffNodes(ctx context.Context, prefix, fromHash, toHash string, delta *WorkspaceVersionDelta) error {
	if fromHash == toHash {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fromTree, err := s.readTreeCounted(fromHash)
	if err != nil {
		return err
	}
	toTree, err := s.readTreeCounted(toHash)
	if err != nil {
		return err
	}
	fromEntries := make(map[string]TreeEntry, len(fromTree.Entries))
	for _, entry := range fromTree.Entries {
		fromEntries[entry.Name] = entry
	}
	for _, toEntry := range toTree.Entries {
		rel := path.Join(prefix, toEntry.Name)
		fromEntry, existed := fromEntries[toEntry.Name]
		delete(fromEntries, toEntry.Name)
		if !existed {
			if err = s.listSide(ctx, rel, toEntry, &delta.Added, true); err != nil {
				return err
			}
			continue
		}
		if err = s.diffEntry(ctx, rel, fromEntry, toEntry, delta); err != nil {
			return err
		}
	}
	for _, fromEntry := range fromEntries {
		if err = s.listSide(ctx, path.Join(prefix, fromEntry.Name), fromEntry, &delta.Deleted, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) diffEntry(ctx context.Context, rel string, from, to TreeEntry, delta *WorkspaceVersionDelta) error {
	switch {
	case from.Kind == EntryTree && to.Kind == EntryTree:
		return s.diffNodes(ctx, rel, from.ObjectHash, to.ObjectHash, delta)
	case from.Kind == to.Kind:
		if from.ObjectHash != to.ObjectHash || from.Mode != to.Mode {
			delta.Modified = append(delta.Modified, change(rel, from, to))
		}
		return nil
	default:
		delta.TypeChanged = append(delta.TypeChanged, change(rel, from, to))
		if from.Kind == EntryTree {
			return s.listSide(ctx, rel, from, &delta.Deleted, false)
		}
		if to.Kind == EntryTree {
			return s.listSide(ctx, rel, to, &delta.Added, true)
		}
		return nil
	}
}

func change(rel string, from, to TreeEntry) PathChange {
	return PathChange{Path: rel, FromKind: from.Kind, ToKind: to.Kind, FromHash: from.ObjectHash,
		ToHash: to.ObjectHash, FromMode: from.Mode, ToMode: to.Mode}
}

// listSide records every leaf of entry (a leaf or a whole subtree) that
// exists on only one side.
func (s *Store) listSide(ctx context.Context, rel string, entry TreeEntry, out *[]PathChange, added bool) error {
	record := func(leafPath string, leaf TreeEntry) {
		pc := PathChange{Path: leafPath}
		if added {
			pc.ToKind, pc.ToHash, pc.ToMode = leaf.Kind, leaf.ObjectHash, leaf.Mode
		} else {
			pc.FromKind, pc.FromHash, pc.FromMode = leaf.Kind, leaf.ObjectHash, leaf.Mode
		}
		*out = append(*out, pc)
	}
	if entry.Kind != EntryTree {
		record(rel, entry)
		return nil
	}
	leaves := make(map[string]flatLeaf)
	if err := s.flattenInto(ctx, entry.ObjectHash, rel, leaves); err != nil {
		return err
	}
	for leafPath, leaf := range leaves {
		record(leafPath, TreeEntry{Kind: leaf.Kind, ObjectHash: leaf.Hash, Mode: leaf.Mode})
	}
	return nil
}

// readTreeCounted is readTree plus an optional test counter.
func (s *Store) readTreeCounted(hash string) (TreeObject, error) {
	if s.treeReads != nil {
		s.treeReads.Add(1)
	}
	return s.readTree(hash)
}
