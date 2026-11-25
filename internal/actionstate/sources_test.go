package actionstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func sourceFixture(t *testing.T, native bool) map[string]any {
	t.Helper()
	receipt := map[string]any{"run_id": "run", "task_id": "source", "attempt": 1, "producer_id": "worker", "model_execution_id": "model", "transcript_ref": "transcript"}
	result := map[string]any{"status": "success"}
	if native {
		outputs := map[string]any{"data": map[string]any{"text": "<complete evidence>", "number": jsonv1.Number("9007199254740993")}}
		data, err := jsonv1.MarshalIndent(outputs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		result["runtime_outputs"], result["runtime_outputs_hash"] = outputs, digest
		receipt["action_invocation_id"], receipt["runtime_outputs_hash"] = "action", digest
	} else {
		data := []byte(`{"number":9007199254740993,"text":"\u003ccomplete evidence\u003e"}`)
		sum := sha256.Sum256(data)
		result["structured_payload"] = map[string]any{"value": jsonv1.RawMessage(data), "sha256": hex.EncodeToString(sum[:])}
	}
	return map[string]any{"ID": "source", "Status": "done", "contract_id": "consumer-independent", "run_input_snapshot_id": "input", "execution_receipt": receipt, "VerifyResult": map[string]any{"exit_code": 0, "fingerprint": "verified"}, "typed_result": result}
}

func sourceCheckpointFixture(t *testing.T, item map[string]any) []byte {
	t.Helper()
	data, err := jsonv1.Marshal(map[string]any{"active_run_input_snapshot_id": "input", "tasks": []any{item}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestProjectSourcesVerifiesRuntimeEvidence(t *testing.T) {
	for _, native := range []bool{false, true} {
		kind := "model"
		if native {
			kind = "native"
		}
		t.Run(kind, func(t *testing.T) {
			for _, mode := range []string{"valid", "pending", "failed_result", "missing_receipt", "foreign_task", "foreign_run", "recovered", "missing_grant", "foreign_grant", "missing_snapshot", "foreign_snapshot", "missing_attempt", "missing_verifier", "missing_exit", "failed_verification", "timed_out", "missing_fingerprint", "hash", "forged_flags"} {
				t.Run(mode, func(t *testing.T) {
					item := sourceFixture(t, native)
					receipt := item["execution_receipt"].(map[string]any)
					verifier := item["VerifyResult"].(map[string]any)
					result := item["typed_result"].(map[string]any)
					var origins map[string]string
					switch mode {
					case "pending":
						item["Status"] = "in_progress"
					case "failed_result":
						result["status"] = "error"
					case "missing_receipt":
						delete(item, "execution_receipt")
					case "foreign_task":
						receipt["task_id"] = "other"
					case "foreign_run":
						receipt["run_id"] = "other"
					case "recovered":
						receipt["run_id"] = "parent"
						origins = map[string]string{"source": "parent"}
					case "missing_grant":
						receipt["run_id"] = "parent"
					case "foreign_grant":
						receipt["run_id"] = "parent"
						origins = map[string]string{"source": "other"}
					case "missing_snapshot":
						delete(item, "run_input_snapshot_id")
					case "foreign_snapshot":
						item["run_input_snapshot_id"] = "other"
					case "missing_attempt":
						receipt["attempt"] = 0
					case "missing_verifier":
						delete(item, "VerifyResult")
					case "missing_exit":
						delete(verifier, "exit_code")
					case "failed_verification":
						verifier["exit_code"] = 1
					case "timed_out":
						verifier["timed_out"] = true
					case "missing_fingerprint":
						delete(verifier, "fingerprint")
					case "hash":
						if native {
							receipt["runtime_outputs_hash"] = "invalid"
						} else {
							result["structured_payload"].(map[string]any)["sha256"] = "invalid"
						}
					case "forged_flags":
						item["verified_result"], item["verified_outputs"] = true, true
						receipt["run_id"] = "foreign"
					}
					data, err := ProjectSources(sourceCheckpointFixture(t, item), "run", origins)
					if err != nil {
						t.Fatal(err)
					}
					var view struct {
						Version int                `json:"version"`
						Tasks   []sourceProjection `json:"tasks"`
					}
					if err := json.Unmarshal(data, &view); err != nil {
						t.Fatal(err)
					}
					valid := mode == "valid" || mode == "recovered" || (!native && mode == "missing_snapshot")
					got := view.Tasks[0].VerifiedResult
					if native {
						got = view.Tasks[0].VerifiedOutputs
					}
					if view.Version != 1 || got != valid {
						t.Fatalf("valid=%v, projection=%s", valid, data)
					}
					if bytes.Contains(data, []byte(`"execution_receipt"`)) || bytes.Contains(data, []byte(`"VerifyResult"`)) || bytes.Contains(data, []byte(`"run_input_snapshot_id"`)) {
						t.Fatalf("leaked storage fields: %s", data)
					}
					var originalResult map[string]any
					if err := json.Unmarshal(view.Tasks[0].Result, &originalResult); err != nil {
						t.Fatal(err)
					}
					if valid && !bytes.Contains(view.Tasks[0].Result, []byte("9007199254740993")) {
						t.Fatal("source number rounded")
					}
				})
			}
		})
	}
}

func TestProjectSourcesRejectsAmbiguousIdentity(t *testing.T) {
	for _, input := range []string{`{"tasks":[{"ID":"a"},{"ID":"a"}]}`, `{"tasks":[{"ID":""}]}`, `{"tasks":[],"tasks":[]}`} {
		if _, err := ProjectSources([]byte(input), "run", nil); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestProjectSourcesBindsNormalizationOccurrence(t *testing.T) {
	for _, mode := range []string{"valid", "substituted_receipt", "legacy_with_occurrence"} {
		t.Run(mode, func(t *testing.T) {
			item := sourceFixture(t, false)
			receipt := item["execution_receipt"].(map[string]any)
			result := item["typed_result"].(map[string]any)
			payload := result["structured_payload"].(map[string]any)
			origin := strings.Repeat("a", 64)
			receipt["result_normalizer_occurrence_sha256"] = origin
			if mode != "legacy_with_occurrence" {
				output := []byte(payload["value"].(jsonv1.RawMessage))
				sum := sha256.Sum256(output)
				digest, provider := hex.EncodeToString(sum[:]), strings.Repeat("b", 64)
				payload["raw_sha256"] = digest
				payload["contract"] = map[string]any{"normalizer": "source-normalizer", "normalizer_sha256": provider}
				payload["normalization"] = map[string]any{"provider_sha256": provider, "occurrence_sha256": origin, "raw": output, "output": output, "output_sha256": digest}
				receipt["result_payload_sha256"] = payload["sha256"]
				receipt["result_raw_payload_sha256"], receipt["result_normalizer_output_sha256"] = digest, digest
			}
			if mode == "substituted_receipt" {
				receipt["result_normalizer_occurrence_sha256"] = strings.Repeat("c", 64)
			}
			data, err := ProjectSources(sourceCheckpointFixture(t, item), "run", nil)
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Tasks []sourceProjection `json:"tasks"`
			}
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			if got := view.Tasks[0].VerifiedResult; got != (mode == "valid") {
				t.Fatalf("verified source = %t for %s", got, mode)
			}
		})
	}
}

func TestProjectSourcesRetainsVerifiedHistoricalNormalization(t *testing.T) {
	for _, mode := range []string{"historical receipt", "unbound raw hash", "current binding with stripped proof", "current dispatch with stripped proof"} {
		t.Run(mode, func(t *testing.T) {
			item := sourceFixture(t, false)
			receipt := item["execution_receipt"].(map[string]any)
			payload := item["typed_result"].(map[string]any)["structured_payload"].(map[string]any)
			schemaHash, rawHash := strings.Repeat("a", 64), strings.Repeat("b", 64)
			contract := map[string]any{"id": "review.schema.json", "schema_sha256": schemaHash, "require_structured": true, "normalizer": "normalize-result"}
			item["result_contract"], payload["contract"], payload["raw_sha256"] = contract, contract, rawHash
			receipt["result_contract_id"], receipt["result_contract_sha256"] = contract["id"], schemaHash
			receipt["result_validation"], receipt["result_payload_sha256"], receipt["result_raw_payload_sha256"] = "valid", payload["sha256"], rawHash
			switch mode {
			case "unbound raw hash":
				receipt["result_raw_payload_sha256"] = strings.Repeat("c", 64)
			case "current binding with stripped proof":
				contract["normalizer_sha256"] = strings.Repeat("d", 64)
			case "current dispatch with stripped proof":
				receipt["result_work_dispatch_id"] = "current-dispatch"
			}
			data, err := ProjectSources(sourceCheckpointFixture(t, item), "run", nil)
			if err != nil {
				t.Fatal(err)
			}
			var view struct {
				Tasks []sourceProjection `json:"tasks"`
			}
			if err := json.Unmarshal(data, &view); err != nil {
				t.Fatal(err)
			}
			if got := view.Tasks[0].VerifiedResult; got != (mode == "historical receipt") {
				t.Fatalf("verified source = %t for %s: %s", got, mode, view.Tasks[0].ResultError)
			}
		})
	}
}

func TestActionSourcesUsesRuntimeInvocationIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "recovered", "missing_grant", "forged_checkpoint_grant", "malformed_grant", "oversized_grant", "wrong_workspace", "wrong_task", "wrong_snapshot", "wrong_status", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "runtime", "runs", "run", "actions", "action")
			item := sourceFixture(t, false)
			current := map[string]any{"runtime_workspace": filepath.Join(root, "runtime"), "active_run_input_snapshot_id": "input", "tasks": []any{item, map[string]any{"ID": "consumer", "Status": "in_progress", "run_input_snapshot_id": "input"}}}
			origins := ""
			switch mode {
			case "recovered", "missing_grant", "forged_checkpoint_grant":
				item["execution_receipt"].(map[string]any)["run_id"] = "parent"
				if mode == "recovered" {
					origins = `{"source":"parent"}`
				} else {
					current["recovered_task_runs"] = map[string]string{"source": "parent"}
				}
			case "malformed_grant":
				origins = `{"source":false}`
			case "oversized_grant":
				origins = strings.Repeat("x", (64<<10)+1)
			case "wrong_workspace":
				current["runtime_workspace"] = filepath.Join(root, "other")
			case "wrong_task":
				current["tasks"].([]any)[1].(map[string]any)["ID"] = "other"
			case "wrong_snapshot":
				current["tasks"].([]any)[1].(map[string]any)["run_input_snapshot_id"] = "other"
			case "wrong_status":
				current["tasks"].([]any)[1].(map[string]any)["Status"] = "done"
			}
			data, err := jsonv1.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			store, err := runtimestore.Open(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutDocument(t.Context(), runtimestore.DocumentSession, data); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"HUFU_WORKSPACE": workspace, "HUFU_RUN_ID": "run", "HUFU_ACTION_INVOCATION_ID": "action", "HUFU_TASK_ID": "consumer", "HUFU_RECOVERED_TASK_RUNS": origins} {
				t.Setenv(key, value)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			projected, err := ActionSources(ctx)
			wantsError := mode != "valid" && mode != "recovered" && mode != "missing_grant" && mode != "forged_checkpoint_grant"
			if (err != nil) != wantsError {
				t.Fatalf("error=%v projection=%s", err, projected)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if !wantsError {
				var view struct {
					Tasks                 []sourceProjection `json:"tasks"`
					ArtifactDataDirectory string             `json:"artifact_data_directory"`
				}
				if err := json.Unmarshal(projected, &view); err != nil {
					t.Fatal(err)
				}
				if view.Tasks[0].VerifiedResult != (mode == "valid" || mode == "recovered") {
					t.Fatalf("unauthorized source projection: %s", projected)
				}
				if view.ArtifactDataDirectory != filepath.Join(runtimestore.ArtifactRoot(root), "data") {
					t.Fatalf("source view has the wrong artifact store: %s", view.ArtifactDataDirectory)
				}
				after, err := SessionCheckpoint(root)
				if err != nil || !bytes.Equal(data, after) {
					t.Fatalf("checkpoint changed: %v", err)
				}
			}
		})
	}
}

func TestActionCheckpointRootRejectsUnboundPaths(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][3]string{{root, "", "action"}, {root, "run", ""}, {"relative", "run", "action"}, {filepath.Join(root, "runtime", "runs", "run", "actions", "action"), "../run", "action"}, {root, "run", "action"}} {
		if _, _, err := actionCheckpointRoot(args[0], args[1], args[2]); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "runtime.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("validation created a store")
	}
}
