package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// missingRequiredResourceConfig declares one required resource that does not
// exist under projectDir, so every call site wired to
// ValidateRequiredResourceLocks must fail admission before reaching any
// provider/model call.
func missingRequiredResourceConfig(name string) agent.TeamConfig {
	return agent.TeamConfig{
		Name: name,
		RequiredResources: []agent.RequiredResourceSpec{
			{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true},
		},
	}
}

func TestRunFailsAdmissionBeforeProviderStartWhenRequiredResourceMissing(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		session:         &TeamSession{Workspace: workspace, Config: missingRequiredResourceConfig("run-admission")},
		sessionData:     NewSession(),
		taskTracker:     NewTaskTracker(),
		projectDir:      t.TempDir(),
	}

	result, err := c.Run(context.Background(), "must not execute")
	if err == nil || result != "" {
		t.Fatalf("Run() result=%q err=%v, want a required-resource-lock error and empty result", result, err)
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the missing resource", err)
	}
	if got := c.LastRunResult(); got == nil || got.Outcome != RunOutcomeFailed {
		t.Fatalf("LastRunResult() = %#v, want failed", got)
	}
}

func TestRunDirectAgentRespectsRequiredResourceLock(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: missingRequiredResourceConfig("direct-admission")},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
		projectDir:  t.TempDir(),
	}

	_, err := c.RunDirectAgent(context.Background(), "helper", "must not execute")
	if err == nil {
		t.Fatal("RunDirectAgent() = nil error, want a required-resource-lock failure")
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the missing resource", err)
	}
}

// TestContinueWithPromptEnforcesRequiredResourceLock proves the previously
// uncovered gap is closed: ContinueWithPrompt never called ValidateResourceLocks
// either, so before this wiring it would have silently continued despite a
// missing required resource.
func TestContinueWithPromptEnforcesRequiredResourceLock(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		session:         &TeamSession{Workspace: workspace, Config: missingRequiredResourceConfig("continue-admission")},
		sessionData:     NewSession(),
		taskTracker:     NewTaskTracker(),
		projectDir:      t.TempDir(),
	}

	_, err := c.ContinueWithPrompt(context.Background(), "must not execute")
	if err == nil {
		t.Fatal("ContinueWithPrompt() = nil error, want a required-resource-lock failure")
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the missing resource", err)
	}
}

func TestTargetedRecoveryRespectsRequiredResourceLock(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: missingRequiredResourceConfig("recovery-admission")},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
		projectDir:  t.TempDir(),
	}

	_, err := c.ReconcileTask(context.Background(), "some-task")
	if err == nil {
		t.Fatal("ReconcileTask() = nil error, want a required-resource-lock failure")
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the missing resource", err)
	}
}

// TestExecuteTasksRevalidatesLockOnEachDispatchBatch proves the lock is
// re-checked on every dispatch batch, not just once at Run start: the first
// call locks a resource that exists, then the file is mutated, and the
// second call on the same coordinator must fail with drift detected instead
// of silently accepting the changed content.
func TestExecuteTasksRevalidatesLockOnEachDispatchBatch(t *testing.T) {
	workspace := t.TempDir()
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("original content"), 0o644); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: missingRequiredResourceConfig("dispatch-admission")},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
		projectDir:  projectDir,
		eventStore:  es,
	}

	if _, err := c.ExecuteTasks(context.Background(), nil); err != nil {
		t.Fatalf("first ExecuteTasks() error = %v, want the lock to succeed", err)
	}

	if err := os.WriteFile(filepath.Join(projectDir, "AGENTS.md"), []byte("mutated content"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = c.ExecuteTasks(context.Background(), nil)
	if err == nil {
		t.Fatal("second ExecuteTasks() = nil error, want drift-detected failure")
	}
	if !strings.Contains(err.Error(), "drift detected") {
		t.Fatalf("error = %v, want it to mention drift detected", err)
	}
}

// TestLegacyTeamWithoutRequiredResourcesUnaffectedAtEveryCallSite is runtime
// invariant 10 exercised at the production wiring level, not just in
// isolated unit tests: a team that never declares required-resources must
// see zero behavior change from ValidateRequiredResourceLocks being called
// at every admission boundary.
func TestLegacyTeamWithoutRequiredResourcesUnaffectedAtEveryCallSite(t *testing.T) {
	legacyConfig := agent.TeamConfig{Name: "legacy-team"}

	t.Run("run direct agent", func(t *testing.T) {
		workspace := t.TempDir()
		c := &Coordinator{
			session:     &TeamSession{Workspace: workspace, Config: legacyConfig},
			sessionData: NewSession(),
			taskTracker: NewTaskTracker(),
			projectDir:  t.TempDir(),
		}
		_, err := c.RunDirectAgent(context.Background(), "helper", "goal")
		if err != nil && strings.Contains(err.Error(), "required resource") {
			t.Fatalf("legacy team unexpectedly hit a required-resource-lock failure: %v", err)
		}
	})

	t.Run("execute tasks", func(t *testing.T) {
		workspace := t.TempDir()
		c := &Coordinator{
			session:     &TeamSession{Workspace: workspace, Config: legacyConfig},
			sessionData: NewSession(),
			taskTracker: NewTaskTracker(),
			projectDir:  t.TempDir(),
		}
		_, err := c.ExecuteTasks(context.Background(), nil)
		if err != nil && strings.Contains(err.Error(), "required resource") {
			t.Fatalf("legacy team unexpectedly hit a required-resource-lock failure: %v", err)
		}
	})

	t.Run("targeted recovery", func(t *testing.T) {
		workspace := t.TempDir()
		c := &Coordinator{
			session:     &TeamSession{Workspace: workspace, Config: legacyConfig},
			sessionData: NewSession(),
			taskTracker: NewTaskTracker(),
			projectDir:  t.TempDir(),
		}
		_, err := c.ReconcileTask(context.Background(), "some-task")
		if err != nil && strings.Contains(err.Error(), "required resource") {
			t.Fatalf("legacy team unexpectedly hit a required-resource-lock failure: %v", err)
		}
	})
}
