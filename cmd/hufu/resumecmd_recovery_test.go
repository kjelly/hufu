package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestResumeRecoveryPreviewThenExecute(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		succeeds, delegated, noInputs bool
	}{
		{name: "direct", succeeds: true},
		{name: "direct_exhausted"},
		{name: "delegated", succeeds: true, delegated: true},
		{name: "direct_empty_inputs", succeeds: true, noInputs: true},
		{name: "delegated_empty_inputs", succeeds: true, delegated: true, noInputs: true},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseResumeRecovery(t, tc.succeeds, tc.delegated, tc.noInputs) })
	}
}

func exerciseResumeRecovery(t *testing.T, succeeds, delegated, noInputs bool) {
	t.Helper()
	oldOpts := opts
	oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel := resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel
	t.Cleanup(func() {
		opts = oldOpts
		resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel
	})
	root := t.TempDir()
	search := filepath.Join(root, "teams")
	teamDir := filepath.Join(search, "fixture-team")
	for _, dir := range []string{teamDir, filepath.Join(root, "home"), filepath.Join(root, "project")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("HUFU_STATE_HOME", filepath.Join(root, "state"))
	t.Chdir(filepath.Join(root, "project"))
	// Recovery exercises durable CLI checkpoints and bounded collector turns;
	// its contract is turn/work preservation, not a five-second latency bound.
	files := map[string]string{
		"team.yaml":      "name: fixture-team\nworker-model: ollama/recovery-cli-fixture\ncoordinator-model: ollama/recovery-cli-fixture\nresult-transport: runtime\ngoal-mode: exploratory\ntimeout: 30\nmax-retries: 0\nacceptance:\n  required-workers: [worker]\n  require-no-unresolved-tasks: true\ninputs:\n  purpose:\n    type: string\n    default: review\n",
		"worker.md":      "---\nname: worker\nrole: worker\n---\nReturn a report with original limitations.\n",
		"coordinator.md": "---\nname: coordinator\nrole: coordinator\n---\nCall finish after recovery.\n",
	}
	if noInputs {
		files["team.yaml"], _, _ = strings.Cut(files["team.yaml"], "inputs:\n")
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(teamDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var repair atomic.Bool
	var dispatched atomic.Bool
	var workCalls, collectorCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"recovery-cli-fixture","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		prompt := fmt.Sprint(request["messages"])
		delta := map[string]any{"role": "assistant"}
		finishReason := "stop"
		if strings.Contains(prompt, "supplied model-authored claims schema") {
			collectorCalls.Add(1)
			if repair.Load() && succeeds {
				delta["content"] = `{"status":"success","summary":"Original limitation preserved"}`
			} else {
				delta["content"] = `{"status":42,"summary":"Original limitation preserved"}`
			}
		} else if !repair.Load() && delegated && strings.Contains(fmt.Sprint(request["tools"]), "name:agent") && !dispatched.Swap(true) {
			finishReason = "tool_calls"
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "fixture-delegate", "type": "function", "function": map[string]any{"name": "agent", "arguments": `{"tasks":[{"agent":"worker","goal":"review fixture"}]}`}}}
		} else if repair.Load() || (delegated && strings.Contains(fmt.Sprint(request["tools"]), "name:finish")) {
			finishReason = "tool_calls"
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "recovery-finish", "type": "function", "function": map[string]any{"name": "finish", "arguments": `{"response":"Recovered review complete"}`}}}
		} else {
			workCalls.Add(1)
			delta["content"] = "Original completed work with a local limitation"
		}
		chunk := map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "recovery-cli-fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}}}
		body, err := json.Marshal(chunk)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	workspace := filepath.Join(root, "workspace")
	opts = runOptions{workspace: workspace, workspaceMode: "exact", canonicalRun: true, providerURL: server.URL + "/v1", contextWindowOverride: 131072, displayMode: "plain", eventFormat: "text", autoApprove: true, quietMode: true}
	registry := team.NewTeamRegistry([]string{search})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	tc, err := loadTeamByName(t.Context(), "fixture-team", registry, opts.providerURL, "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	var direct *team.DirectAgentResult
	var directErr error
	if delegated {
		_, directErr = tc.coordinator.Run(t.Context(), "review fixture")
	} else {
		direct, directErr = tc.coordinator.RunDirectAgent(t.Context(), "worker", "review fixture")
	}
	if directErr == nil && (direct == nil || direct.Error == nil) {
		t.Fatal("invalid collector claims unexpectedly accepted")
	}
	var blocked *team.TodoItem
	for _, item := range tc.coordinator.TaskTracker().TodoList().Items() {
		if item.Status == team.TaskBlocked && item.ExecutionReceipt != nil && item.ExecutionReceipt.ResultCollection != nil {
			blocked = item
			break
		}
	}
	if blocked == nil {
		t.Fatalf("original run did not preserve blocked collection: direct=%+v err=%v tasks=%+v", direct, directErr, tc.coordinator.TaskTracker().TodoList().Items())
	}
	snapshot := tc.coordinator.RunInputSnapshot()
	if snapshot == nil || team.ValidateRunInputSnapshot(snapshot) != nil || (noInputs && len(snapshot.Inputs) != 0) {
		t.Fatalf("original invocation has no frozen input identity: %+v", snapshot)
	}
	checkpoint := blocked.ExecutionReceipt.ResultCollection
	if blocked.RunInputSnapshotID != snapshot.ID || blocked.RunInputSnapshotHash != snapshot.SnapshotHash || blocked.ExecutionReceipt.RunInputSnapshotID != snapshot.ID || blocked.ExecutionReceipt.RunInputSnapshotHash != snapshot.SnapshotHash || checkpoint.InputsID != snapshot.ID || checkpoint.InputsSHA256 != snapshot.SnapshotHash {
		t.Fatalf("task/receipt/checkpoint lost invocation inputs: %+v", blocked)
	}
	if saved := team.LoadSession(workspace); saved != nil && saved.RecoveryRequired {
		t.Fatalf("initial run finalization incomplete: %s; result=%+v direct=%+v err=%v", saved.RecoveryReason, tc.coordinator.LastRunResult(), direct, directErr)
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = "fixture-team", search, tree.ActiveBranch, blocked.ExecutionReceipt.RunID, blocked.ID, ""
	originalWork, originalTurns := blocked.ExecutionReceipt.ResultCollection.Work, blocked.ExecutionReceipt.ResultCollection.TurnsUsed
	before := runtimeStoreFiles(t, workspace)
	beforeCalls := collectorCalls.Load()
	opts.dryRun, opts.outputFormat = true, "json"
	cmd := &cobra.Command{Use: "resume"}
	cmd.SetContext(t.Context())
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runResumeCommand(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var a team.ResultCollectionRecoveryAssessment
	if err := json.Unmarshal(output.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Disposition != team.RecoveryRepairClaims || a.TaskID != blocked.ID {
		t.Fatalf("preview=%+v", a)
	}
	if !reflect.DeepEqual(before, runtimeStoreFiles(t, workspace)) || beforeCalls != collectorCalls.Load() {
		t.Fatal("positive CLI preview wrote state or called model")
	}
	repair.Store(true)
	opts.dryRun, opts.outputFormat = false, ""
	runErr := runResumeCommand(cmd, nil)
	if (runErr == nil) != succeeds {
		t.Fatalf("repair succeeds=%t error=%v", succeeds, runErr)
	}
	source, err := team.ReadResumeSource(workspace, resumeBranch, "")
	if err != nil {
		t.Fatal(err)
	}
	var done *team.TodoItem
	for _, item := range source.Session.Tasks {
		if item.ID == blocked.ID {
			done = item
		}
	}
	expectedStatus, additional := team.TaskDone, 1
	if !succeeds {
		expectedStatus, additional = team.TaskBlocked, 2
	}
	if done == nil || done.Status != expectedStatus || done.ExecutionReceipt.ResultCollection.Work != originalWork || done.ExecutionReceipt.ResultCollection.TurnsUsed != originalTurns+additional || workCalls.Load() != 1 || collectorCalls.Load() != beforeCalls+int32(additional) {
		t.Fatalf("done=%+v work=%d collector=%d", done, workCalls.Load(), collectorCalls.Load())
	}
	if !succeeds {
		beforeCalls = collectorCalls.Load()
		used := done.ExecutionReceipt.ResultCollection.TurnsUsed
		if used != done.ExecutionReceipt.ResultCollection.TurnLimit {
			t.Fatal("repair stopped before its bounded allowance was exhausted")
		}
		resumeRepairResult = ""
		if err := runResumeCommand(cmd, nil); err == nil {
			t.Fatal("ordinary resume accepted unresolved exhausted collection")
		}
		if collectorCalls.Load() != beforeCalls || workCalls.Load() != 1 {
			t.Fatal("ordinary resume implicitly renewed collection budget or replayed work")
		}
		after, err := team.ReadResumeSource(workspace, resumeBranch, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range after.Session.Tasks {
			if item.ID == blocked.ID && (item.ExecutionReceipt.ResultCollection.TurnsUsed != used || len(item.ExecutionReceipt.RepairProvenance.OperatorGrants) != 1) {
				t.Fatal("ordinary resume changed consumed turns or added a grant")
			}
		}
	}
}
