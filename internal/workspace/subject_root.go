package workspace

import (
	"fmt"
	"os"
	"path/filepath"
)

func DiscoverSubjectRoot(start string) (string, error) {
	canonical, err := canonicalPath(start)
	if err != nil {
		return "", fmt.Errorf("discover subject root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("discover subject root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("discover subject root: %q is not a directory", canonical)
	}

	for current := canonical; ; current = filepath.Dir(current) {
		gitInfo, statErr := os.Lstat(filepath.Join(current, ".git"))
		if statErr == nil && (gitInfo.IsDir() || gitInfo.Mode().IsRegular()) {
			return current, nil
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect git marker in %q: %w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return canonical, nil
		}
	}
}

func canonicalPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	return canonicalPathWithMissingSuffix(absolute)
}

func canonicalPathWithMissingSuffix(absolute string) (string, error) {
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			parts := append([]string{filepath.Clean(resolved)}, reverseStrings(suffix)...)
			return filepath.Join(parts...), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("canonicalize %q: %w", absolute, err)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func reverseStrings(values []string) []string {
	reversed := make([]string, len(values))
	for index := range values {
		reversed[len(values)-1-index] = values[index]
	}
	return reversed
}
