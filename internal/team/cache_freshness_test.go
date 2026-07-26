package team

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestCachePolicy_UseRefreshBypass(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		projectDir:      ws,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}

	// 1. Default policy is CacheUse
	if got := c.GetCachePolicy(); got != CacheUse {
		t.Fatalf("expected initial policy CacheUse, got %s", got)
	}

	// Store result under CacheUse
	c.storeTaskCache("agent-a", "task 1", "output-v1")
	out, ok := c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if !ok || out != "output-v1" {
		t.Fatalf("expected cache hit output-v1, got out=%q, ok=%v", out, ok)
	}

	// 2. Set policy to CacheBypass -> lookup should fail, store should be ignored
	c.SetCachePolicy(CacheBypass)
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if ok {
		t.Fatalf("expected cache miss under CacheBypass, got hit with %q", out)
	}
	c.storeTaskCache("agent-a", "task 2", "output-v2")

	// Switch back to CacheUse and verify task 2 was not stored
	c.SetCachePolicy(CacheUse)
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 2")
	if ok {
		t.Fatalf("task 2 should not have been stored under CacheBypass")
	}

	// 3. Set policy to CacheRefresh -> lookup should fail, but store should update cache
	c.SetCachePolicy(CacheRefresh)
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if ok {
		t.Fatalf("expected cache miss under CacheRefresh, got hit with %q", out)
	}

	c.storeTaskCache("agent-a", "task 1", "output-v2-refreshed")

	// Switch back to CacheUse and verify refreshed result is stored
	c.SetCachePolicy(CacheUse)
	out, ok = c.lookupTaskCache(context.Background(), "agent-a", "task 1")
	if !ok || out != "output-v2-refreshed" {
		t.Fatalf("expected refreshed output-v2-refreshed under CacheUse, got out=%q, ok=%v", out, ok)
	}
}

func TestCacheIdentity_FreshnessValidation(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		projectDir:      ws,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}

	// Store entry with initial identity
	c.storeTaskCache("agent-b", "build binary", "binary built successfully")

	// Lookup immediately -> hit
	out, ok := c.lookupTaskCache(context.Background(), "agent-b", "build binary")
	if !ok || out != "binary built successfully" {
		t.Fatalf("expected initial cache hit, got out=%q, ok=%v", out, ok)
	}

	// Simulate source code change by modifying a file in workspace
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Lookup after workspace modification -> should be a cache miss due to ProjectFingerprint mismatch
	out, ok = c.lookupTaskCache(context.Background(), "agent-b", "build binary")
	if ok {
		t.Fatalf("expected cache miss after workspace source code modification, got hit with %q", out)
	}
}

func TestCacheIdentity_TTLExpiration(t *testing.T) {
	target := CacheIdentity{
		AgentIdentity: "agent-c",
		TaskGoal:      "fetch news",
	}

	entry := cachedTaskEntry{
		taskDesc: "fetch news",
		identity: CacheIdentity{
			AgentIdentity: "agent-c",
			TaskGoal:      "fetch news",
			CreatedAt:     time.Now().Add(-10 * time.Minute),
			TTL:           5 * time.Minute,
		},
	}

	if entry.isFresh(target) {
		t.Fatal("entry with 10-minute age and 5-minute TTL should NOT be fresh")
	}

	entryFresh := cachedTaskEntry{
		taskDesc: "fetch news",
		identity: CacheIdentity{
			AgentIdentity: "agent-c",
			TaskGoal:      "fetch news",
			CreatedAt:     time.Now(),
			TTL:           5 * time.Minute,
		},
	}
	if !entryFresh.isFresh(target) {
		t.Fatal("entry with recent CreatedAt and 5-minute TTL SHOULD be fresh")
	}
}

func TestComputeRepoCommitAndFingerprint(t *testing.T) {
	ws := t.TempDir()

	// Initially empty non-git dir
	commit := ComputeRepoCommit(ws)
	if commit != "" {
		t.Fatalf("expected empty git commit for non-git dir, got %q", commit)
	}

	fp1 := ComputeProjectFingerprint(ws)

	// Create a file
	if err := os.WriteFile(filepath.Join(ws, "foo.txt"), []byte("bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp2 := ComputeProjectFingerprint(ws)
	if fp1 == fp2 {
		t.Fatalf("fingerprint should change when a file is added to workspace")
	}

	// Init git repo to test git commit & porcelain fingerprint
	cmd := exec.Command("git", "init")
	cmd.Dir = ws
	if err := cmd.Run(); err == nil {
		cmdConfig := exec.Command("git", "config", "user.email", "test@example.com")
		cmdConfig.Dir = ws
		_ = cmdConfig.Run()
		cmdConfigName := exec.Command("git", "config", "user.name", "Test User")
		cmdConfigName.Dir = ws
		_ = cmdConfigName.Run()

		cmdAdd := exec.Command("git", "add", "foo.txt")
		cmdAdd.Dir = ws
		_ = cmdAdd.Run()

		cmdCommit := exec.Command("git", "commit", "-m", "initial commit")
		cmdCommit.Dir = ws
		if err := cmdCommit.Run(); err == nil {
			gitCommitSha := ComputeRepoCommit(ws)
			if gitCommitSha == "" {
				t.Fatalf("expected valid git commit SHA after git commit")
			}
		}
	}
}

func TestCapabilityCache_TTLAndInvalidation(t *testing.T) {
	ws := t.TempDir()
	readyFile := filepath.Join(ws, "ready.txt")
	if err := os.WriteFile(readyFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: ws,
			Config:    agent.TeamConfig{Name: "test", Shell: "sh"},
		},
		projectDir:         ws,
		capabilityCache:    make(map[string]CapabilityResult),
		capabilityInflight: make(map[string]chan CapabilityResult),
	}

	req := agent.CapabilityRequirement{Name: "check-ready", Probe: "test -f ready.txt"}
	results, err := c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{req})
	if err != nil || len(results) != 1 || !results[0].Available {
		t.Fatalf("first capability check should succeed: %v", err)
	}

	// Remove file
	if err := os.Remove(readyFile); err != nil {
		t.Fatal(err)
	}

	// Cached probe should return true
	results, err = c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{req})
	if err != nil || len(results) != 1 || !results[0].Available {
		t.Fatalf("cached capability check should succeed: %v", err)
	}

	// Invalidate capability cache -> probe re-executes and should fail because file is gone
	c.InvalidateCapabilityCache()
	_, err = c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{req})
	if err == nil {
		t.Fatalf("capability check after invalidation should fail because ready.txt was removed")
	}

	// Always-fresh scope test
	reqFresh := agent.CapabilityRequirement{Name: "check-fresh", Probe: "test -f ready.txt", Scope: "always-fresh"}
	if err := os.WriteFile(readyFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err = c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{reqFresh})
	if err != nil || len(results) != 1 || !results[0].Available {
		t.Fatalf("always-fresh probe should succeed initially")
	}
	// Remove file and check again without calling InvalidateCapabilityCache
	_ = os.Remove(readyFile)
	_, err = c.checkCapabilityRequirements(context.Background(), []agent.CapabilityRequirement{reqFresh})
	if err == nil {
		t.Fatalf("always-fresh probe should re-probe every time and fail when file is removed")
	}
}

func TestIsCacheForbiddenTasks(t *testing.T) {
	c := &Coordinator{}
	if !c.IsCacheForbidden("run task [no-cache]", "") {
		t.Fatalf("expected [no-cache] to trigger cache forbidden")
	}
	if !c.IsCacheForbidden("deploy_prod to cluster", "") {
		t.Fatalf("expected deploy_prod to trigger cache forbidden")
	}
	if c.IsCacheForbidden("read doc file", "test -f doc.md") {
		t.Fatalf("normal task should be cacheable")
	}
}

func TestComputeProjectFingerprint_GitDirtyTreeContentSensitivity(t *testing.T) {
	ws := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")

	mainFile := filepath.Join(ws, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "initial commit")

	// Case 1: Tracked file content modification (porcelain status remains " M main.go")
	if err := os.WriteFile(mainFile, []byte("package main\nfunc main() { // v1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpTrackedV1 := ComputeProjectFingerprint(ws)

	if err := os.WriteFile(mainFile, []byte("package main\nfunc main() { // v2\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpTrackedV2 := ComputeProjectFingerprint(ws)

	if fpTrackedV1 == fpTrackedV2 {
		t.Fatalf("fingerprint should change when tracked file content is modified again, got identical %q", fpTrackedV1)
	}

	// Case 2: Staged file content modification (porcelain status remains "M  main.go")
	runGit("add", "main.go")
	fpStagedV1 := ComputeProjectFingerprint(ws)

	if err := os.WriteFile(mainFile, []byte("package main\nfunc main() { // v3 staged\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	fpStagedV2 := ComputeProjectFingerprint(ws)

	if fpStagedV1 == fpStagedV2 {
		t.Fatalf("fingerprint should change when staged file content is modified again, got identical %q", fpStagedV1)
	}

	// Case 3: Untracked file content modification (porcelain status remains "?? untracked.txt")
	untrackedFile := filepath.Join(ws, "untracked.txt")
	if err := os.WriteFile(untrackedFile, []byte("content v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpUntrackedV1 := ComputeProjectFingerprint(ws)

	if err := os.WriteFile(untrackedFile, []byte("content v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	fpUntrackedV2 := ComputeProjectFingerprint(ws)

	if fpUntrackedV1 == fpUntrackedV2 {
		t.Fatalf("fingerprint should change when untracked file content is modified, got identical %q", fpUntrackedV1)
	}
}

func TestCacheLookup_GitDirtyTreeInvalidation(t *testing.T) {
	ws := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}

	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")

	mainFile := filepath.Join(ws, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "init")

	// Modify main.go to dirty v1
	if err := os.WriteFile(mainFile, []byte("package main\n// dirty v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		projectDir:      ws,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}

	// Store task cache result under dirty v1
	c.storeTaskCache("builder", "build app", "build-v1-success")

	// Lookup immediately under dirty v1 -> HIT
	out, ok := c.lookupTaskCache(context.Background(), "builder", "build app")
	if !ok || out != "build-v1-success" {
		t.Fatalf("expected cache hit under dirty v1, got out=%q, ok=%v", out, ok)
	}

	// Modify main.go to dirty v2 (porcelain status remains " M main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n// dirty v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Lookup under dirty v2 -> MUST MISS due to fingerprint mismatch
	out, ok = c.lookupTaskCache(context.Background(), "builder", "build app")
	if ok {
		t.Fatalf("expected cache MISS after second tracked file modification, but got hit with %q", out)
	}
}

func TestComputeProjectFingerprint_SpecialCharacterPaths(t *testing.T) {
	testCases := []struct {
		name     string
		filename string
	}{
		{"SpaceInFilename", "file with space.txt"},
		{"QuotedFilename", "file \"quoted\".txt"},
		{"NewlineInFilename", "file\nwith\nnewline.txt"},
		{"UnicodeFilename", "檔名_測試_unicode.txt"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()

			runGit := func(args ...string) {
				cmd := exec.Command("git", args...)
				cmd.Dir = ws
				if err := cmd.Run(); err != nil {
					t.Fatalf("git %v failed: %v", args, err)
				}
			}

			runGit("init")
			runGit("config", "user.email", "test@example.com")
			runGit("config", "user.name", "Test User")

			targetFile := filepath.Join(ws, tc.filename)
			if err := os.WriteFile(targetFile, []byte("content v1"), 0o644); err != nil {
				t.Fatal(err)
			}

			fp1 := ComputeProjectFingerprint(ws)

			if err := os.WriteFile(targetFile, []byte("content v2"), 0o644); err != nil {
				t.Fatal(err)
			}

			fp2 := ComputeProjectFingerprint(ws)

			if fp1 == fp2 {
				t.Fatalf("fingerprint should change when %s (%q) content is modified, got identical %q", tc.name, tc.filename, fp1)
			}
		})
	}
}

func TestCacheLookup_GitCommandFailureIneligible(t *testing.T) {
	ws := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}

	// 1. Normal git repo with commit -> Store & Lookup succeed (HIT)
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")

	mainFile := filepath.Join(ws, "main.go")
	if err := os.WriteFile(mainFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "main.go")
	runGit("commit", "-m", "init")

	c := &Coordinator{
		projectDir:      ws,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	ctx := context.Background()

	normId := c.ComputeCacheIdentity("builder", "compile project", "", "")
	if normId.HasError {
		t.Fatalf("expected HasError=false on normal git repo, got true")
	}

	c.storeTaskCache("builder", "compile project", "binary-v1")
	out, ok := c.lookupTaskCache(ctx, "builder", "compile project")
	if !ok || out != "binary-v1" {
		t.Fatalf("expected cache HIT under normal git repo, got out=%q, ok=%v", out, ok)
	}

	// 2. Real Git command failure via no-commit Git repository (isGitRepo=true, rev-parse HEAD fails)
	wsNoCommit := t.TempDir()
	cmdInit := exec.Command("git", "init")
	cmdInit.Dir = wsNoCommit
	if err := cmdInit.Run(); err != nil {
		t.Fatalf("git init failed: %v", err)
	}

	cNoCommit := &Coordinator{
		projectDir:      wsNoCommit,
		taskResultCache: make(map[string][]cachedTaskEntry),
	}

	// Verify production ComputeCacheIdentity sets HasError=true via real git rev-parse HEAD failure
	errId := cNoCommit.ComputeCacheIdentity("builder", "compile project", "", "")
	if !errId.HasError {
		t.Fatalf("expected HasError=true when git rev-parse HEAD fails on empty repo, got false")
	}

	// Store task cache result through production method
	cNoCommit.storeTaskCache("builder", "compile project", "binary-err")

	// Production lookupTaskCache under real git failure identity MUST MISS (ok == false)
	outErr, okErr := cNoCommit.lookupTaskCache(ctx, "builder", "compile project")
	if okErr {
		t.Fatalf("expected lookupTaskCache MISS under real git command failure identity, but got HIT with %q", outErr)
	}
}
