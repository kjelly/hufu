package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func cleanupWorkspaceDistillationFixture(t *testing.T, closer io.Closer) {
	t.Helper()
	t.Cleanup(func() { _ = closer.Close() })
}

func TestWorkspaceDistillationPreviewThenApplyPreservesKnowledgeBeforeCleanup(t *testing.T) {
	for _, mode := range []string{"apply", "changed source", "storage failure", "call budget"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newWorkspaceCLIFixture(t)
			fixture.run(t, "register")
			fixture.deps.registryOptions = []workspacepkg.RegistryOption{workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28))))}
			registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot, fixture.deps.registryOptions...)
			if err != nil {
				t.Fatal(err)
			}
			cleanupWorkspaceDistillationFixture(t, registry)
			old, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
			if err != nil {
				t.Fatal(err)
			}
			_ = registry.Close()
			oldSession := team.NewSession()
			oldSession.Tasks = []*team.TodoItem{{ID: "old", Agent: "worker", Desc: "earlier duplicate work", Status: team.TaskDone, Output: "A useful general procedure."}}
			if mode == "call budget" {
				oldSession.Tasks[0].Output = strings.Repeat("evidence ", taskdistill.MaxInputBytes/4)
			}
			if err = team.SaveSession(old.ControlRoot, oldSession); err != nil {
				t.Fatal(err)
			}
			manager, err := workspacepkg.NewManager(fixture.stateRoot, fixture.deps.registryOptions...)
			if err != nil {
				t.Fatal(err)
			}
			deleted, err := manager.Delete(t.Context(), workspacepkg.DeleteRequest{StartDir: fixture.projectRoot, TeamName: "default"})
			if err != nil {
				t.Fatal(err)
			}
			trash := deleted.Items[0]
			registry, err = workspacepkg.OpenReadWrite(fixture.stateRoot, fixture.deps.registryOptions...)
			if err != nil {
				t.Fatal(err)
			}
			cleanupWorkspaceDistillationFixture(t, registry)
			destination, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
			if err != nil {
				t.Fatal(err)
			}
			_ = registry.Close()
			session := team.NewSession()
			session.Tasks = []*team.TodoItem{{ID: "1", Agent: "worker", Desc: "duplicate useful work", Status: team.TaskDone, Output: "The same useful procedure."}}
			if err = team.SaveSession(destination.ControlRoot, session); err != nil {
				t.Fatal(err)
			}
			search := t.TempDir()
			helperSetupTeam(t, search, "default")
			calls := 0
			original := taskDistillationGeneratorFactory
			taskDistillationGeneratorFactory = func(_ context.Context, _ string, draftWorkspace string, _ string) (maintenanceTextGenerator, string, func(), error) {
				if draftWorkspace == destination.ControlRoot {
					t.Fatal("model admission can overwrite the source session")
				}
				// Real preflight persists policy/manifests in its own session.
				if err := team.SaveSession(draftWorkspace, team.NewSession()); err != nil {
					t.Fatal(err)
				}
				calls++
				draft := taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "1", Action: "distill", Reason: "useful"}, {SourceID: trash.TrashID, Action: "distill", Reason: "duplicate"}}, Knowledge: []taskdistill.Knowledge{{Text: "Preserve the useful general procedure and its applicability.", SourceIDs: []string{"1", trash.TrashID}}}}
				raw, _ := json.Marshal(draft)
				return staticTextGenerator{reply: string(raw)}, "test-model", func() {}, nil
			}
			t.Cleanup(func() { taskDistillationGeneratorFactory = original })
			if mode == "call budget" {
				_, err = executeWorkspaceCommand(t, fixture.deps, "distill", "--team", "default", "--trash", trash.TrashID, "--max-model-calls", "1", "--team-search-path", search)
				if err == nil || calls != 0 || !pathExistsForCLI(trash.TrashPath) {
					t.Fatalf("budget preflight admitted a provider or changed the source: calls=%d err=%v", calls, err)
				}
				return
			}
			output := fixture.run(t, "distill", "--team", "default", "--task", "1", "--trash", trash.TrashID, "--team-search-path", search)
			var plan workspaceDistillationPlan
			if err = json.Unmarshal([]byte(output), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.ID == "" || calls != 1 || len(team.LoadSession(destination.ControlRoot).Tasks) != 1 || !pathExistsForCLI(trash.TrashPath) {
				t.Fatal("preview changed source tasks")
			}
			checkScope := newWorkspaceCommand(fixture.deps)
			checkScope.SetContext(t.Context())
			changedScope := workspacepkg.Resolution{ProjectID: destination.ProjectID, WorkspaceID: destination.ID, TeamName: "default", ControlRoot: destination.ControlRoot, ContextScopeID: destination.ContextScopeID + "-changed"}
			if err = applyWorkspaceDistillation(checkScope, manager, changedScope, plan.ID, fixture.stateRoot); err == nil {
				t.Fatal("changed context scope accepted the saved preview")
			}
			if _, err = os.Stat(filepath.Join(destination.ControlRoot, "context.sqlite")); !os.IsNotExist(err) {
				t.Fatal("preview confirmed knowledge")
			}
			if mode == "changed source" {
				oldSession.Tasks[0].Output = "different evidence"
				if err = team.SaveSession(trash.TrashPath, oldSession); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "storage failure" {
				repo, err := contextstore.OpenSQLite(filepath.Join(destination.ControlRoot, "context.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				_ = repo.Close()
				if err = os.Remove(filepath.Join(destination.ControlRoot, "context.sqlite")); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(filepath.Join(destination.ControlRoot, "context.sqlite"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, err = executeWorkspaceCommand(t, fixture.deps, "distill", "--team", "default", "--apply", plan.ID, "--yes")
			if mode != "apply" {
				if err == nil || len(team.LoadSession(destination.ControlRoot).Tasks) != 1 || !pathExistsForCLI(trash.TrashPath) {
					t.Fatalf("failed apply mutated sources: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(team.LoadSession(destination.ControlRoot).Tasks) != 0 || pathExistsForCLI(trash.TrashPath) {
				t.Fatal("apply did not clean selected sources")
			}
			repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(destination.ControlRoot, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			items, err := repo.Query(t.Context(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: destination.ContextScopeID, TeamID: "default"}})
			_ = repo.Close()
			if err != nil || len(items) != 1 || len(items[0].Evidence) != 2 {
				t.Fatalf("knowledge=%+v err=%v", items, err)
			}
			if _, err = executeWorkspaceCommand(t, fixture.deps, "distill", "--team", "default", "--apply", plan.ID, "--yes"); err != nil {
				t.Fatalf("idempotent retry: %v", err)
			}
			if calls != 1 {
				t.Fatal("apply called the model again")
			}
		})
	}
}

func TestWorkspaceDistillationRequiresExplicitReviewedApply(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	for _, args := range [][]string{{"distill"}, {"distill", "--team", "default"}, {"distill", "--team", "default", "--apply", "plan"}, {"distill", "--team", "default", "--task", "1", "--yes"}, {"distill", "--team", "default", "--apply", "plan", "--yes", "--task", "1"}} {
		if _, err := executeWorkspaceCommand(t, fixture.deps, args...); err == nil {
			t.Fatalf("flags accepted: %s", strings.Join(args, " "))
		}
	}
}
