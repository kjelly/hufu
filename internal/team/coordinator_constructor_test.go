package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestNewCoordinatorCompositionRootPartialInjection(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{
		Workspace: workspace,
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "composition-test", GoalMode: "exploratory"},
	}
	injectedEvidence := &mockEvidenceService{}

	c, err := newCoordinator(coordinatorParams{Session: session, MaxConcurrent: 2}, RuntimeServices{
		Evidence: injectedEvidence,
	})
	if err != nil {
		t.Fatalf("newCoordinator failed: %v", err)
	}
	t.Cleanup(func() {
		if c.auditLogger != nil {
			_ = c.auditLogger.Close()
		}
		if c.contextRepo != nil {
			_ = c.contextRepo.Close()
		}
	})

	if c.EvidenceService() != EvidenceService(injectedEvidence) {
		t.Fatal("partial injection did not install the requested EvidenceService")
	}
	services := c.RuntimeServices()
	if services.Planner == nil || services.SessionStore == nil || services.PolicyEngine == nil ||
		services.ContextCompiler == nil || services.AgentPool == nil || services.WorkflowEngine == nil ||
		services.EventJournal == nil || services.ToolResolver == nil || services.ModelRuntime == nil ||
		services.SubagentRegistry == nil || services.ExecutionRegistry == nil ||
		services.ExperienceProcessor == nil || services.TaskCache == nil || services.RepairController == nil {
		t.Fatalf("partial injection left a default service unset: %#v", services)
	}
	if len(c.coreTools) == 0 {
		t.Fatal("dependent tools were not initialized after service composition")
	}
}

func TestNewCoordinatorRejectsNilSession(t *testing.T) {
	if _, err := newCoordinator(coordinatorParams{}, RuntimeServices{}); err == nil {
		t.Fatal("newCoordinator accepted a nil session")
	}
}

func TestNewCoordinatorErrorClosesOwnedResources(t *testing.T) {
	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skip("open file descriptor inspection is unavailable")
	}
	workspace := t.TempDir()
	logDir := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, terminalSessionsFile), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := &TeamSession{
		Workspace: workspace,
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "constructor-error", GoalMode: "exploratory"},
	}

	if _, err := newCoordinator(coordinatorParams{Session: session}, RuntimeServices{}); err == nil {
		t.Fatal("newCoordinator succeeded with corrupt terminal session state")
	}
	if open := openFileDescriptorsUnder(t, workspace); len(open) != 0 {
		t.Fatalf("constructor error leaked workspace resources: %v", open)
	}
}

func openFileDescriptorsUnder(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var open []string
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, root+string(filepath.Separator)) {
			open = append(open, target)
		}
	}
	return open
}
