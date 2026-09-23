package versionstore

import (
	"context"
	"fmt"
)

// Verify re-reads every object reachable from a snapshot and checks that each
// file's content matches its name (I4), each tree decodes canonically, and
// every entry's recorded size matches its object.
func (s *Store) Verify(ctx context.Context, id SnapshotID) error {
	snapshot, err := s.GetSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if snapshot.State == StatePruned || snapshot.State == StateOrphaned {
		return fmt.Errorf("%w: snapshot %s is %s", ErrWorkspaceSnapshotUnavailable, id, snapshot.State)
	}
	return s.verifyTree(ctx, snapshot.RootTreeHash)
}

func (s *Store) verifyTree(ctx context.Context, rootHash string) error {
	leaves, err := s.flattenTree(ctx, rootHash)
	if err != nil {
		return err
	}
	for rel, entry := range leaves {
		if err = ctx.Err(); err != nil {
			return err
		}
		switch entry.Kind {
		case EntryBlob:
			size, hashErr := s.hashObjectFile(ObjectBlob, entry.Hash)
			if hashErr != nil {
				return fmt.Errorf("verify %q: %w", rel, hashErr)
			}
			if size != entry.Size {
				return fmt.Errorf("%w: %q blob has %d bytes, tree records %d", ErrCASCorrupt, rel, size, entry.Size)
			}
		case EntrySymlink:
			target, linkErr := s.readLink(entry.Hash)
			if linkErr != nil {
				return fmt.Errorf("verify %q: %w", rel, linkErr)
			}
			if int64(len(target)) != entry.Size {
				return fmt.Errorf("%w: %q link target length mismatch", ErrCASCorrupt, rel)
			}
		}
	}
	return nil
}
