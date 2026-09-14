package inspect

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestInspectStorageIsReadOnlyAndReportsMissingWAL(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "item", Kind: contextstore.ContextObservation, Content: "sensitive content must not be emitted",
		Scope: contextstore.Scope{ProjectID: "project"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly retained a WAL file: %v", err)
	}

	before := snapshotInspectWorkspace(t, workspace)
	envelope, err := InspectStorage(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotInspectWorkspace(t, workspace)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("storage inspection changed workspace entries: before=%v after=%v", snapshotNames(before), snapshotNames(after))
	}
	if envelope.Kind != KindStorage || envelope.Query.Workspace != "" {
		t.Fatalf("storage envelope = %#v", envelope)
	}
	data, ok := envelope.Data.(StorageData)
	if !ok {
		t.Fatalf("storage data type = %T", envelope.Data)
	}
	if data.PageCount <= 0 || data.PageSize <= 0 || data.SchemaVersion <= 0 || data.DatabaseBytes <= 0 || data.ContextRows != 1 || data.FTSRows != 1 || data.WALBytes != 0 || data.JournalMode != "wal" {
		t.Fatalf("storage data = %#v", data)
	}
}

func TestInspectStorageDoesNotChangeActiveWALWorkspace(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "active-wal-item", Kind: contextstore.ContextObservation, Content: "active WAL content",
		Scope: contextstore.Scope{ProjectID: "project"},
	}); err != nil {
		t.Fatal(err)
	}
	before := snapshotInspectWorkspace(t, workspace)
	envelope, err := InspectStorage(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	after := snapshotInspectWorkspace(t, workspace)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("active-WAL inspection changed workspace entries: before=%v after=%v", snapshotNames(before), snapshotNames(after))
	}
	data := envelope.Data.(StorageData)
	if data.WALBytes <= 0 || data.ContextRows != 1 {
		t.Fatalf("active-WAL storage data = %#v", data)
	}
}

func snapshotNames(snapshot map[string]inspectFileSnapshot) []string {
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestInspectStorageErrorsDoNotCreateOrRepairStorage(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		workspace := filepath.Join(t.TempDir(), "missing")
		_, err := InspectStorage(t.Context(), InspectQuery{Workspace: workspace})
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v, want ErrIntegrity", err)
		}
		if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
			t.Fatalf("inspection created missing workspace: %v", statErr)
		}
	})

	t.Run("invalid schema", func(t *testing.T) {
		workspace := t.TempDir()
		db, err := sql.Open("sqlite", filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "CREATE TABLE unrelated (id INTEGER)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := snapshotInspectWorkspace(t, workspace)
		_, err = InspectStorage(t.Context(), InspectQuery{Workspace: workspace})
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v, want ErrIntegrity", err)
		}
		if after := snapshotInspectWorkspace(t, workspace); !reflect.DeepEqual(after, before) {
			t.Fatalf("invalid-schema inspection changed workspace\nbefore: %#v\nafter:  %#v", before, after)
		}
	})
}

func TestInspectStorageHonorsCancellation(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := InspectStorage(canceled, InspectQuery{Workspace: t.TempDir()})
	if !errors.Is(err, ErrIntegrity) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want ErrIntegrity and context.Canceled", err)
	}
}
