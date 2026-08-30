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

func TestDetectContextLengths_NoNetSkipsProviderProbe(t *testing.T) {
	called := false
	original := detectOllamaContextLengths
	detectOllamaContextLengths = func(context.Context, string, string, []string) { called = true }
	t.Cleanup(func() { detectOllamaContextLengths = original })
	detectContextLengths(context.Background(), true, "http://provider.invalid/v1", "", []string{"ollama/no-net-regression:latest"})
	if called {
		t.Fatal("no-net setup sent a context-length probe to the provider")
	}
}

func TestDetectContextLengths_ProbesWhenNetworkAllowed(t *testing.T) {
	called := false
	original := detectOllamaContextLengths
	detectOllamaContextLengths = func(context.Context, string, string, []string) { called = true }
	t.Cleanup(func() { detectOllamaContextLengths = original })
	detectContextLengths(context.Background(), false, "http://provider.invalid/v1", "", []string{"ollama/net-regression:latest"})
	if !called {
		t.Fatal("network-allowed setup did not perform context-length probe")
	}
}

func TestConfiguredContextWindowIsAppliedWhenNoNet(t *testing.T) {
	modelID := "no-net-configured-context-model"
	team.RegisterConfiguredContextWindow([]string{modelID}, 16384)
	spec := team.GlobalModelSpecRegistry().GetSpec(modelID)
	if spec.ContextWindow != 16384 || spec.ContextWindowSource != "operator" || spec.IsEstimated {
		t.Fatalf("no-net configured context spec = %#v, want operator exact capacity", spec)
	}
}

func TestApplyCLICompactionOverrides_InvalidPolicyUsesDefaults(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	opts.compactionMaxHistoryMessages = 0
	opts.compactionRetainHistoryMessages = 0
	opts.compactionVerifiedHistoryTargetTokens = 0
	opts.compactionToolOutputMaxBytes = 0
	opts.compactionToolOutputMaxRunes = 0
	opts.compactionToolOutputMaxTokens = 0
	opts.compactionDiagnosticMaxLines = 0
	opts.compactionDiagnosticMaxTokens = 0

	session := &team.TeamSession{}
	if err := applyCLICompactionOverrides(session); err != nil {
		t.Fatalf("applyCLICompactionOverrides failed: %v", err)
	}

	want := agent.DefaultCompactionPolicy()
	if session.Config.Compaction != want {
		t.Fatalf("compaction policy = %#v, want %#v", session.Config.Compaction, want)
	}
	if err := session.Config.Compaction.Validate(); err != nil {
		t.Fatalf("default compaction policy is invalid: %v", err)
	}
}

func TestChatExposesAndAppliesAllCompactionOverrides(t *testing.T) {
	for _, name := range []string{
		"compaction-max-history-messages",
		"compaction-retain-history-messages",
		"compaction-verified-history-target-tokens",
		"compaction-tool-output-max-bytes",
		"compaction-tool-output-max-runes",
		"compaction-tool-output-max-tokens",
		"compaction-diagnostic-max-lines",
		"compaction-diagnostic-max-tokens",
	} {
		if replCmd.Flags().Lookup(name) == nil {
			t.Fatalf("chat flag --%s is not registered", name)
		}
	}

	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	base := agent.DefaultCompactionPolicy()
	base.MaxHistoryMessages = 50
	base.RetainHistoryMessages = 40
	base.VerifiedHistoryTargetTokens = 9000
	base.ToolOutputMaxBytes = 2000
	base.ToolOutputMaxRunes = 500
	base.ToolOutputMaxTokens = 180
	base.DiagnosticMaxLines = 10
	base.DiagnosticMaxTokens = 90
	opts.compactionMaxHistoryMessages = 40
	opts.compactionRetainHistoryMessages = 30
	opts.compactionVerifiedHistoryTargetTokens = 8000
	opts.compactionToolOutputMaxBytes = 1000
	opts.compactionToolOutputMaxRunes = 200
	opts.compactionToolOutputMaxTokens = 100
	opts.compactionDiagnosticMaxLines = 8
	opts.compactionDiagnosticMaxTokens = 40
	session := &team.TeamSession{Config: agent.TeamConfig{Compaction: base}}
	if err := applyCLICompactionOverrides(session); err != nil {
		t.Fatal(err)
	}
	if got := session.Config.Compaction; got.MaxHistoryMessages != 40 || got.RetainHistoryMessages != 30 || got.VerifiedHistoryTargetTokens != 8000 || got.ToolOutputMaxBytes != 1000 || got.ToolOutputMaxRunes != 200 || got.ToolOutputMaxTokens != 100 || got.DiagnosticMaxLines != 8 || got.DiagnosticMaxTokens != 40 {
		t.Fatalf("chat compaction overrides = %#v", got)
	}
}

func TestCompactionCLIRejectsImpossibleToolOutputCapsAndKeepsZeroAsDefault(t *testing.T) {
	for _, name := range []string{"compaction-tool-output-max-bytes", "compaction-tool-output-max-runes", "compaction-tool-output-max-tokens"} {
		if replCmd.Flags().Lookup(name) == nil {
			t.Fatalf("compaction flag --%s is not registered for chat", name)
		}
	}

	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	for name, set := range map[string]func(){
		"bytes":  func() { opts.compactionToolOutputMaxBytes = 1 },
		"runes":  func() { opts.compactionToolOutputMaxRunes = 1 },
		"tokens": func() { opts.compactionToolOutputMaxTokens = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			opts = originalOpts
			set()
			if err := applyCLICompactionOverrides(&team.TeamSession{}); err == nil {
				t.Fatal("impossible CLI tool-output cap was accepted")
			}
		})
	}

	opts = originalOpts
	opts.compactionToolOutputMaxBytes = 0
	opts.compactionToolOutputMaxRunes = 0
	opts.compactionToolOutputMaxTokens = 0
	session := &team.TeamSession{}
	if err := applyCLICompactionOverrides(session); err != nil {
		t.Fatalf("zero CLI caps should retain defaults: %v", err)
	}
	if session.Config.Compaction != agent.DefaultCompactionPolicy() {
		t.Fatalf("zero CLI caps changed default policy: %#v", session.Config.Compaction)
	}
}

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
