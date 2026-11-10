package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReviewGateKeepsRuntimeAuthorizedRecoveryOrigins(t *testing.T) {
	for _, mode := range []string{"accepted", "missing_grant", "foreign_grant", "missing_snapshot", "foreign_snapshot", "foreign_receipt_task", "forged_checkpoint"} {
		t.Run(mode, func(t *testing.T) {
			current := gateFixture(t)
			current.RecoveredTaskRuns = map[string]string{}
			for i := range current.Tasks {
				item := &current.Tasks[i]
				item.SnapshotID = current.SnapshotID
				if item.ContractID == "synthesize-review" || item.ContractID == "inventory-review-handoffs" {
					item.Receipt.RunID = "resumed-run"
				} else {
					current.RecoveredTaskRuns[item.ID] = item.Receipt.RunID
				}
			}
			switch mode {
			case "missing_grant":
				delete(current.RecoveredTaskRuns, "3")
			case "foreign_grant":
				current.RecoveredTaskRuns["3"] = "unrelated"
			case "missing_snapshot":
				current.Tasks[0].SnapshotID = ""
			case "foreign_snapshot":
				current.Tasks[0].SnapshotID = "unrelated"
			case "foreign_receipt_task":
				current.Tasks[0].Receipt.TaskID = "other"
			case "forged_checkpoint":
				data := raw(t, current)
				current.RecoveredTaskRuns = nil
				data = append([]byte(`{"recovered_task_runs":{"3":"run-1"},`), data[1:]...)
				if err := json.Unmarshal(data, &current); err != nil {
					t.Fatal(err)
				}
			}
			id, digest, err := verify(current, "resumed-run")
			if (err == nil) != (mode == "accepted") || (err == nil && (id != "7" || digest == "")) {
				t.Fatalf("report=%s hash=%s err=%v", id, digest, err)
			}
		})
	}
}

func TestEmbeddedReviewGateUsesRuntimeRecoveryGrant(t *testing.T) {
	for _, capability := range []string{"verify-review-report", "inventory-review-handoffs"} {
		t.Run(capability, func(t *testing.T) {
			teamDir, err := filepath.Abs("..")
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			provider, ok := loaded.ProviderRegistry.Get(capability)
			if !ok {
				t.Fatal("native gate provider missing")
			}
			workspace, _ := actionFixture(t)
			root, _, err := checkpointRoot(workspace, "run-1", "action-1")
			if err != nil {
				t.Fatal(err)
			}
			var current session
			if err := json.Unmarshal(readCheckpoint(t, root), &current); err != nil {
				t.Fatal(err)
			}
			origins := map[string]string{}
			for i := range current.Tasks {
				item := &current.Tasks[i]
				if item.Status == "done" {
					item.SnapshotID = current.SnapshotID
					origins[item.ID] = item.Receipt.RunID
				} else {
					item.ContractID = capability
				}
			}
			writeCheckpoint(t, root, raw(t, current))
			workspace = filepath.Join(root, "runtime", "runs", "resumed-run", "actions", "action-2")
			if err := os.MkdirAll(workspace, 0o700); err != nil {
				t.Fatal(err)
			}
			actionType := "verify_review_report"
			if capability == "inventory-review-handoffs" {
				actionType = "inventory_review_handoffs"
			}
			action := team.Action{Capability: capability, Type: actionType, Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`}
			environment := team.ActionEnvironment{Workspace: workspace, RunID: "resumed-run", ActionInvocationID: "action-2", TaskID: "8", RecoveredTaskRuns: origins}
			result, err := provider.Execute(team.WithActionEnvironment(t.Context(), environment), action)
			if err != nil || !strings.Contains(string(raw(t, result)), `"passed":true`) {
				t.Fatalf("authorized native recovery failed: result=%v err=%v", result, err)
			}
			environment.RecoveredTaskRuns = nil
			result, err = provider.Execute(team.WithActionEnvironment(t.Context(), environment), action)
			if err == nil && strings.Contains(string(raw(t, result)), `"passed":true`) {
				t.Fatal("native gate accepted old receipts without a runtime grant")
			}
		})
	}
}

func TestRecoveryOriginsRequiresBoundedRuntimeJSON(t *testing.T) {
	for _, encoded := range []string{`{"3":"run-1"}`, "", "{}", `{"3":false}`, strings.Repeat("x", (64<<10)+1)} {
		origins, err := recoveryOrigins(encoded)
		invalid := encoded == `{"3":false}` || len(encoded) > 64<<10
		if (err != nil) != invalid {
			t.Fatalf("unexpected runtime origin parse result: err=%v", err)
		}
		if encoded == `{"3":"run-1"}` && origins["3"] != "run-1" {
			t.Fatal("runtime origin was lost")
		}
	}
}
