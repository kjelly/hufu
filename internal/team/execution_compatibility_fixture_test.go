package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
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
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no execution compatibility fixtures")
	}

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			fixture := filepath.Join(root, name)
			workspace := filepath.Join(t.TempDir(), "workspace")
			copyFixtureWorkspace(t, filepath.Join(fixture, "workspace"), workspace)
			fixtureSession := filepath.Join(fixture, "workspace", sessionFile)
			sourceBytes, err := os.ReadFile(fixtureSession)
			if err != nil {
				t.Fatalf("read fixture session: %v", err)
			}
			workspaceSession := filepath.Join(workspace, sessionFile)
			before, err := os.ReadFile(workspaceSession)
			if err != nil {
				t.Fatalf("read copied fixture session: %v", err)
			}
			if !bytes.Equal(sourceBytes, before) {
				t.Fatal("fixture workspace copy does not preserve source bytes")
			}

			report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
			if err != nil {
				t.Fatalf("inspect fixture: %v", err)
			}
			assertCompatibilityFixtureReport(t, filepath.Join(fixture, "expected-report.json"), report)
			afterInspect, err := os.ReadFile(workspaceSession)
			if err != nil {
				t.Fatalf("re-read fixture session: %v", err)
			}
			if !bytes.Equal(before, afterInspect) {
				t.Fatal("inspector rewrote saved fixture bytes")
			}

			if _, err := ApplyExecutionCompatibility(context.Background(), workspace, ""); err != nil {
				t.Fatalf("apply fixture: %v", err)
			}
			post, err := InspectExecutionCompatibility(context.Background(), workspace, "")
			if err != nil {
				t.Fatalf("inspect materialized fixture: %v", err)
			}
			if post.MigratableTasks != 0 || post.AmbiguousTasks != 0 || post.UnmigratableTasks != 0 || post.MigratablePolicySnapshots != 0 || post.AmbiguousPolicySnapshots != 0 || post.UnmigratablePolicySnapshots != 0 {
				t.Fatalf("materialized fixture remains actionable: %#v", post)
			}
			if _, err := replayTodoListFromFixtureWorkspace(workspace); err != nil {
				t.Fatalf("canonical replay fixture: %v", err)
			}
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
		return os.WriteFile(target, data, 0o600)
	}); err != nil {
		t.Fatalf("copy fixture workspace: %v", err)
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

// replayTodoListFromFixtureWorkspace verifies that the event store remains readable
// after apply without exposing an EventStore writer to fixture assertions.
func replayTodoListFromFixtureWorkspace(workspace string) ([]*TodoItem, error) {
	store, err := OpenEventStore(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 && len(events) != 0 {
		return nil, fmt.Errorf("canonical replay produced no tasks")
	}
	return tasks, nil
}
