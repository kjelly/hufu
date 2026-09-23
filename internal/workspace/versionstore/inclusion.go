package versionstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// candidateSet is the managed-path candidate list of one capture. Paths are
// root-relative, forward-slash, and sorted; each still has to be classified
// by Lstat. unmanaged holds directory prefixes that are never read, written,
// or deleted (control-root subtrees, Git submodules, nested repositories).
type candidateSet struct {
	gitMode   bool
	paths     []string
	unmanaged []string
}

// inclusionPolicy is versionstore's own policy (it deliberately does not reuse
// the effect snapshotter's name-based bookkeeping filter). Only three kinds
// of path are excluded: any ".git" entry, the excluded subtrees, and paths
// ignored by Git (Git mode) or .hufuignore (walk mode).
type inclusionPolicy struct {
	excludeSubtrees []string
	// requireHufuignore fails a non-Git capture that has no .hufuignore.
	requireHufuignore bool
}

// normalizeSubtrees validates root-relative exclusion prefixes.
func normalizeSubtrees(subtrees []string) ([]string, error) {
	out := make([]string, 0, len(subtrees))
	for _, subtree := range subtrees {
		clean := path.Clean(filepath.ToSlash(strings.TrimSpace(subtree)))
		if clean == "." {
			return nil, fmt.Errorf("%w: an excluded subtree cannot be the whole root", ErrVersionedWorkspaceUnresolved)
		}
		if clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
			return nil, fmt.Errorf("excluded subtree %q is not root-relative", subtree)
		}
		out = append(out, clean)
	}
	sort.Strings(out)
	return out, nil
}

// underAny reports whether rel equals or lies below one of prefixes.
func underAny(rel string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

// materializeTempPrefix names materialize's temp files; a crash can leave
// one behind, and it must never be captured as a managed file.
const materializeTempPrefix = ".hufu-materialize-"

func isMaterializeTemp(rel string) bool {
	return strings.HasPrefix(path.Base(rel), materializeTempPrefix)
}

func hasGitSegment(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

// isGitWorkTree reports whether root is inside a Git working tree and the git
// binary is available.
func isGitWorkTree(ctx context.Context, root string) bool {
	if _, err := exec.LookPath("git"); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = root
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func gitOutput(ctx context.Context, root string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	trimmed := strings.TrimSuffix(string(out), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\x00"), nil
}

// discoverCandidates lists candidate managed paths below root.
func discoverCandidates(ctx context.Context, root string, policy inclusionPolicy) (candidateSet, error) {
	if isGitWorkTree(ctx, root) {
		return discoverGitCandidates(ctx, root, policy)
	}
	return discoverWalkCandidates(ctx, root, policy)
}

// discoverGitCandidates uses Git only to decide the candidate set: tracked
// files plus untracked, non-ignored files. Submodules (gitlink entries) and
// nested repositories (untracked directories reported with a trailing '/')
// are unmanaged.
func discoverGitCandidates(ctx context.Context, root string, policy inclusionPolicy) (candidateSet, error) {
	set := candidateSet{gitMode: true, unmanaged: append([]string(nil), policy.excludeSubtrees...)}
	staged, err := gitOutput(ctx, root, "ls-files", "-z", "--stage")
	if err != nil {
		return candidateSet{}, err
	}
	untracked, err := gitOutput(ctx, root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return candidateSet{}, err
	}
	seen := make(map[string]bool, len(staged)+len(untracked))
	var paths []string
	for _, line := range staged {
		meta, rel, ok := strings.Cut(line, "\t")
		if !ok {
			return candidateSet{}, fmt.Errorf("unexpected git ls-files --stage line %q", line)
		}
		if strings.HasPrefix(meta, "160000 ") {
			set.unmanaged = append(set.unmanaged, rel)
			continue
		}
		paths = append(paths, rel)
	}
	nested := nestedRepoFinder{root: root, known: map[string]bool{}}
	for _, rel := range untracked {
		if strings.HasSuffix(rel, "/") {
			set.unmanaged = append(set.unmanaged, strings.TrimSuffix(rel, "/"))
			continue
		}
		if repo, ok := nested.enclosingRepo(rel); ok {
			set.unmanaged = append(set.unmanaged, repo)
			continue
		}
		paths = append(paths, rel)
	}
	sort.Strings(set.unmanaged)
	for _, rel := range paths {
		if rel == "" || seen[rel] || hasGitSegment(rel) || isMaterializeTemp(rel) || underAny(rel, set.unmanaged) {
			continue
		}
		seen[rel] = true
		set.paths = append(set.paths, rel)
	}
	sort.Strings(set.paths)
	return set, nil
}

// nestedRepoFinder detects untracked files that live inside a nested
// repository even when Git itself did not report the repository as a
// directory (for example an incompletely initialized .git).
type nestedRepoFinder struct {
	root  string
	known map[string]bool
}

func (f nestedRepoFinder) enclosingRepo(rel string) (string, bool) {
	for _, dir := range parentDirs(rel) {
		isRepo, cached := f.known[dir]
		if !cached {
			_, err := os.Lstat(filepath.Join(f.root, filepath.FromSlash(dir), ".git"))
			isRepo = err == nil
			f.known[dir] = isRepo
		}
		if isRepo {
			return dir, true
		}
	}
	return "", false
}

// discoverWalkCandidates walks root without following symlinks.
func discoverWalkCandidates(ctx context.Context, root string, policy inclusionPolicy) (candidateSet, error) {
	ignore, err := loadHufuignore(root, policy.requireHufuignore)
	if err != nil {
		return candidateSet{}, err
	}
	set := candidateSet{unmanaged: append([]string(nil), policy.excludeSubtrees...)}
	err = filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if full == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, full)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		isDir := entry.IsDir()
		if entry.Name() == ".git" || underAny(rel, set.unmanaged) || ignore.matches(rel, isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		if !isDir && !isMaterializeTemp(rel) {
			set.paths = append(set.paths, rel)
		}
		return nil
	})
	if err != nil {
		return candidateSet{}, fmt.Errorf("walk workspace: %w", err)
	}
	sort.Strings(set.paths)
	return set, nil
}

func loadHufuignore(root string, required bool) (*hufuIgnore, error) {
	data, err := os.ReadFile(filepath.Join(root, HufuignoreFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if required {
				return nil, ErrHufuignoreRequired
			}
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", HufuignoreFile, err)
	}
	return parseHufuignore(data)
}

// HasHufuignore reports whether root contains a .hufuignore file.
func HasHufuignore(root string) bool {
	info, err := os.Lstat(filepath.Join(root, HufuignoreFile))
	return err == nil && info.Mode().IsRegular()
}

// IsGitWorkTree reports whether root is a Git working tree (git available).
func IsGitWorkTree(ctx context.Context, root string) bool { return isGitWorkTree(ctx, root) }
