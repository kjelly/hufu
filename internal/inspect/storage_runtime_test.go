package inspect

import (
	"reflect"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// TestInspectStorageReportsRuntimeStoreReadOnly covers runtime.sqlite both
// quiescent and while a writer holds it open with a live WAL.
func TestInspectStorageReportsRuntimeStoreReadOnly(t *testing.T) {
	tests := []struct {
		name       string
		keepWriter bool
	}{
		{name: "quiescent"},
		{name: "live WAL", keepWriter: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			repo, err := contextstore.OpenSQLite(workspace + "/context.sqlite")
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			writer, err := runtimestore.Open(t.Context(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.PutDocument(t.Context(), runtimestore.DocumentSession, []byte(`{"rounds":1}`)); err != nil {
				t.Fatal(err)
			}
			if !tt.keepWriter {
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				defer writer.Close()
			}

			before := snapshotInspectWorkspace(t, workspace)
			envelope, err := InspectStorage(t.Context(), InspectQuery{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			if after := snapshotInspectWorkspace(t, workspace); !reflect.DeepEqual(after, before) {
				t.Fatalf("storage inspection changed workspace entries: before=%v after=%v", snapshotNames(before), snapshotNames(after))
			}
			runtime := envelope.Data.(StorageData).Runtime
			if runtime == nil || runtime.Documents != 1 || runtime.SchemaVersion <= 0 || runtime.DatabaseBytes <= 0 || runtime.JournalMode != "wal" || (tt.keepWriter && runtime.WALBytes == 0) {
				t.Fatalf("runtime storage = %+v", runtime)
			}
		})
	}
}
