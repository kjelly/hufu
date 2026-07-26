package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CachePolicy defines how task result caching is handled during execution.
type CachePolicy string

const (
	// CacheUse is the normal mode: look up past results and store new ones.
	CacheUse CachePolicy = "use"
	// CacheRefresh forces re-execution of tasks while updating the cache with fresh results.
	CacheRefresh CachePolicy = "refresh"
	// CacheBypass completely disables reading and writing to the cache.
	CacheBypass CachePolicy = "bypass"
)

// CacheIdentity captures full contextual identity for workspace/source/environment freshness.
type CacheIdentity struct {
	AgentIdentity        string        `json:"agent_identity,omitempty"`
	TaskGoal             string        `json:"task_goal,omitempty"`
	Constraints          string        `json:"constraints,omitempty"`
	Verify               string        `json:"verify,omitempty"`
	VerifyMode           string        `json:"verify_mode,omitempty"`
	RepoCommit           string        `json:"repo_commit,omitempty"`
	WorkspaceGen         int64         `json:"workspace_gen,omitempty"`
	WorkspacePath        string        `json:"workspace_path,omitempty"`
	ProjectFingerprint   string        `json:"project_fingerprint,omitempty"`
	HasError             bool          `json:"has_error,omitempty"`
	ToolRegistryVersion  string        `json:"tool_registry_version,omitempty"`
	SkillHashes          string        `json:"skill_hashes,omitempty"`
	PolicyVersion        string        `json:"policy_version,omitempty"`
	ModelFamily          string        `json:"model_family,omitempty"`
	DependencyHashes     string        `json:"dependency_hashes,omitempty"`
	ExecutionProfileName string        `json:"execution_profile_name,omitempty"`
	DisableMemory        bool          `json:"disable_memory,omitempty"`
	CreatedAt            time.Time     `json:"created_at,omitempty"`
	TTL                  time.Duration `json:"ttl,omitempty"`
}

// ComputeRepoCommit attempts to determine the Git HEAD commit SHA for the directory.
// Returns an empty string if git is unavailable or the directory is not a git repository.
func ComputeRepoCommit(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	gitPath := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitPath); err == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err == nil && strings.TrimSpace(out.String()) == "true" {
		return true
	}
	return false
}

// ComputeProjectFingerprintWithStatus generates a SHA256 digest of top-level project files,
// git diffs (staged and unstaged binary diffs), and untracked file contents (if in git).
// Returns (fingerprint, hasError).
func ComputeProjectFingerprintWithStatus(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	hasher := sha256.New()
	hasError := false

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if isGitRepo(dir) {
		commitCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		commitCmd.Dir = dir
		var commitOut bytes.Buffer
		commitCmd.Stdout = &commitOut
		if err := commitCmd.Run(); err != nil {
			hasError = true
			_, _ = fmt.Fprintf(hasher, "CMD_ERROR:rev-parse:%v\n", err)
		} else {
			hasher.Write([]byte("COMMIT:"))
			hasher.Write(bytes.TrimSpace(commitOut.Bytes()))
			hasher.Write([]byte("\n"))
		}

		// 1. Unstaged binary diff
		diffCmd := exec.CommandContext(ctx, "git", "diff", "--binary")
		diffCmd.Dir = dir
		var diffOut bytes.Buffer
		diffCmd.Stdout = &diffOut
		if err := diffCmd.Run(); err != nil {
			hasError = true
			_, _ = fmt.Fprintf(hasher, "CMD_ERROR:diff:%v\n", err)
		} else {
			hasher.Write([]byte("UNSTAGED_DIFF:"))
			hasher.Write(diffOut.Bytes())
			hasher.Write([]byte("\n"))
		}

		// 2. Staged binary diff
		cachedDiffCmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--binary")
		cachedDiffCmd.Dir = dir
		var cachedDiffOut bytes.Buffer
		cachedDiffCmd.Stdout = &cachedDiffOut
		if err := cachedDiffCmd.Run(); err != nil {
			hasError = true
			_, _ = fmt.Fprintf(hasher, "CMD_ERROR:diff_cached:%v\n", err)
		} else {
			hasher.Write([]byte("STAGED_DIFF:"))
			hasher.Write(cachedDiffOut.Bytes())
			hasher.Write([]byte("\n"))
		}

		// 3. Untracked status and file contents using NUL-terminated (-z) format
		statusCmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z", "-uall")
		statusCmd.Dir = dir
		var statusOut bytes.Buffer
		statusCmd.Stdout = &statusOut
		if err := statusCmd.Run(); err != nil {
			hasError = true
			_, _ = fmt.Fprintf(hasher, "CMD_ERROR:status:%v\n", err)
		} else {
			hasher.Write([]byte("STATUS:"))
			hasher.Write(statusOut.Bytes())
			hasher.Write([]byte("\n"))

			tokens := bytes.Split(statusOut.Bytes(), []byte{0})
			for _, token := range tokens {
				if len(token) >= 3 && bytes.HasPrefix(token, []byte("?? ")) {
					pathStr := string(token[3:])
					fullPath := filepath.Join(dir, pathStr)
					hashUntrackedPath(hasher, dir, fullPath, pathStr)
				}
			}
		}

		return hex.EncodeToString(hasher.Sum(nil)), hasError
	}

	// 4. Fallback for non-git directories: scan entries and mtimes
	return computeFallbackDirFingerprint(dir), false
}

// ComputeProjectFingerprint generates a SHA256 digest of top-level project files.
func ComputeProjectFingerprint(dir string) string {
	fp, _ := ComputeProjectFingerprintWithStatus(dir)
	return fp
}

func hashUntrackedPath(hasher hash.Hash, baseDir, fullPath, relPath string) {
	info, err := os.Stat(fullPath)
	if err != nil {
		return
	}
	if info.IsDir() {
		_ = filepath.Walk(fullPath, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(baseDir, p)
			content, rErr := os.ReadFile(p)
			if rErr == nil {
				_, _ = fmt.Fprintf(hasher, "UNTRACKED:%s:%d;", rel, len(content))
				_ = ""
				hasher.Write(content)
			}
			return nil
		})
	} else {
		content, err := os.ReadFile(fullPath)
		if err == nil {
			_, _ = fmt.Fprintf(hasher, "UNTRACKED:%s:%d;", relPath, len(content))
			hasher.Write(content)
		}
	}
}

func computeFallbackDirFingerprint(dir string) string {
	hasher := sha256.New()
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		_, _ = fmt.Fprintf(hasher, "%s:%d:%d;", rel, fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(hasher.Sum(nil))
}

// SetCachePolicy sets the cache operational policy for the coordinator.
func (c *Coordinator) SetCachePolicy(p CachePolicy) {
	if c == nil {
		return
	}
	c.cachePolicyMu.Lock()
	defer c.cachePolicyMu.Unlock()
	c.cachePolicy = p
}

// GetCachePolicy returns the active cache operational policy for the coordinator (defaulting to CacheUse).
func (c *Coordinator) GetCachePolicy() CachePolicy {
	if c == nil {
		return CacheUse
	}
	prof := c.ExecutionProfile()
	if prof.DisableTaskCache {
		return CacheBypass
	}
	c.cachePolicyMu.RLock()
	defer c.cachePolicyMu.RUnlock()
	if c.cachePolicy != "" {
		return c.cachePolicy
	}
	if prof.DefaultCachePolicy != "" {
		return prof.DefaultCachePolicy
	}
	return CacheUse
}

// ComputeCacheIdentity constructs a CacheIdentity object reflecting current coordinator state.
func (c *Coordinator) ComputeCacheIdentity(agentKey, taskDesc, verify, verifyMode string) CacheIdentity {
	projDir := ""
	if c != nil {
		projDir = c.projectDir
		if projDir == "" && c.session != nil {
			projDir = c.session.Workspace
		}
	}

	var gen int64
	if c != nil {
		gen = c.cacheGeneration.Load()
	}

	fp, hasErr := ComputeProjectFingerprintWithStatus(projDir)

	return CacheIdentity{
		AgentIdentity:      agentKey,
		TaskGoal:           taskDesc,
		Verify:             verify,
		VerifyMode:         normalizeVerifyMode(verifyMode),
		RepoCommit:         ComputeRepoCommit(projDir),
		WorkspaceGen:       gen,
		ProjectFingerprint: fp,
		HasError:           hasErr,
		CreatedAt:          time.Now(),
	}
}

// isFresh checks if a cached entry matches the current identity's workspace, source, and TTL requirements.
func (e cachedTaskEntry) isFresh(target CacheIdentity) bool {
	// 0. Subprocess error state makes entry explicitly cache-ineligible
	if e.identity.HasError || target.HasError {
		return false
	}

	// 1. Check TTL expiration
	if e.identity.TTL > 0 && !e.identity.CreatedAt.IsZero() {
		if time.Since(e.identity.CreatedAt) > e.identity.TTL {
			return false
		}
	}

	// 2. Check repo commit if both are non-empty
	if e.identity.RepoCommit != "" && target.RepoCommit != "" {
		if e.identity.RepoCommit != target.RepoCommit {
			return false
		}
	}

	// 3. Check project source fingerprint if both are non-empty
	if e.identity.ProjectFingerprint != "" && target.ProjectFingerprint != "" {
		if e.identity.ProjectFingerprint != target.ProjectFingerprint {
			return false
		}
	}

	return true
}

// IsCacheForbidden checks if a task or verify prompt explicitly disables caching or is non-idempotent.
func (c *Coordinator) IsCacheForbidden(taskGoal, verify string) bool {
	combined := strings.ToLower(taskGoal + " " + verify)
	forbiddenKeywords := []string{
		"[bypass-cache]",
		"[no-cache]",
		"[rerun]",
		"[force-refresh]",
		"vm_create",
		"deploy_prod",
		"rotate_credentials",
		"security_audit",
		"benchmark_run",
	}
	for _, kw := range forbiddenKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}
