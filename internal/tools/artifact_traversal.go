package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveSearchRoot applies the merged policy before either an explicit or
// default search root is handed to a recursive tool. A default root retains
// the historical working-directory behavior; its descendants are filtered by
// the traversal helpers below.
func resolveSearchRoot(path, workDir, operation string, cfg ToolConfig) (string, error) {
	if path != "" {
		return checkPathOrConsent(path, workDir, operation, cfg)
	}
	root := workDir
	if root == "" {
		root = "."
	}
	if err := enforceArtifactPathPolicy(canonicalPathForAuthorization(root), cfg); err != nil {
		return "", err
	}
	return root, nil
}

// artifactTraversalCandidate returns the canonical path only when a
// recursively discovered entry remains under the authorized root and outside
// every blocked artifact root. Symlink entries are canonicalized before they
// are returned, so backends never receive an alias that can be retargeted
// across the artifact boundary as part of normal traversal.
func artifactTraversalCandidate(path, root string, policy *ArtifactPathPolicy) (string, bool) {
	if policy == nil {
		return path, true
	}
	canonicalPath := canonicalPathForAuthorization(path)
	canonicalRoot := canonicalPathForAuthorization(root)
	if isArtifactPathBlocked(canonicalPath, policy) {
		return "", false
	}
	if canonicalPath != canonicalRoot && !strings.HasPrefix(canonicalPath, canonicalRoot+string(filepath.Separator)) {
		return "", false
	}
	return canonicalPath, true
}

func collectAuthorizedFileCandidates(ctx context.Context, root string, policy *ArtifactPathPolicy) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		candidate, ok := artifactTraversalCandidate(root, root, policy)
		if !ok {
			return []string{}, nil
		}
		return []string{candidate}, nil
	}

	candidates := make([]string, 0)
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		canonical, ok := artifactTraversalCandidate(path, root, policy)
		if !ok {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		// filepath.Walk reports symlinks as non-directories. Only retain a
		// symlink when its target is a regular file and was canonicalized
		// inside the authorized root above.
		if targetInfo, statErr := os.Stat(path); statErr != nil || !targetInfo.Mode().IsRegular() {
			return nil
		}
		candidates = append(candidates, canonical)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk authorized search candidates: %w", err)
	}
	return uniqueStrings(candidates), nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}
