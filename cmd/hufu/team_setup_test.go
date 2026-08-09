package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/team"
)

func TestLoadTeamCommon_RejectsStrictWorkspaceBeforeWrite(t *testing.T) {
	projDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(projDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	// Target workspace is inside the subject project directory
	workspaceDir := filepath.Join(projDir, "workspace")

	session := &team.TeamSession{
		Workspace: workspaceDir,
		Dir:       filepath.Join(projDir, ".agent-teams", "test-team"),
		Config: agent.TeamConfig{
			Name:             "test-team",
			ExecutionProfile: "strict-verification",
		},
	}

	origProfile := opts.executionProfile
	opts.executionProfile = "strict-verification"
	t.Cleanup(func() {
		opts.executionProfile = origProfile
	})

	ctx := context.Background()
	_, err = loadTeamCommon(ctx, "test-team", session, "", "", nil, nil, nil, false, false, false)
	if err == nil {
		t.Fatal("expected workspace isolation error from loadTeamCommon, got nil")
	}

	// Verify that the rejected strict workspace directory WAS NOT CREATED on disk
	if _, statErr := os.Stat(workspaceDir); !os.IsNotExist(statErr) {
		t.Errorf("workspace directory %q was created despite isolation failure: %v", workspaceDir, statErr)
	}
}

func TestLoadTeamByName_RejectsStrictWorkspaceWithoutCreatingDirectory(t *testing.T) {
	projDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(projDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	teamDir := filepath.Join(projDir, ".agent-teams", "dev-team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	teamYaml := `name: dev-team
description: Dev team
agents:
  helper:
    role: worker
`
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(teamYaml), 0o644); err != nil {
		t.Fatalf("WriteFile team.yaml failed: %v", err)
	}

	origProfile := opts.executionProfile
	opts.executionProfile = "strict-verification"
	t.Cleanup(func() {
		opts.executionProfile = origProfile
	})

	parentWorkspace := filepath.Join(projDir, "workspace")
	expectedWorkspace := filepath.Join(parentWorkspace, "dev-team")

	registry := team.NewTeamRegistry([]string{filepath.Join(projDir, ".agent-teams")})
	ctx := context.Background()

	_, err = loadTeamByName(ctx, "dev-team", registry, "", "", nil, nil, nil, false, false)
	if err == nil {
		t.Fatal("expected workspace isolation error from loadTeamByName, got nil")
	}

	if _, statErr := os.Stat(parentWorkspace); !os.IsNotExist(statErr) {
		t.Errorf("expected parent workspace directory %q to not exist, but it was created: %v", parentWorkspace, statErr)
	}
	if _, statErr := os.Stat(expectedWorkspace); !os.IsNotExist(statErr) {
		t.Errorf("expected workspace directory %q to not exist, but it was created: %v", expectedWorkspace, statErr)
	}
}

func TestLoadDefaultTeam_RejectsStrictWorkspaceWithoutCreatingDirectory(t *testing.T) {
	projDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(projDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
	})

	origProfile := opts.executionProfile
	opts.executionProfile = "strict-verification"
	t.Cleanup(func() {
		opts.executionProfile = origProfile
	})

	parentWorkspace := filepath.Join(projDir, "workspace")
	expectedWorkspace := filepath.Join(parentWorkspace, "default")

	ctx := context.Background()
	_, err = loadDefaultTeam(ctx, "", "", nil, nil, nil, false, false)
	if err == nil {
		t.Fatal("expected workspace isolation error from loadDefaultTeam, got nil")
	}

	if _, statErr := os.Stat(parentWorkspace); !os.IsNotExist(statErr) {
		t.Errorf("expected parent workspace directory %q to not exist, but it was created: %v", parentWorkspace, statErr)
	}
	if _, statErr := os.Stat(expectedWorkspace); !os.IsNotExist(statErr) {
		t.Errorf("expected workspace directory %q to not exist, but it was created: %v", expectedWorkspace, statErr)
	}
}

func TestArchiveToMemory_SkipsWhenDisableHistoricalMemory(t *testing.T) {
	tmpDir := t.TempDir()
	session := &team.TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}

	c, err := team.NewCoordinator(session, "", "", nil, nil, nil, team.RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	freshProf, _ := team.GetBuiltinProfile(string(team.ProfileFreshVerification))
	c.SetExecutionProfile(freshProf)

	// Provide non-empty old session entries
	oldEntries := []memory.SessionSummaryEntry{
		{Role: "user", Content: "hello"},
	}

	// archiveToMemory should return early without attempting store operations when DisableHistoricalMemory is true
	archiveToMemory(context.Background(), nil, c, session, oldEntries)
}
