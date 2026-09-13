package team

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/audit"
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
	t.Cleanup(func() { _ = c.Close() })

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

func TestCoordinatorCloseIsConcurrentIdempotentAndReleasesOwnedResources(t *testing.T) {
	workspace := t.TempDir()
	c, err := newCoordinator(coordinatorParams{Session: &TeamSession{
		Workspace: workspace,
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "close-test", GoalMode: "exploratory"},
	}}, RuntimeServices{})
	if err != nil {
		t.Fatalf("newCoordinator failed: %v", err)
	}
	c.initTaskJournal()
	c.initEventStore()

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { errs <- c.Close() })
	}
	wg.Wait()
	close(errs)
	for closeErr := range errs {
		if closeErr != nil {
			t.Fatalf("Close returned an error: %v", closeErr)
		}
	}
	if got := audit.GetDefault(); got == c.auditLogger {
		t.Fatal("Close left the coordinator audit logger installed as the process default")
	}
	if open := openFileDescriptorsUnder(t, workspace); len(open) != 0 {
		t.Fatalf("Close leaked workspace resources: %v", open)
	}
}

func TestCoordinatorCloseReleasesActiveContextPreflight(t *testing.T) {
	workspace := t.TempDir()
	c, err := newCoordinator(coordinatorParams{Session: &TeamSession{
		Workspace: workspace,
		Dir:       t.TempDir(),
		Config:    agent.TeamConfig{Name: "close-preflight", GoalMode: "exploratory"},
	}}, RuntimeServices{})
	if err != nil {
		t.Fatalf("newCoordinator failed: %v", err)
	}
	if err := c.PrepareContextPreflightContext(t.Context()); err != nil {
		t.Fatalf("PrepareContextPreflightContext failed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if c.invocationLeaseHeld {
		t.Fatal("Close left the context preflight invocation lease held")
	}
	if c.eventStore != nil {
		t.Fatal("Close left the context preflight event store attached")
	}
	if open := openFileDescriptorsUnder(t, workspace); len(open) != 0 {
		t.Fatalf("Close leaked preflight resources: %v", open)
	}
}

func TestCoordinatorCloseDoesNotReleaseAnotherCoordinatorResources(t *testing.T) {
	newTestCoordinator := func(name string) *Coordinator {
		t.Helper()
		c, err := newCoordinator(coordinatorParams{Session: &TeamSession{
			Workspace: t.TempDir(),
			Dir:       t.TempDir(),
			Config:    agent.TeamConfig{Name: name, GoalMode: "exploratory"},
		}}, RuntimeServices{})
		if err != nil {
			t.Fatalf("newCoordinator(%s) failed: %v", name, err)
		}
		return c
	}
	first := newTestCoordinator("close-owner-first")
	second := newTestCoordinator("close-owner-second")
	if got := audit.GetDefault(); got != second.auditLogger {
		t.Fatal("second coordinator audit logger is not the process default")
	}

	clone := &Coordinator{contextRepo: second.contextRepo, auditLogger: second.auditLogger}
	if err := clone.Close(); err != nil {
		t.Fatalf("non-owning clone Close failed: %v", err)
	}
	if _, err := second.contextRepo.Revision(t.Context()); err != nil {
		t.Fatalf("non-owning clone closed the parent context repository: %v", err)
	}
	if got := audit.GetDefault(); got != second.auditLogger {
		t.Fatal("non-owning clone detached the parent audit logger")
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}
	if got := audit.GetDefault(); got != second.auditLogger {
		t.Fatal("closing an older coordinator detached the newer default audit logger")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if got := audit.GetDefault(); got != nil {
		t.Fatal("closing the active coordinator left a default audit logger installed")
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
