package actionstate_test

import (
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/actionstate"
	"github.com/kjelly/hufu/internal/team"
)

// Preserve the native producer/consumer regression at the runtime boundary
// after team adapters stop interpreting raw receipts themselves.
func TestProjectSourcesMatchesNativeRuntimeReceipts(t *testing.T) {
	for _, scenario := range []string{"valid", "changed_output", "changed_receipt", "compact_hash"} {
		t.Run(scenario, func(t *testing.T) {
			outputs, digest, err := team.CanonicalizeRuntimeOutputs(map[string]any{
				"scope": map[string]any{"count": jsonv1.Number("9007199254740993"), "ratio": jsonv1.Number("1e-7"), "paths": []any{"來源<&>.go", nil, true}, "empty": map[string]any{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			receiptDigest := digest
			switch scenario {
			case "changed_output":
				outputs["scope"] = map[string]any{"count": 2}
			case "changed_receipt":
				receiptDigest = "sha256:" + strings.Repeat("0", 64)
			case "compact_hash":
				data, err := jsonv1.Marshal(outputs)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(data)
				digest = "sha256:" + hex.EncodeToString(sum[:])
				receiptDigest = digest
			}
			checkpoint := map[string]any{"active_run_input_snapshot_id": "input", "tasks": []any{map[string]any{
				"ID": "source", "Status": "done", "run_input_snapshot_id": "input",
				"execution_receipt": map[string]any{"run_id": "run", "task_id": "source", "attempt": 1, "action_invocation_id": "action", "runtime_outputs_hash": receiptDigest},
				"VerifyResult":      map[string]any{"exit_code": 0, "fingerprint": "verified"},
				"typed_result":      map[string]any{"status": "success", "runtime_outputs": outputs, "runtime_outputs_hash": digest},
			}}}
			data, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			view, err := actionstate.ProjectSources(data, "run", nil)
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Tasks []struct {
					Verified bool `json:"verified_outputs"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(view, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != 1 || result.Tasks[0].Verified != (scenario == "valid") {
				t.Fatalf("runtime receipt verification: %s", view)
			}
		})
	}
}
