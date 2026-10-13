package inspect

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSemanticRetrievalWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	traces := strings.Join([]string{
		`{"trace_id":"a","run_id":"run-1","task_id":"t1","source":"shared_persistent","route":"semantic","mode":"active","fallback_reason":"none","latency_millis":120}`,
		`{"trace_id":"b","run_id":"run-1","task_id":"t2","source":"worker_private","route":"semantic","mode":"active","fallback_reason":"embedding_warming"}`,
		`{"trace_id":"c","run_id":"run-2","task_id":"t1","source":"shared_persistent","route":"lexical","mode":"off","fallback_reason":""}`,
	}, "\n") + "\n"
	status := `{"run_id":"run-1","recorded_at":"2026-10-13T10:00:00Z","status":{"mode":"auto","state":"ready","model":"embeddinggemma:latest","queries":2,"routed_semantic":2}}` + "\n"
	for name, content := range map[string]string{"semantic-retrieval-traces.jsonl": traces, "semantic-runtime-status.jsonl": status} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

func TestInspectSemanticRetrieval(t *testing.T) {
	workspace := writeSemanticRetrievalWorkspace(t)
	tests := []struct {
		name        string
		query       InspectQuery
		wantErr     error
		wantTraces  int
		wantRuntime string
	}{
		{name: "all runs", query: InspectQuery{}, wantTraces: 3, wantRuntime: "run-1"},
		{name: "one run", query: InspectQuery{RunID: "run-1"}, wantTraces: 2, wantRuntime: "run-1"},
		{name: "run without runtime record", query: InspectQuery{RunID: "run-2"}, wantTraces: 1},
		{name: "one task", query: InspectQuery{RunID: "run-1", TaskID: "t2"}, wantTraces: 1, wantRuntime: "run-1"},
		{name: "unknown run", query: InspectQuery{RunID: "run-9"}, wantErr: ErrNotFound},
		{name: "branch filter rejected", query: InspectQuery{BranchID: "main"}, wantErr: ErrInvalidQuery},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query := tc.query
			query.Workspace = workspace
			envelope, err := InspectSemanticRetrieval(t.Context(), query)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data := envelope.Data.(SemanticRetrievalData)
			if envelope.Kind != KindSemanticRetrieval || envelope.Integrity.EventChain != "not_applicable" || data.Summary.Traces != tc.wantTraces {
				t.Fatalf("envelope = %+v data = %+v", envelope, data)
			}
			gotRuntime := ""
			if data.Runtime != nil {
				gotRuntime = data.Runtime.RunID
			}
			if gotRuntime != tc.wantRuntime {
				t.Fatalf("runtime run = %q, want %q", gotRuntime, tc.wantRuntime)
			}
		})
	}
}
