package main

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestDisplayResolvedConfig(t *testing.T) {
	// Just ensure the function does not panic on empty inputs.
	session := &team.TeamSession{
		Agents: map[string]*agent.AgentDef{},
	}
	displayResolvedConfig(session, nil, "", "", "", "", 8, team.ExecutionProfile{})
}

func TestBuildAllowedPaths(t *testing.T) {
	tmpDir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
	})

	session := &team.TeamSession{
		Workspace: filepath.Join(tmpDir, "workspace"),
		Dir:       filepath.Join(tmpDir, "team"),
	}
	registry := team.NewTeamRegistry([]string{tmpDir})
	cfg := &config.Config{}
	origAllowPaths := opts.allowPaths
	opts.allowPaths = nil
	t.Cleanup(func() {
		opts.allowPaths = origAllowPaths
	})
	paths := buildAllowedPaths(session, registry, cfg)
	if len(paths) == 0 {
		t.Error("expected at least one allowed path")
	}
	if paths[0] != tmpDir {
		t.Fatalf("expected cwd %q first in allowed paths, got %q", tmpDir, paths[0])
	}

	noRegistryPaths := buildAllowedPaths(session, nil, cfg)
	if len(noRegistryPaths) == 0 || noRegistryPaths[0] != tmpDir {
		t.Fatalf("expected cwd %q first even without registry, got %#v", tmpDir, noRegistryPaths)
	}

	opts.allowPaths = []string{filepath.Join(tmpDir, "extra"), "~/more"}
	withExtra := buildAllowedPaths(session, nil, cfg)
	if !containsPath(withExtra, filepath.Join(tmpDir, "extra")) {
		t.Fatalf("expected extra allow path to be included, got %#v", withExtra)
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func TestSortedAgents(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"charlie": {Name: "charlie"},
		"alpha":   {Name: "alpha"},
		"bravo":   {Name: "bravo"},
	}
	got := sortedAgents(agents)
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != 3 {
		t.Fatalf("expected 3 agents, got %d", len(got))
	}
	for i, a := range got {
		if a.Name != want[i] {
			t.Errorf("position %d: got %q, want %q", i, a.Name, want[i])
		}
	}
}

func TestAgentNamesFromSession(t *testing.T) {
	session := &team.TeamSession{
		Agents: map[string]*agent.AgentDef{
			"coordinator":  {Name: "coordinator", Role: "coordinator"},
			"orchestrator": {Name: "orchestrator", Role: "orchestrator"},
			"worker-a":     {Name: "worker-a", Role: "worker"},
			"worker-b":     {Name: "worker-b", Role: "worker"},
		},
	}
	got := agentNamesFromSession(session)
	// Should exclude coordinator and orchestrator
	if len(got) != 2 {
		t.Fatalf("expected 2 worker agents, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, a := range got {
		seen[a.Name] = true
	}
	if !seen["worker-a"] || !seen["worker-b"] {
		t.Errorf("expected worker-a and worker-b, got %v", got)
	}
}

func TestPrepareSessionLifecycle_CorruptArchivedSessionStartsInitialPhase(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "phase-test"}}
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("write corrupt session: %v", err)
	}
	// A readable transcript is archived while the corrupt JSON is discarded;
	// neither is allowed to become canonical task/delegation state.
	if err := team.SaveSessionMD(workspace, "# Prior session\n\nclaimed a worker completed\n"); err != nil {
		t.Fatalf("write session markdown: %v", err)
	}

	original := opts
	opts.newSession = true
	t.Cleanup(func() { opts = original })

	sd, archived, err := prepareSessionLifecycle(session, false)
	if err != nil {
		t.Fatalf("prepare lifecycle: %v", err)
	}
	if len(archived) != 0 {
		t.Fatalf("corrupt JSON unexpectedly yielded archive entries: %#v", archived)
	}
	if sd == nil || sd.DelegationPhase != team.DelegationPhaseInitialPending || len(sd.Tasks) != 0 {
		t.Fatalf("fresh replacement session retained non-canonical delegation state: %#v", sd)
	}
	if persisted := team.LoadSession(workspace); persisted == nil || persisted.DelegationPhase != team.DelegationPhaseInitialPending || len(persisted.Tasks) != 0 {
		t.Fatalf("persisted fresh session = %#v, want empty initial-pending session", persisted)
	}
	history, err := filepath.Glob(filepath.Join(workspace, "history", "*.md"))
	if err != nil || len(history) != 1 {
		t.Fatalf("archived transcript = %v, err=%v; want one history file", history, err)
	}
}

func TestPrepareSessionLifecycle_ForcedFreshDoesNotReuseCheckpoint(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "fresh-profile-test"}}
	if err := team.SaveSession(workspace, &team.SessionData{
		Tasks: []*team.TodoItem{{ID: "old", Agent: "reviewer", Status: team.TaskDone, Output: "old result"}},
	}); err != nil {
		t.Fatalf("save prior session: %v", err)
	}

	original := opts
	opts.newSession = false
	t.Cleanup(func() { opts = original })

	sd, _, err := prepareSessionLifecycle(session, true)
	if err != nil {
		t.Fatalf("prepare forced-fresh lifecycle: %v", err)
	}
	if sd == nil || len(sd.Tasks) != 0 || sd.DelegationPhase != team.DelegationPhaseInitialPending {
		t.Fatalf("forced-fresh lifecycle reused checkpoint: %#v", sd)
	}
}

func TestArchivePreviousSessionWithMarkdownClearsConversationHistory(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "fresh-test"}}
	if err := team.SaveSessionMD(workspace, "# prior session\n"); err != nil {
		t.Fatalf("SaveSessionMD: %v", err)
	}
	if err := team.SaveConversationHistory(workspace, []fantasy.Message{fantasy.NewUserMessage("stale completed review")}); err != nil {
		t.Fatalf("SaveConversationHistory: %v", err)
	}

	if err := archivePreviousSession(session); err != nil {
		t.Fatalf("archivePreviousSession: %v", err)
	}
	if team.HasConversationHistory(workspace) {
		t.Fatal("--new archive retained chat_history.md after archiving session markdown")
	}
}
