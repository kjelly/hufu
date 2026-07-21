package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
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
	AgentIdentity       string        `json:"agent_identity,omitempty"`
	TaskGoal            string        `json:"task_goal,omitempty"`
	Constraints         string        `json:"constraints,omitempty"`
	Verify              string        `json:"verify,omitempty"`
	VerifyMode          string        `json:"verify_mode,omitempty"`
	RepoCommit          string        `json:"repo_commit,omitempty"`
	WorkspaceGen        int64         `json:"workspace_gen,omitempty"`
	ProjectFingerprint  string        `json:"project_fingerprint,omitempty"`
	ToolRegistryVersion string        `json:"tool_registry_version,omitempty"`
	SkillHashes         string        `json:"skill_hashes,omitempty"`
	PolicyVersion       string        `json:"policy_version,omitempty"`
	ModelFamily         string        `json:"model_family,omitempty"`
	DependencyHashes    string        `json:"dependency_hashes,omitempty"`
	CreatedAt           time.Time     `json:"created_at,omitempty"`
	TTL                 time.Duration `json:"ttl,omitempty"`
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

// ComputeProjectFingerprint generates a SHA256 digest of top-level project files and git status (if available).
func ComputeProjectFingerprint(dir string) string {
	if dir == "" {
		return ""
	}
	hasher := sha256.New()

	// 1. Try git status --porcelain first if git is available
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = dir
	var gitOut bytes.Buffer
	cmd.Stdout = &gitOut
	if err := cmd.Run(); err == nil {
		hasher.Write(gitOut.Bytes())
		commit := ComputeRepoCommit(dir)
		hasher.Write([]byte(commit))
		return hex.EncodeToString(hasher.Sum(nil))
	}

	// 2. Fallback: scan top-level directory entries and mtimes
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(hasher, "%s:%d:%d;", entry.Name(), info.Size(), info.ModTime().UnixNano()) //nolint:errcheck
	}
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
	c.cachePolicyMu.RLock()
	defer c.cachePolicyMu.RUnlock()
	if c.cachePolicy == "" {
		return CacheUse
	}
	return c.cachePolicy
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

	return CacheIdentity{
		AgentIdentity:      agentKey,
		TaskGoal:           taskDesc,
		Verify:             verify,
		VerifyMode:         normalizeVerifyMode(verifyMode),
		RepoCommit:         ComputeRepoCommit(projDir),
		WorkspaceGen:       gen,
		ProjectFingerprint: ComputeProjectFingerprint(projDir),
		CreatedAt:          time.Now(),
	}
}

// isFresh checks if a cached entry matches the current identity's workspace, source, and TTL requirements.
func (e cachedTaskEntry) isFresh(target CacheIdentity) bool {
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
