package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestFrozenRepositorySnapshotRecoveryRequiresRuntimeGrant(t *testing.T) {
	for _, mode := range []string{"current", "recovered", "no_grant", "foreign_grant", "missing_run", "foreign_task", "foreign_snapshot", "checkpoint_grant", "invalid_json", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "runtime", "runs", "resumed-run", "actions", "verify")
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HUFU_WORKSPACE", workspace)
			t.Setenv("HUFU_RUN_ID", "resumed-run")
			t.Setenv("HUFU_ACTION_INVOCATION_ID", "verify")
			t.Setenv("HUFU_TASK_ID", "2")
			t.Setenv("HUFU_RECOVERED_TASK_RUNS", "{}")
			requested := resolverScope{Kind: "repository_snapshot", Head: "HEAD"}
			manifest := &repositorySnapshotManifest{ID: "sha256:" + strings.Repeat("a", 64), Revision: "frozen-head"}
			scope := scopeAttestation{Requested: requested, RequestedInputHash: scopeInputDigest(requested), Satisfied: true, RepositorySnapshot: manifest}
			scope.Resolved.Head = manifest.Revision
			scopeBytes, err := json.Marshal(scope)
			if err != nil {
				t.Fatal(err)
			}
			var plain any
			if err := json.Unmarshal(scopeBytes, &plain); err != nil {
				t.Fatal(err)
			}
			outputs, digest, err := team.CanonicalizeRuntimeOutputs(map[string]any{"scope": plain})
			if err != nil {
				t.Fatal(err)
			}
			receipt := map[string]any{"run_id": "source-run", "task_id": "1", "attempt": 1, "action_invocation_id": "produce", "runtime_outputs_hash": digest}
			item := map[string]any{"ID": "1", "Status": "done", "contract_id": "produce-workset", "run_input_snapshot_id": "frozen-inputs", "execution_receipt": receipt,
				"VerifyResult": map[string]any{"exit_code": 0, "fingerprint": "verified"},
				"typed_result": map[string]any{"status": "success", "runtime_outputs": outputs, "runtime_outputs_hash": digest}}
			active := map[string]any{"ID": "2", "Status": "in_progress", "run_input_snapshot_id": "frozen-inputs"}
			checkpoint := map[string]any{"active_run_input_snapshot_id": "frozen-inputs", "tasks": []any{item, active}}
			switch mode {
			case "current":
				receipt["run_id"] = "resumed-run"
			case "recovered":
				t.Setenv("HUFU_RECOVERED_TASK_RUNS", `{"1":"source-run"}`)
			case "foreign_grant":
				t.Setenv("HUFU_RECOVERED_TASK_RUNS", `{"1":"foreign"}`)
			case "missing_run":
				receipt["run_id"] = ""
			case "foreign_task":
				receipt["task_id"] = "other"
			case "foreign_snapshot":
				item["run_input_snapshot_id"] = "other"
			case "checkpoint_grant":
				checkpoint["recovered_task_runs"] = map[string]string{"1": "source-run"}
			case "invalid_json":
				t.Setenv("HUFU_RECOVERED_TASK_RUNS", `{"1":false}`)
			case "oversized":
				t.Setenv("HUFU_RECOVERED_TASK_RUNS", strings.Repeat("x", (64<<10)+1))
			}
			data, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			store, err := runtimestore.Open(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.PutDocument(t.Context(), runtimestore.DocumentSession, data); err != nil {
				t.Fatal(err)
			}
			actual, err := frozenRepositorySnapshot(t.Context(), requested)
			valid := mode == "current" || mode == "recovered"
			if (err == nil) != valid || (err == nil && actual.ID != manifest.ID) {
				t.Fatalf("snapshot=%+v err=%v", actual, err)
			}
		})
	}
}
