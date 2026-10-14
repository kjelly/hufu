package team

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// legacyFixture is a file-based workspace whose files hold what the retired
// writers produced, built from the current APIs' output.
type legacyFixture struct {
	workspace string
	files     map[string][]byte
}

func newLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	source := t.TempDir()
	events, err := NewEventStore(source, "run-legacy", "session-legacy")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{
		{Type: "run_started", Actor: "user", IdempotencyKey: "run:started"},
		{Type: "task_progress", Actor: "worker", TaskID: "t1", Attempt: 1, Payload: []byte(`{"progress":"first"}`)},
		{Type: "task_progress", Actor: "worker", BranchID: "exp", Payload: []byte(`{"progress":"on a branch"}`)},
		{Type: "run_finished", Actor: "coordinator", Payload: []byte(`{"outcome":"success"}`)},
	} {
		if err := events.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	var eventLog bytes.Buffer
	if err := ExportEventLog(context.Background(), source, &eventLog); err != nil {
		t.Fatal(err)
	}
	treeJSON := []byte(`{"active_branch":"exp","branches":{"main":{"id":"main","name":"main","created_at":"2026-10-01T00:00:00Z","state":{}},` +
		`"exp":{"id":"exp","name":"exp","parent_id":"main","created_at":"2026-10-02T00:00:00Z","state":{"active_model":"model-exp"}}},"labels":{"release":"exp"}}`)
	files := map[string][]byte{
		legacyEventLog:                                   eventLog.Bytes(),
		legacySessionTree:                                treeJSON,
		"session.json":                                   []byte(`{"rounds":4,"tasks":[{"id":"t1","status":"done"}]}`),
		"conversation_compaction_state.json":             []byte(`{"version":1,"branches":{}}`),
		"session_history.json":                           []byte(`[{"role":"user","content":[{"type":"text","data":{"text":"hi"}}]}]`),
		filepath.Join(logsDir, "terminal_sessions.json"): []byte(`[]`),
		filepath.Join(logsDir, "reliability_eval.json"):  []byte(`{"version":1}`),
		legacyTaskJournal:                                []byte("{\"op\":\"put\",\"agent\":\"worker\",\"desc\":\"a\",\"ts\":\"2026-10-01T00:00:00Z\"}\n{\"op\":\"del\",\"agent\":\"worker\",\"desc\":\"a\",\"ts\":\"2026-10-01T00:00:01Z\"}\n"),
		legacyDecisionIndex:                              []byte("{\"decision_id\":\"dec-1\",\"schema_version\":1}\n"),
	}
	workspace := t.TempDir()
	for name, data := range files {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A projection that stays a file must be left alone.
	if err := os.WriteFile(filepath.Join(workspace, "chat_history.md"), []byte("# chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return legacyFixture{workspace: workspace, files: files}
}

// treeSnapshot captures every file under root with its bytes.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			snapshot[rel+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(path)
		snapshot[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestConvertFileBasedWorkspaceKeepsEveryRecord(t *testing.T) {
	fixture := newLegacyFixture(t)
	ws := fixture.workspace
	result, err := ConvertFileBasedWorkspace(context.Background(), ws, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events != 4 || result.SessionBranches != 2 || len(result.Documents) != 5 || result.TaskJournalRows != 2 || result.DecisionIndexRows != 1 || len(result.MovedFiles) != len(fixture.files) {
		t.Fatalf("conversion = %+v", result)
	}

	// The journal re-exports to the original log, and keeps growing on it.
	if exported := exportedJournal(t, ws); !bytes.Equal(exported, fixture.files[legacyEventLog]) {
		t.Fatalf("event log changed in conversion:\n%s\nwant\n%s", exported, fixture.files[legacyEventLog])
	}
	events, err := OpenEventStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	next, err := events.AppendPersisted(RunEvent{Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"after conversion"}`)})
	_ = events.Close()
	if err != nil || next.PreviousHash == "" {
		t.Fatalf("append after conversion = %+v, %v", next, err)
	}
	for _, document := range legacyRuntimeDocuments {
		if body, _ := getRuntimeDocument(t, ws, document.document); !bytes.Equal(body, fixture.files[document.path]) {
			t.Fatalf("document %s = %q, want %q", document.document, body, fixture.files[document.path])
		}
	}
	tree, err := LoadSessionTree(ws)
	if err != nil {
		t.Fatal(err)
	}
	exp := tree.Branches["exp"]
	if tree.ActiveBranch != "exp" || len(tree.Branches) != 2 || exp == nil || exp.ParentID != "main" || exp.State.ActiveModel != "model-exp" || tree.Labels["release"] != "exp" {
		t.Fatalf("session tree = %+v", tree)
	}
	if journal, err := readTaskJournalLog(ws); err != nil || !bytes.Equal(journal, fixture.files[legacyTaskJournal]) {
		t.Fatalf("task journal = %q, %v", journal, err)
	}
	if rows, err := readDecisionIndexRows(ws); err != nil || len(rows) != 1 || !strings.Contains(string(rows[0]), "dec-1") {
		t.Fatalf("decision index rows = %q, %v", rows, err)
	}

	// The originals are kept, byte for byte, out of the runtime's way.
	for name, data := range fixture.files {
		if _, err := os.Lstat(filepath.Join(ws, name)); !os.IsNotExist(err) {
			t.Fatalf("%s is still in the workspace", name)
		}
		if kept, err := os.ReadFile(filepath.Join(ws, LegacyRuntimeBackupDir, name)); err != nil || !bytes.Equal(kept, data) {
			t.Fatalf("backup of %s = %q, %v", name, kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "chat_history.md")); err != nil {
		t.Fatalf("a projection file was moved: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(ws, ".runtime-conversion-*")); len(matches) != 0 {
		t.Fatalf("staging left behind: %v", matches)
	}
	if _, err := ConvertFileBasedWorkspace(context.Background(), ws, false); !errors.Is(err, ErrNotFileBased) {
		t.Fatalf("second conversion = %v, want ErrNotFileBased", err)
	}
}

// TestConvertFileBasedWorkspaceChangesNothingUnlessItSucceeds covers a dry
// run and every refusal: the workspace keeps its exact files.
func TestConvertFileBasedWorkspaceChangesNothingUnlessItSucceeds(t *testing.T) {
	tests := []struct {
		name    string
		dryRun  bool
		corrupt func(t *testing.T, ws string)
		wantErr error
	}{
		{name: "dry run", dryRun: true},
		{name: "broken event chain", corrupt: func(t *testing.T, ws string) {
			path := filepath.Join(ws, legacyEventLog)
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, bytes.Replace(data, []byte(`"first"`), []byte(`"forged"`), 1), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: ErrEventLogInvalid},
		{name: "unreadable session tree", corrupt: func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, legacySessionTree), []byte(`{not json`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws := newLegacyFixture(t).workspace
			if tt.corrupt != nil {
				tt.corrupt(t, ws)
			}
			before := treeSnapshot(t, ws)
			result, err := ConvertFileBasedWorkspace(context.Background(), ws, tt.dryRun)
			if tt.dryRun {
				if err != nil || result.Events != 4 || len(result.MovedFiles) == 0 {
					t.Fatalf("dry run = %+v, %v", result, err)
				}
			} else if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
				t.Fatalf("conversion error = %v, want %v", err, tt.wantErr)
			}
			if after := treeSnapshot(t, ws); !reflect.DeepEqual(after, before) {
				t.Fatalf("workspace changed:\nbefore %v\nafter  %v", keysOf(before), keysOf(after))
			}
		})
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestConvertFileBasedWorkspaceFinishesAnInterruptedConversion(t *testing.T) {
	ws := t.TempDir()
	store, err := runtimestore.Open(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// runtime.sqlite committed, the original left in place.
	if err := os.WriteFile(filepath.Join(ws, "session.json"), []byte(`{"rounds":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ConvertFileBasedWorkspace(context.Background(), ws, false)
	if err != nil || !result.Resumed {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(ws, LegacyRuntimeBackupDir, "session.json")); err != nil {
		t.Fatalf("original was not moved to the backup: %v", err)
	}
}

func TestConvertFileBasedWorkspaceRefusesWorkspacesWithoutFiles(t *testing.T) {
	if _, err := ConvertFileBasedWorkspace(context.Background(), t.TempDir(), false); !errors.Is(err, ErrNotFileBased) {
		t.Fatalf("conversion of an empty workspace = %v, want ErrNotFileBased", err)
	}
}
