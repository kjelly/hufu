package versionstore

import (
	"context"
	"fmt"
)

// DiscoveryOptions configures DiscoverManagedPaths.
type DiscoveryOptions struct {
	// ExcludeSubtrees are root-relative subtrees that are never managed.
	ExcludeSubtrees   []string
	RequireHufuignore bool
}

// ManagedPathSet is the managed-path candidate list of a root under the
// version store's inclusion policy.
type ManagedPathSet struct {
	GitMode bool
	// Paths are root-relative, forward-slash candidates, sorted. Each must
	// still be classified with Lstat; a tracked file deleted from the working
	// tree is listed but no longer exists.
	Paths []string
	// Excluded is exactly the normalized ExcludeSubtrees.
	Excluded []string
	// Nested are the submodule and nested repository prefixes discovered
	// under the root. Their contents are never listed in Paths.
	Nested []string
}

// DiscoverManagedPaths lists root's managed paths with the same inclusion
// policy as a capture: Git's tracked and untracked-but-not-ignored files in
// a Git working tree, otherwise a walk honoring .hufuignore, and never .git
// or the excluded subtrees.
func DiscoverManagedPaths(ctx context.Context, root string, opts DiscoveryOptions) (ManagedPathSet, error) {
	subtrees, err := normalizeSubtrees(opts.ExcludeSubtrees)
	if err != nil {
		return ManagedPathSet{}, fmt.Errorf("discover managed paths: %w", err)
	}
	set, err := discoverCandidates(ctx, root, inclusionPolicy{excludeSubtrees: subtrees, requireHufuignore: opts.RequireHufuignore})
	if err != nil {
		return ManagedPathSet{}, fmt.Errorf("discover managed paths: %w", err)
	}
	excluded := make(map[string]bool, len(subtrees))
	for _, subtree := range subtrees {
		excluded[subtree] = true
	}
	var nested []string
	for _, prefix := range set.unmanaged {
		if !excluded[prefix] {
			nested = append(nested, prefix)
		}
	}
	return ManagedPathSet{GitMode: set.gitMode, Paths: set.paths, Excluded: subtrees, Nested: nested}, nil
}

// GitIgnoredPaths reports which root-relative paths Git ignores in the
// working tree at root. The paths need not exist. Tracked files are never
// reported.
func GitIgnoredPaths(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	return gitIgnored(ctx, root, paths)
}

// HufuignoreMatcher returns root's .hufuignore matcher. A root without a
// .hufuignore ignores nothing.
func HufuignoreMatcher(root string) (func(rel string, isDir bool) bool, error) {
	ignore, err := loadHufuignore(root, false)
	if err != nil {
		return nil, err
	}
	return func(rel string, isDir bool) bool { return ignore.matches(rel, isDir) }, nil
}

// MaterializeTempPrefix names the temp files a crash can leave behind while
// materializing files into a root. Discovery never lists them, so other
// writers that stage files the same way (for example applying an isolated
// attempt's changes) should use this prefix too.
const MaterializeTempPrefix = materializeTempPrefix
