package team

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/executioncompat"
)

// TestSavedLegacyFixturesReplayCanonicalAfterMaterialization pins the inspector and append-only
// materializer against checked-in, producer-described legacy workspaces. The
// test copies each fixture before applying it: source bytes in testdata are
// evidence and must never be rewritten by a test or migration path.
func TestSavedLegacyFixturesReplayCanonicalAfterMaterialization(t *testing.T) {
	root := filepath.Join("testdata", "execution-compat")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read fixture root: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		t.Fatal("no execution compatibility fixtures")
	}
	requiredFixtures := []string{
		"canonical-current",
		"session-hufu-local",
		"session-named-provider",
		"session-provider-binding",
		"provider-session-bound",
		"session-typed-local",
		"qualified-model-ambiguous",
		"profile-binding-conflict",
		"invalid-typed-target",
		"receipt-backend-conflict",
		"policy-v3-route",
		"branch-fork-lineage",
		"session-only-task",
		"corrupt-hash-chain",
		"partial-materialization",
		"unknown-unrelated-event",
	}
	for _, name := range requiredFixtures {
		if !slices.Contains(names, name) {
			t.Fatalf("required execution compatibility fixture %q is missing", name)
		}
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fixture := filepath.Join(root, name)
			workspace := filepath.Join(t.TempDir(), "workspace")
			copyFixtureWorkspace(t, filepath.Join(fixture, "workspace"), workspace)
			beforeInspect := snapshotFixtureWorkspace(t, workspace)
			errorPath := filepath.Join(fixture, "expected-error.json")
			if expectedError, exists := readCompatibilityFixtureError(t, errorPath); exists {
				if _, err := InspectExecutionCompatibility(t.Context(), workspace, ""); err == nil {
					t.Fatal("hard-error fixture inspection succeeded")
				} else if !strings.Contains(err.Error(), expectedError.Contains) {
					t.Fatalf("inspection error %q does not contain expected fragment %q", err, expectedError.Contains)
				}
				assertFixtureWorkspaceSnapshot(t, beforeInspect, snapshotFixtureWorkspace(t, workspace), "hard-error inspection")
				if _, err := os.Stat(filepath.Join(fixture, "expected-report.json")); err == nil {
					t.Fatal("hard-error fixture also has expected-report.json")
				} else if !os.IsNotExist(err) {
					t.Fatalf("stat expected report: %v", err)
				}
				return
			}

			report, err := InspectExecutionCompatibility(t.Context(), workspace, "")
			if err != nil {
				t.Fatalf("inspect fixture: %v", err)
			}
			assertCompatibilityFixtureReport(t, filepath.Join(fixture, "expected-report.json"), report)
			assertFixtureWorkspaceSnapshot(t, beforeInspect, snapshotFixtureWorkspace(t, workspace), "successful inspection")
			assertCompatibilityFixtureApplyOutcome(t, workspace, report)
		})
	}
}

func copyFixtureWorkspace(t *testing.T, source, destination string) {
	t.Helper()
	if err := fs.WalkDir(os.DirFS(source), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(destination, path)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(filepath.Join(source, path))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	}); err != nil {
		t.Fatalf("copy fixture workspace: %v", err)
	}
}

type compatibilityFixtureError struct {
	Contains string `json:"contains"`
}

func readCompatibilityFixtureError(t *testing.T, path string) (compatibilityFixtureError, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return compatibilityFixtureError{}, false
	}
	if err != nil {
		t.Fatalf("read expected fixture error: %v", err)
	}
	var expected compatibilityFixtureError
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatalf("decode expected fixture error: %v", err)
	}
	if strings.TrimSpace(expected.Contains) == "" {
		t.Fatal("expected fixture error contains is empty")
	}
	return expected, true
}

func assertCompatibilityFixtureApplyOutcome(t *testing.T, workspace string, report *executioncompat.InspectionReport) {
	t.Helper()
	beforeApply := snapshotFixtureWorkspace(t, workspace)
	actionable := report.AmbiguousTasks != 0 || report.UnmigratableTasks != 0 || report.AmbiguousPolicySnapshots != 0 || report.UnmigratablePolicySnapshots != 0
	if actionable {
		if _, err := ApplyExecutionCompatibility(t.Context(), workspace, ""); err == nil {
			t.Fatal("ambiguous or unmigratable fixture was materialized")
		}
		assertFixtureWorkspaceSnapshot(t, beforeApply, snapshotFixtureWorkspace(t, workspace), "failed apply")
		return
	}

	result, err := ApplyExecutionCompatibility(t.Context(), workspace, "")
	if err != nil {
		t.Fatalf("apply fixture: %v", err)
	}
	migratable := report.MigratableTasks != 0 || report.MigratablePolicySnapshots != 0
	if !migratable {
		if result.TaskMigrationEvents != 0 || result.PolicyMigrationEvents != 0 || result.ProjectionRebuilt {
			t.Fatalf("non-migratable fixture changed by apply: %#v", result)
		}
		if err := replayTodoListFromFixtureWorkspace(workspace); err != nil {
			t.Fatalf("canonical replay fixture: %v", err)
		}
		return
	}
	if result.TaskMigrationEvents == 0 && result.PolicyMigrationEvents == 0 {
		t.Fatalf("migratable fixture appended no migration: %#v", result)
	}
	post, err := InspectExecutionCompatibility(t.Context(), workspace, "")
	if err != nil {
		t.Fatalf("inspect materialized fixture: %v", err)
	}
	if post.MigratableTasks != 0 || post.AmbiguousTasks != 0 || post.UnmigratableTasks != 0 || post.MigratablePolicySnapshots != 0 || post.AmbiguousPolicySnapshots != 0 || post.UnmigratablePolicySnapshots != 0 {
		t.Fatalf("materialized fixture remains actionable: %#v", post)
	}
	if err := replayTodoListFromFixtureWorkspace(workspace); err != nil {
		t.Fatalf("canonical replay fixture: %v", err)
	}
}

type fixtureWorkspaceEntry struct {
	Data    []byte
	Mode    fs.FileMode
	Size    int64
	ModTime time.Time
}

func snapshotFixtureWorkspace(t *testing.T, root string) map[string]fixtureWorkspaceEntry {
	t.Helper()
	snapshot := make(map[string]fixtureWorkspaceEntry)
	if err := fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fixtureWorkspaceEntry{Mode: info.Mode(), Size: info.Size(), ModTime: info.ModTime()}
		if !entry.IsDir() {
			value.Data, err = os.ReadFile(filepath.Join(root, path))
			if err != nil {
				return err
			}
		}
		snapshot[path] = value
		return nil
	}); err != nil {
		t.Fatalf("snapshot fixture workspace: %v", err)
	}
	return snapshot
}

func assertFixtureWorkspaceSnapshot(t *testing.T, want, got map[string]fixtureWorkspaceEntry, phase string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s changed fixture file count: got %d want %d", phase, len(got), len(want))
	}
	for path, wantEntry := range want {
		gotEntry, exists := got[path]
		if !exists {
			t.Fatalf("%s removed fixture path %q", phase, path)
		}
		if wantEntry.Mode != gotEntry.Mode || wantEntry.Size != gotEntry.Size || !wantEntry.ModTime.Equal(gotEntry.ModTime) || !bytes.Equal(wantEntry.Data, gotEntry.Data) {
			t.Fatalf("%s changed fixture path %q", phase, path)
		}
	}
}

func assertCompatibilityFixtureReport(t *testing.T, path string, report any) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expected report: %v", err)
	}
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal actual report: %v", err)
	}
	var expectedValue, actualValue any
	if err := json.Unmarshal(want, &expectedValue); err != nil {
		t.Fatalf("decode expected report: %v", err)
	}
	if err := json.Unmarshal(got, &actualValue); err != nil {
		t.Fatalf("decode actual report: %v", err)
	}
	expectedJSON, _ := json.Marshal(expectedValue)
	actualJSON, _ := json.Marshal(actualValue)
	if !bytes.Equal(expectedJSON, actualJSON) {
		t.Fatalf("fixture report mismatch for %s\n--- got ---\n%s", path, got)
	}
}

// replayTodoListFromFixtureWorkspace verifies canonical replay independently
// for every visible branch. A fork can correctly contain one migration per
// branch for an inherited task, which must not be reduced as one global task
// stream. Policy and unknown-event fixtures legitimately replay to an empty
// task projection.
func replayTodoListFromFixtureWorkspace(workspace string) error {
	store, err := OpenEventStore(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return err
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return err
	}
	branchIDs := make([]string, 0, len(tree.Branches))
	for branchID := range tree.Branches {
		branchIDs = append(branchIDs, branchID)
	}
	slices.Sort(branchIDs)
	for _, branchID := range branchIDs {
		lineage, err := ProjectValidatedEventsForBranch(events, tree, branchID)
		if err != nil {
			return err
		}
		if _, err := ReplayTodoList(lineage); err != nil {
			return err
		}
	}
	return nil
}
