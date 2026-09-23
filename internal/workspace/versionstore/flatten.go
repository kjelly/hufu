package versionstore

import (
	"context"
	"fmt"
	"path"
)

// flatLeaf is one managed path of a snapshot tree.
type flatLeaf struct {
	Kind EntryKind
	Hash string
	Mode uint32
	Size int64
}

// flattenTree returns every blob and symlink below rootHash keyed by
// root-relative path. It verifies each tree object while reading it and
// rejects empty non-root trees.
func (s *Store) flattenTree(ctx context.Context, rootHash string) (map[string]flatLeaf, error) {
	leaves := make(map[string]flatLeaf)
	if err := s.flattenInto(ctx, rootHash, "", leaves); err != nil {
		return nil, err
	}
	return leaves, nil
}

func (s *Store) flattenInto(ctx context.Context, hash, prefix string, leaves map[string]flatLeaf) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := s.readTree(hash)
	if err != nil {
		return err
	}
	if prefix != "" && len(tree.Entries) == 0 {
		return fmt.Errorf("%w: empty tree at %q", ErrInvalidTree, prefix)
	}
	for _, entry := range tree.Entries {
		rel := path.Join(prefix, entry.Name)
		if entry.Kind == EntryTree {
			if err = s.flattenInto(ctx, entry.ObjectHash, rel, leaves); err != nil {
				return err
			}
			continue
		}
		leaves[rel] = flatLeaf{Kind: entry.Kind, Hash: entry.ObjectHash, Mode: entry.Mode, Size: entry.Size}
	}
	return nil
}
