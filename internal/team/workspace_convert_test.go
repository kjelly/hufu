package team

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			snapshot[rel] = "symlink:" + target
			return err
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

// installInterruptedConversion simulates a crash immediately after database
// publication, using the same import, verification, and sealing as conversion.
func installInterruptedConversion(t *testing.T, fixture legacyFixture) {
	t.Helper()
	staging := t.TempDir()
	present, err := presentLegacyFiles(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	record, err := newWorkspaceConversionRecord(fixture.workspace, present)
	if err != nil {
		t.Fatal(err)
	}
	var imported WorkspaceConversion
	if err := importLegacyRuntimeState(t.Context(), fixture.workspace, staging, &imported); err != nil {
		t.Fatal(err)
	}
	if err := verifyConvertedRuntimeState(staging, imported); err != nil {
		t.Fatal(err)
	}
	if err := sealWorkspaceConversion(t.Context(), staging, record, imported); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(runtimestore.Path(staging), runtimestore.Path(fixture.workspace)); err != nil {
		t.Fatal(err)
	}
}

func archiveConversionOriginal(t *testing.T, ws, name string) {
	t.Helper()
	target := filepath.Join(ws, LegacyRuntimeBackupDir, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(ws, name), target); err != nil {
		t.Fatal(err)
	}
}

func TestConvertFileBasedWorkspaceFinishesAnInterruptedConversion(t *testing.T) {
	// Exercise every boundary, including all originals moved but the note not
	// yet written. Recovery must preserve the imported data, not just move files.
	for moved := range len(legacyRuntimeFiles()) + 1 {
		t.Run(fmt.Sprintf("after_%d_moves", moved), func(t *testing.T) {
			fixture := newLegacyFixture(t)
			ws := fixture.workspace
			installInterruptedConversion(t, fixture)
			for _, name := range legacyRuntimeFiles()[:moved] {
				archiveConversionOriginal(t, ws, name)
			}
			before := treeSnapshot(t, ws)
			preview, err := ConvertFileBasedWorkspace(t.Context(), ws, true)
			if err != nil || !preview.Resumed || len(preview.MovedFiles) != len(fixture.files)-moved || preview.Events != 4 || preview.TaskJournalRows != 2 {
				t.Fatalf("preview = %+v, %v", preview, err)
			}
			if !reflect.DeepEqual(before, treeSnapshot(t, ws)) {
				t.Fatal("resume dry run changed workspace")
			}
			result, err := ConvertFileBasedWorkspace(t.Context(), ws, false)
			if err != nil || !result.Resumed || result.Events != 4 || result.SessionBranches != 2 || len(result.Documents) != 5 || result.TaskJournalRows != 2 || result.DecisionIndexRows != 1 {
				t.Fatalf("resume = %+v, %v", result, err)
			}
			if session := LoadSession(ws); session == nil || session.Rounds != 4 || len(session.Tasks) != 1 || session.Tasks[0].Status != "done" {
				t.Fatalf("imported session = %+v", session)
			}
			if !bytes.Equal(exportedJournal(t, ws), fixture.files[legacyEventLog]) {
				t.Fatal("resumed journal differs from original")
			}
			for name, want := range fixture.files {
				got, err := os.ReadFile(filepath.Join(ws, LegacyRuntimeBackupDir, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("backup %s differs: %v", name, err)
				}
			}
			if _, err := os.Stat(filepath.Join(ws, LegacyRuntimeBackupDir, "CONVERTED")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConvertFileBasedWorkspaceRefusesUnprovenDatabase(t *testing.T) {
	for _, kind := range []string{"empty_valid", "zero_bytes", "corrupt", "directory", "symlink"} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_%t", kind, dryRun), func(t *testing.T) {
				fixture := newLegacyFixture(t)
				ws := fixture.workspace
				switch kind {
				case "empty_valid":
					// A real database predating the legacy source is not proof of
					// an import, even with the entire current runtime schema.
					stage := t.TempDir()
					store, err := runtimestore.Open(t.Context(), stage)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(runtimestore.Path(stage), runtimestore.Path(ws)); err != nil {
						t.Fatal(err)
					}
				case "zero_bytes", "corrupt":
					data := []byte{}
					if kind == "corrupt" {
						data = []byte("not SQLite")
					}
					if err := os.WriteFile(runtimestore.Path(ws), data, 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(runtimestore.Path(ws), 0o700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), runtimestore.Path(ws)); err != nil {
						t.Fatal(err)
					}
				}
				assertConversionRefusalUnchanged(t, ws, dryRun)
			})
		}
	}
}

func assertConversionRefusalUnchanged(t *testing.T, ws string, dryRun bool) {
	t.Helper()
	before := treeSnapshot(t, ws)
	if _, err := ConvertFileBasedWorkspace(t.Context(), ws, dryRun); err == nil {
		t.Fatal("unsafe conversion succeeded")
	}
	if !reflect.DeepEqual(before, treeSnapshot(t, ws)) {
		t.Fatal("refused conversion changed workspace")
	}
}

func TestConvertFileBasedWorkspacePreflightsEveryBackupPath(t *testing.T) {
	cases := append(legacyRuntimeFiles(), "CONVERTED", "logs", "logs/decisions", ".")
	for _, name := range cases {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_%t", name, dryRun), func(t *testing.T) {
				ws := newLegacyFixture(t).workspace
				target := filepath.Join(ws, LegacyRuntimeBackupDir, name)
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("existing backup"), 0o600); err != nil {
					t.Fatal(err)
				}
				assertConversionRefusalUnchanged(t, ws, dryRun)
			})
		}
	}
}

func TestConvertFileBasedWorkspaceRefusesChangedImport(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, string)
	}{
		{"changed source", func(t *testing.T, ws string) {
			if err := os.WriteFile(filepath.Join(ws, "session.json"), []byte(`{"rounds":999}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing source", func(t *testing.T, ws string) {
			if err := os.Remove(filepath.Join(ws, "session.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed backup", func(t *testing.T, ws string) {
			archiveConversionOriginal(t, ws, "session.json")
			if err := os.WriteFile(filepath.Join(ws, LegacyRuntimeBackupDir, "session.json"), []byte(`{"rounds":999}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"duplicate backup", func(t *testing.T, ws string) {
			path := filepath.Join(ws, LegacyRuntimeBackupDir, "session.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"rounds":4}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range cases {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_%t", tt.name, dryRun), func(t *testing.T) {
				fixture := newLegacyFixture(t)
				installInterruptedConversion(t, fixture)
				tt.change(t, fixture.workspace)
				assertConversionRefusalUnchanged(t, fixture.workspace, dryRun)
			})
		}
	}
	// Every imported table participates in recovery validation, even if a
	// tamper preserves row counts and leaves a syntactically valid database.
	for _, query := range []string{
		`UPDATE runtime_documents SET body = '{}' WHERE name = 'session'`,
		`DELETE FROM events`,
		`UPDATE task_journal SET record = '{}' WHERE sequence = 1`,
		`UPDATE decision_index SET entry = '{}'`,
		`UPDATE session_branches SET state = '{}'`,
		`UPDATE session_labels SET target = 'main'`,
		`UPDATE session_tree_head SET active_branch = 'main'`,
		`UPDATE runtime_documents SET body = '{}' WHERE name = 'file_storage_conversion'`,
	} {
		for _, dryRun := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/dry_%t", query, dryRun), func(t *testing.T) {
				fixture := newLegacyFixture(t)
				installInterruptedConversion(t, fixture)
				if err := withRuntimeStore(fixture.workspace, func(store *runtimestore.Store) error {
					return store.WithTx(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), query)
						return err
					})
				}); err != nil {
					t.Fatal(err)
				}
				assertConversionRefusalUnchanged(t, fixture.workspace, dryRun)
			})
		}
	}
}

func TestConvertFileBasedWorkspaceRefusesSymlinkedBackup(t *testing.T) {
	for _, name := range []string{".", "logs", "logs/decisions", "session.json", "CONVERTED"} {
		t.Run(name, func(t *testing.T) {
			ws := newLegacyFixture(t).workspace
			target := filepath.Join(ws, LegacyRuntimeBackupDir, name)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), target); err != nil {
				t.Fatal(err)
			}
			assertConversionRefusalUnchanged(t, ws, true)
			assertConversionRefusalUnchanged(t, ws, false)
		})
	}
}

func TestConvertFileBasedWorkspaceRecoversAfterArchivingError(t *testing.T) {
	fixture := newLegacyFixture(t)
	ws := fixture.workspace
	installInterruptedConversion(t, fixture)
	record, err := loadWorkspaceConversionRecord(t.Context(), ws)
	if err != nil || record == nil {
		t.Fatalf("conversion record = %+v, %v", record, err)
	}
	// Model an unexpected conflict appearing after the preflight and database
	// publication. Earlier renames succeed before archiving hits the conflict.
	conflict := filepath.Join(ws, LegacyRuntimeBackupDir, "session_history.json")
	if err := os.MkdirAll(filepath.Dir(conflict), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("external backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = conversionPartialError(moveLegacyFiles(ws, legacyRuntimeFiles(), record))
	if err == nil || !strings.Contains(err.Error(), "installed") || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("archiving error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, LegacyRuntimeBackupDir, legacyEventLog)); err != nil {
		t.Fatalf("expected earlier originals already archived: %v", err)
	}
	assertConversionRefusalUnchanged(t, ws, true)
	assertConversionRefusalUnchanged(t, ws, false)
	if err := os.Remove(conflict); err != nil {
		t.Fatal(err)
	}
	if result, err := ConvertFileBasedWorkspace(t.Context(), ws, false); err != nil || !result.Resumed {
		t.Fatalf("resume after resolving conflict = %+v, %v", result, err)
	}
	if session := LoadSession(ws); session == nil || session.Rounds != 4 {
		t.Fatalf("imported session after archiving failure = %+v", session)
	}
}

func TestConvertFileBasedWorkspaceRefusesWorkspacesWithoutFiles(t *testing.T) {
	if _, err := ConvertFileBasedWorkspace(context.Background(), t.TempDir(), false); !errors.Is(err, ErrNotFileBased) {
		t.Fatalf("conversion of an empty workspace = %v, want ErrNotFileBased", err)
	}
}
