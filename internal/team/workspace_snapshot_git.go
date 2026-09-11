package team

import (
	"context"
	"os/exec"
	"strings"
)

// gitCandidateFiles uses Git to discover which paths matter in root quickly —
// tracked files plus untracked-but-not-ignored files — respecting
// .gitignore automatically instead of hand-rolled skip rules
// (docs/architecture/execution-runtime.md). The caller still
// hashes every candidate's actual bytes from disk: Git only accelerates
// candidate discovery, it is never trusted for content integrity.
//
// The second return value is false whenever root is not (detectably) a Git
// working tree or the git binary/commands are unavailable, so the caller
// falls back to a full directory walk.
func gitCandidateFiles(ctx context.Context, root string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	if err := runGitCheck(ctx, root, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, false
	}
	tracked, err := runGitFileList(ctx, root, "ls-files", "-z")
	if err != nil {
		return nil, false
	}
	untracked, err := runGitFileList(ctx, root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, false
	}
	seen := make(map[string]bool, len(tracked)+len(untracked))
	candidates := make([]string, 0, len(tracked)+len(untracked))
	for _, path := range append(tracked, untracked...) {
		if path == "" || seen[path] || isWorkspaceInternalPath(path) {
			continue
		}
		seen[path] = true
		candidates = append(candidates, path)
	}
	return candidates, true
}

func runGitCheck(ctx context.Context, root string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	return cmd.Run()
}

func runGitFileList(ctx context.Context, root string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSuffix(string(out), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\x00"), nil
}
