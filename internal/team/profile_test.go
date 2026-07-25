package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/hooks"
)

func TestProfile_BuiltinProfiles(t *testing.T) {
	profiles := BuiltinProfiles()
	if len(profiles) < 4 {
		t.Fatalf("expected at least 4 builtin profiles, got %d", len(profiles))
	}

	expectedNames := []ExecutionProfileName{
		ProfileDefault,
		ProfileUnattended,
		ProfileStrictVerification,
		ProfileFreshVerification,
	}

	for _, name := range expectedNames {
		p, ok := GetBuiltinProfile(string(name))
		if !ok {
			t.Errorf("GetBuiltinProfile(%q) returned false", name)
		}
		if p.Name != name {
			t.Errorf("GetBuiltinProfile(%q).Name = %q, want %q", name, p.Name, name)
		}
	}

	strict := profiles[ProfileStrictVerification]
	if !strict.StrictPolicy {
		t.Error("strict-verification StrictPolicy want true")
	}
	if strict.PolicyFailureMode != PolicyFailClosed {
		t.Errorf("strict-verification PolicyFailureMode = %q, want closed", strict.PolicyFailureMode)
	}
	if strict.AcceptanceMode != AcceptanceBlocking {
		t.Errorf("strict-verification AcceptanceMode = %q, want blocking", strict.AcceptanceMode)
	}
	if strict.DefaultCachePolicy != CacheUse {
		t.Errorf("strict-verification DefaultCachePolicy = %q, want CacheUse", strict.DefaultCachePolicy)
	}
	if strict.DisableHistoricalMemory || strict.DisableHistoricalTaskReuse {
		t.Error("strict-verification allows historical memory and task reuse on active session")
	}

	fresh := profiles[ProfileFreshVerification]
	if !fresh.StrictPolicy {
		t.Error("fresh-verification StrictPolicy want true")
	}
	if fresh.DefaultCachePolicy != CacheBypass {
		t.Errorf("fresh-verification DefaultCachePolicy = %q, want bypass", fresh.DefaultCachePolicy)
	}
	if !fresh.DisableHistoricalMemory || !fresh.DisableHistoricalTaskReuse || !fresh.DisableJournalRestore || !fresh.DisableTaskCache {
		t.Error("fresh-verification must disable historical memory, task reuse, journal restore, and task cache")
	}
}

func TestProfile_ResolutionPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		cli         string
		team        string
		wantName    ExecutionProfileName
		wantErr     bool
	}{
		{
			name:     "CLI flag overrides team.yml",
			cli:      "strict-verification",
			team:     "unattended",
			wantName: ProfileStrictVerification,
		},
		{
			name:     "team.yml used when CLI flag is empty",
			cli:      "",
			team:     "unattended",
			wantName: ProfileUnattended,
		},
		{
			name:     "default profile when both are empty",
			cli:      "",
			team:     "",
			wantName: ProfileDefault,
		},
		{
			name:     "fresh-verification via CLI",
			cli:      "fresh-verification",
			team:     "",
			wantName: ProfileFreshVerification,
		},
		{
			name:    "unknown profile name returns error",
			cli:     "invalid-profile",
			team:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveExecutionProfile(tt.cli, tt.team)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveExecutionProfile(%q, %q) error = %v, wantErr %v", tt.cli, tt.team, err, tt.wantErr)
			}
			if !tt.wantErr && got.Name != tt.wantName {
				t.Errorf("ResolveExecutionProfile(%q, %q) = %q, want %q", tt.cli, tt.team, got.Name, tt.wantName)
			}
		})
	}
}

func TestProfile_FreshVerificationIsolation(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	// Create non-empty stm.md and ltm.md in workspace
	if err := SaveSTM(tmpDir, "# 發現\n- old finding entry\n"); err != nil {
		t.Fatalf("SaveSTM failed: %v", err)
	}
	if err := SaveLTM(tmpDir, "test-team", "# Architecture\n- old ltm pattern\n"); err != nil {
		t.Fatalf("SaveLTM failed: %v", err)
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	freshProf, _ := GetBuiltinProfile(string(ProfileFreshVerification))
	c.SetExecutionProfile(freshProf)

	// Verify profile is set on coordinator
	if got := c.ExecutionProfile(); got.Name != ProfileFreshVerification {
		t.Fatalf("c.ExecutionProfile() = %q, want %q", got.Name, ProfileFreshVerification)
	}

	// 1. Verify CachePolicy returns CacheBypass under fresh-verification
	if got := c.GetCachePolicy(); got != CacheBypass {
		t.Errorf("GetCachePolicy() under fresh-verification = %q, want bypass", got)
	}

	// 2. Verify SetSessionData does NOT restore task cache or task list when DisableHistoricalTaskReuse is true
	pastSessionData := &SessionData{
		Rounds: 5,
		Tasks: []*TodoItem{
			{ID: "task-1", Agent: "helper", Desc: "past task", Status: TaskDone, Output: "past output"},
		},
		Entries: []SessionEntry{
			{Role: "user", Content: "hello", Timestamp: time.Now().Format(time.RFC3339)},
		},
	}
	c.SetSessionData(pastSessionData)

	c.taskResultCacheMu.RLock()
	cacheLen := len(c.taskResultCache["helper"])
	c.taskResultCacheMu.RUnlock()
	if cacheLen != 0 {
		t.Errorf("taskResultCache length under fresh-verification = %d, want 0 (historical task reuse disabled)", cacheLen)
	}

	// 3. Verify ResumeInterruptedTasks returns 0 (bypassed) when historical reuse/journal restore is disabled
	resumedCount, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("ResumeInterruptedTasks error = %v", err)
	}
	if resumedCount != 0 {
		t.Errorf("ResumeInterruptedTasks() = %d, want 0", resumedCount)
	}

	// 4. Verify memory loading helpers return empty strings when DisableHistoricalMemory is true
	if got := c.buildMemorySuffix("coordinator"); got != "" {
		t.Errorf("buildMemorySuffix() under fresh-verification = %q, want empty string", got)
	}
	if got := c.buildTaskSTMContext(); got != "" {
		t.Errorf("buildTaskSTMContext() under fresh-verification = %q, want empty string", got)
	}
	if got := c.buildLTMContext(); got != "" {
		t.Errorf("buildLTMContext() under fresh-verification = %q, want empty string", got)
	}
}

func TestProfile_AcceptanceModeBlocking(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team", Acceptance: "exit 1"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	strictProf.RequireEvidenceManifest = false // focus test on acceptance check
	c.SetExecutionProfile(strictProf)
	c.acceptanceCmd = "exit 1"
	c.selfHealingAttempts = 2 // exhaust self healing so blocking mode fails immediately

	finishTool := &finishTool{coordinator: c}
	raw, err := finishTool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}

	textResp := fmt.Sprintf("%+v", raw)
	if c.finishCalled.Load() {
		t.Error("finishCalled set to true despite failing acceptance check in AcceptanceBlocking mode")
	}
	if !strings.Contains(textResp, "Acceptance check failed") {
		t.Errorf("expected acceptance failure error in response, got %q", textResp)
	}
}

func TestProfile_RequireEvidenceManifest(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)

	finishTool := &finishTool{coordinator: c}
	raw, err := finishTool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}

	textResp := fmt.Sprintf("%+v", raw)
	if c.finishCalled.Load() {
		t.Error("finishCalled set to true despite missing evidence manifest under RequireEvidenceManifest")
	}
	if !strings.Contains(textResp, "RequireEvidenceManifest policy violation") {
		t.Errorf("expected RequireEvidenceManifest error, got %q", textResp)
	}
}

func TestProfile_RequireClosedTerminals(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)

	// Set terminal session manager with an active session
	mgr, err := NewTerminalSessionManager(tmpDir, nil)
	if err != nil {
		t.Fatalf("NewTerminalSessionManager failed: %v", err)
	}
	session1, _ := mgr.Start(context.Background(), TerminalStartRequest{OwnerTaskID: "task-1", Command: []string{"echo", "hi"}})
	if session1 != nil {
		defer func() { _ = mgr.Close(context.Background(), session1.ID) }()
	}
	c.terminalSessionMgr = mgr

	finishTool := &finishTool{coordinator: c}
	raw, err := finishTool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"done"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}
	textResp := fmt.Sprintf("%+v", raw)
	if c.finishCalled.Load() {
		t.Error("finishCalled set to true despite active open terminals under RequireClosedTerminals")
	}
	if !strings.Contains(textResp, "RequireClosedTerminals policy violation") && !strings.Contains(textResp, "unresolved") {
		t.Errorf("expected RequireClosedTerminals error, got %q", textResp)
	}
}

func TestProfile_PolicyFailClosed(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)

	// In strictProf (PolicyFailClosed), guardReviewer with nil GuardSidecar returns false (denies call)
	c.mu.Lock()
	coreTools := c.coreTools
	c.mu.Unlock()

	var guardFn func(ctx context.Context, toolName, args string, rules []string) (bool, string, error)
	for _, tool := range coreTools {
		if hasGuard, ok := tool.(interface {
			GuardReviewer() func(ctx context.Context, toolName, args string, rules []string) (bool, string, error)
		}); ok {
			guardFn = hasGuard.GuardReviewer()
			if guardFn != nil {
				break
			}
		}
	}
	if guardFn != nil {
		approved, reason, err := guardFn(context.Background(), "bash", `{"command":"rm -rf /"}`, []string{"no rm"})
		if approved {
			t.Error("guardReviewer approved tool call despite nil guard sidecar under PolicyFailClosed")
		}
		if err == nil {
			t.Error("expected error from guardReviewer under PolicyFailClosed when sidecar is nil")
		}
		_ = reason
	}
}

func TestProfile_RecoveryPolicyResolution(t *testing.T) {
	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))

	// Without explicit policy, profile DefaultRecoveryPolicy (reconcile) is used instead of retry
	pol := ResolveRecoveryPolicy("", SideEffectWorkspaceWrite, false, strictProf)
	if pol != RecoveryReconcile {
		t.Errorf("ResolveRecoveryPolicy() = %q, want %q", pol, RecoveryReconcile)
	}

	// Explicit policy always takes precedence
	polExplicit := ResolveRecoveryPolicy(RecoveryNever, SideEffectWorkspaceWrite, false, strictProf)
	if polExplicit != RecoveryNever {
		t.Errorf("ResolveRecoveryPolicy() explicit = %q, want %q", polExplicit, RecoveryNever)
	}
}

func TestProfile_TeamYMLParsing(t *testing.T) {
	tmpDir := t.TempDir()
	teamYMLContent := `name: test-team
execution-profile: strict-verification
timeout: 300
`
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yml"), []byte(teamYMLContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML failed: %v", err)
	}

	if cfg.ExecutionProfile != "strict-verification" {
		t.Errorf("cfg.ExecutionProfile = %q, want strict-verification", cfg.ExecutionProfile)
	}

	prof, err := ResolveExecutionProfile("", cfg.ExecutionProfile)
	if err != nil {
		t.Fatalf("ResolveExecutionProfile failed: %v", err)
	}
	if prof.Name != ProfileStrictVerification {
		t.Errorf("resolved profile name = %q, want %q", prof.Name, ProfileStrictVerification)
	}
}

func TestProfile_RequireWorkspaceIsolation(t *testing.T) {
	projDir := t.TempDir()
	isolatedDir := t.TempDir()
	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))

	// Case 1: Named team with workspace == project root (should fail)
	teamDir := filepath.Join(projDir, ".agent-teams", "team-a")
	sessionProjRoot := &TeamSession{
		Workspace: projDir,
		Dir:       teamDir,
		Config:    agent.TeamConfig{Name: "team-a"},
	}
	c1, err := NewCoordinator(sessionProjRoot, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c1.projectDir = projDir
	c1.SetExecutionProfile(strictProf)
	if err := c1.ValidateWorkspaceIsolation(); err == nil {
		t.Error("expected error when Workspace == project root for named team, got nil")
	}

	// Case 2: Named team with workspace == project subdir (should fail under strict isolation)
	subDir := filepath.Join(projDir, "workspace")
	sessionSubdir := &TeamSession{
		Workspace: subDir,
		Dir:       teamDir,
		Config:    agent.TeamConfig{Name: "team-a"},
	}
	c2, err := NewCoordinator(sessionSubdir, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c2.projectDir = projDir
	c2.SetExecutionProfile(strictProf)
	if err := c2.ValidateWorkspaceIsolation(); err == nil {
		t.Error("expected error when Workspace is inside project root (project subdir), got nil")
	}

	// Case 3: Named team with workspace inside team definition dir (should fail)
	sessionControlDir := &TeamSession{
		Workspace: teamDir,
		Dir:       teamDir,
		Config:    agent.TeamConfig{Name: "team-a"},
	}
	c3, err := NewCoordinator(sessionControlDir, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c3.projectDir = projDir
	c3.SetExecutionProfile(strictProf)
	if err := c3.ValidateWorkspaceIsolation(); err == nil {
		t.Error("expected error when Workspace == team control dir, got nil")
	}

	// Case 4: Named team with workspace outside project root & team dir (should succeed)
	sessionIsolated := &TeamSession{
		Workspace: isolatedDir,
		Dir:       teamDir,
		Config:    agent.TeamConfig{Name: "team-a"},
	}
	c4, err := NewCoordinator(sessionIsolated, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c4.projectDir = projDir
	c4.SetExecutionProfile(strictProf)
	if err := c4.ValidateWorkspaceIsolation(); err != nil {
		t.Errorf("expected success for completely isolated workspace directory outside project root, got: %v", err)
	}

	// Case 5: Default team with workspace == project root (should fail)
	sessionDefaultRoot := &TeamSession{
		Workspace: projDir,
		Dir:       projDir,
		Config:    agent.TeamConfig{Name: "default"},
	}
	c5, err := NewCoordinator(sessionDefaultRoot, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c5.projectDir = projDir
	c5.SetExecutionProfile(strictProf)
	if err := c5.ValidateWorkspaceIsolation(); err == nil {
		t.Error("expected error when default team Workspace == project root, got nil")
	}

	// Case 6: Default team with workspace == project subdir (should fail)
	sessionDefaultSubdir := &TeamSession{
		Workspace: subDir,
		Dir:       subDir,
		Config:    agent.TeamConfig{Name: "default"},
	}
	c6, err := NewCoordinator(sessionDefaultSubdir, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c6.projectDir = projDir
	c6.SetExecutionProfile(strictProf)
	if err := c6.ValidateWorkspaceIsolation(); err == nil {
		t.Error("expected error for default team with workspace inside project root, got nil")
	}

	// Case 7: Default team with workspace outside project root (should succeed)
	sessionDefaultIsolated := &TeamSession{
		Workspace: isolatedDir,
		Dir:       isolatedDir,
		Config:    agent.TeamConfig{Name: "default"},
	}
	c7, err := NewCoordinator(sessionDefaultIsolated, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	c7.projectDir = projDir
	c7.SetExecutionProfile(strictProf)
	if err := c7.ValidateWorkspaceIsolation(); err != nil {
		t.Errorf("expected success for default team with isolated workspace directory, got: %v", err)
	}
}

func TestProfile_RequireLockedResources(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "workspace")
	session := &TeamSession{
		Workspace: subDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)

	// ValidateResourceLocks creates workspace dirs and returns nil when no failing capability requirements exist
	if err := c.ValidateResourceLocks(context.Background()); err != nil {
		t.Errorf("ValidateResourceLocks failed: %v", err)
	}
}

func TestProfile_HookFailureMode(t *testing.T) {
	reg := hooks.NewHookRegistry()
	reg.SetFailureMode(hooks.PolicyFailClosed)

	// Register shell hook with a command that exits non-zero
	if err := hooks.RegisterShellHooks(reg, map[string]string{"before_tool_call": "exit 1"}); err != nil {
		t.Fatalf("RegisterShellHooks failed: %v", err)
	}

	payload := hooks.HookPayload{
		HookPoint: "before_tool_call",
		ToolName:  "bash",
	}

	resp := reg.Dispatch(context.Background(), "before_tool_call", payload)
	if resp.Result != hooks.HookError {
		t.Errorf("expected HookError under PolicyFailClosed when shell hook exits 1, got %v", resp.Result)
	}
	if !strings.Contains(resp.ErrorMessage, "failed") {
		t.Errorf("expected error message to mention failure, got %q", resp.ErrorMessage)
	}
}

func TestProfile_UnattendedProfile(t *testing.T) {
	unattendedProf, ok := GetBuiltinProfile(string(ProfileUnattended))
	if !ok {
		t.Fatal("GetBuiltinProfile(unattended) returned false")
	}
	if !unattendedProf.IsUnattended() {
		t.Error("IsUnattended() want true for ProfileUnattended")
	}

	defaultProf, _ := GetBuiltinProfile(string(ProfileDefault))
	if defaultProf.IsUnattended() {
		t.Error("IsUnattended() want false for ProfileDefault")
	}
}

func TestProfile_RequireEvidenceManifest_ManifestGateAndAcknowledge(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "workspace")
	session := &TeamSession{
		Workspace: subDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)

	// Case 1: Done task missing evidence
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "helper", Desc: "done task", Source: TaskSourceCoordinator}})
	items := c.taskTracker.TodoList().Items()
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "output")

	finishTool := &finishTool{coordinator: c}
	raw, err := finishTool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"done"}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}
	textResp := fmt.Sprintf("%+v", raw)
	if !strings.Contains(textResp, "missing verification evidence") {
		t.Errorf("expected missing verification evidence error for done task without evidence, got %q", textResp)
	}

	// Provide evidence for the completed task
	items[0].VerifyResult = &VerificationResult{ExitCode: 0}

	// Case 2: Failed task cannot be acknowledged in strict mode
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "helper", Desc: "failed task", Source: TaskSourceCoordinator}})
	itemsAll := c.taskTracker.TodoList().Items()
	failedItem := itemsAll[len(itemsAll)-1]
	c.taskTracker.TodoList().UpdateStatus(failedItem.ID, TaskError, "failed task error")

	rawAck, err := finishTool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"done", "acknowledge_failed_tasks": true}`})
	if err != nil {
		t.Fatalf("finishTool.Run failed: %v", err)
	}
	textAckResp := fmt.Sprintf("%+v", rawAck)
	if !strings.Contains(textAckResp, "cannot finish while worker tasks failed or were blocked") {
		t.Errorf("expected failed tasks violation even with acknowledge_failed_tasks under strict profile, got %q", textAckResp)
	}
}

func TestFreshProfile_DoesNotInheritRounds(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{Workspace: tmpDir, Dir: tmpDir}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	freshProf, _ := GetBuiltinProfile(string(ProfileFreshVerification))
	c.SetExecutionProfile(freshProf)

	sd := &SessionData{
		Rounds: 10,
	}

	c.SetSessionData(sd)
	if total := c.totalRounds(); total != 0 {
		t.Errorf("totalRounds under fresh profile = %d, want 0 (must not inherit historical rounds)", total)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.SetExecutionProfile(strictProf)
	c.SetSessionData(sd)
	if total := c.totalRounds(); total != 10 {
		t.Errorf("totalRounds under strict profile = %d, want 10 (should inherit rounds)", total)
	}
}

func TestProfile_StrictVsFreshDifferences(t *testing.T) {
	strict, ok1 := GetBuiltinProfile(string(ProfileStrictVerification))
	fresh, ok2 := GetBuiltinProfile(string(ProfileFreshVerification))
	if !ok1 || !ok2 {
		t.Fatal("failed to load builtin profiles")
	}

	if strict.DisableHistoricalTaskReuse == fresh.DisableHistoricalTaskReuse {
		t.Errorf("expected DisableHistoricalTaskReuse difference between strict (%v) and fresh (%v)", strict.DisableHistoricalTaskReuse, fresh.DisableHistoricalTaskReuse)
	}
	if strict.DisableHistoricalMemory == fresh.DisableHistoricalMemory {
		t.Errorf("expected DisableHistoricalMemory difference between strict (%v) and fresh (%v)", strict.DisableHistoricalMemory, fresh.DisableHistoricalMemory)
	}
	if strict.DefaultCachePolicy == fresh.DefaultCachePolicy {
		t.Errorf("expected DefaultCachePolicy difference between strict (%v) and fresh (%v)", strict.DefaultCachePolicy, fresh.DefaultCachePolicy)
	}
}

func TestFreshProfile_ClearsPersistedConversationHistory(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{Workspace: tmpDir, Dir: tmpDir}

	history := []fantasy.Message{
		fantasy.NewUserMessage("prior prompt from previous run"),
	}
	if err := SaveConversationHistory(tmpDir, history); err != nil {
		t.Fatalf("SaveConversationHistory failed: %v", err)
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	if len(c.conversationHistory) == 0 {
		t.Fatal("expected conversationHistory to be pre-populated by NewCoordinator before applying fresh profile")
	}

	freshProf, _ := GetBuiltinProfile(string(ProfileFreshVerification))
	c.SetExecutionProfile(freshProf)

	if len(c.conversationHistory) != 0 {
		t.Errorf("expected conversationHistory to be empty after applying fresh profile, got %d messages", len(c.conversationHistory))
	}
}
