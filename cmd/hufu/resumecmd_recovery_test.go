package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestResumeRecoveryPreviewThenExecute(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	for _, tc := range []struct {
		name                          string
		succeeds, delegated, noInputs bool
		workflow                      bool
	}{
		{name: "direct", succeeds: true},
		{name: "direct_exhausted"},
		{name: "delegated", succeeds: true, delegated: true},
		{name: "direct_empty_inputs", succeeds: true, noInputs: true},
		{name: "delegated_empty_inputs", succeeds: true, delegated: true, noInputs: true},
		{name: "failed_workflow", succeeds: true, delegated: true, workflow: true},
		{name: "failed_workflow_exhausted", delegated: true, workflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) { exerciseResumeRecovery(t, tc.succeeds, tc.delegated, tc.noInputs, tc.workflow) })
	}
}

func exerciseResumeRecovery(t *testing.T, succeeds, delegated, noInputs, workflow bool) {
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
	if workflow {
		files["team.yaml"] += "workflow:\n  phases: [prepare, verify]\npolicies:\n  allow_phase_skip: true\ndelegation:\n  bind-task-goal-contracts: true\ntasks:\n  - id: prepare\n    agent: worker\n    phase: prepare\n    side_effect: none\n    execution:\n      requires_result: true\n  - id: verify\n    agent: verifier\n    phase: verify\n    side_effect: none\n    execution:\n      requires_result: true\n"
		files["verifier.md"] = "---\nname: verifier\nrole: worker\n---\nVerify the retained result.\n"
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(teamDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var repair atomic.Bool
	var dispatched atomic.Bool
	var verificationDispatched atomic.Bool
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
			arguments := `{"tasks":[{"agent":"worker","goal":"review fixture"}]}`
			if workflow {
				arguments = `{"tasks":[{"agent":"worker","goal":"review fixture","contract_id":"prepare"}]}`
			}
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "fixture-delegate", "type": "function", "function": map[string]any{"name": "agent", "arguments": arguments}}}
		} else if repair.Load() && workflow && strings.Contains(fmt.Sprint(request["tools"]), "name:agent") && !verificationDispatched.Swap(true) {
			finishReason = "tool_calls"
			delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "fixture-verify", "type": "function", "function": map[string]any{"name": "agent", "arguments": `{"tasks":[{"agent":"verifier","goal":"verify retained result","contract_id":"verify"}]}`}}}
		} else if (repair.Load() && !workflow) || (delegated && strings.Contains(fmt.Sprint(request["tools"]), "name:finish")) {
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
	// Both initial loading and resume must use this fixture's subject. Process
	// CWD is ambient state and can change while in-process tools finish.
	project := canonicalRuntimePath(filepath.Join(root, "project"))
	// Stop subject-root discovery here, independent of Git markers in the
	// configured temporary root or its ancestors.
	if output, err := exec.CommandContext(t.Context(), "git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("initialize recovery project: %v\n%s", err, output)
	}
	opts = runOptions{workspace: workspace, workspaceMode: "exact", startDir: project, subjectRoot: project, canonicalRun: true, providerURL: server.URL + "/v1", contextWindowOverride: 131072, displayMode: "plain", eventFormat: "text", autoApprove: true, quietMode: true}
	opts.modelOverride, opts.maxTokensOverride, opts.temperatureOverride = "ollama/recovery-cli-fixture", "4096", "0.15"
	opts.timeoutOverride = 45
	registry := team.NewTeamRegistry([]string{search})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	tc, err := loadTeamByName(t.Context(), "fixture-team", registry, opts.providerURL, "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	if tc.session.Scope.SubjectRoot != project {
		t.Fatalf("initial subject root = %q, want %q", tc.session.Scope.SubjectRoot, project)
	}
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
	if workflow {
		source, err := team.ReadResumeSource(workspace, resumeBranch, "")
		if err != nil || source.Session.WorkflowState != team.PhaseFailed {
			t.Fatalf("fixture did not persist a failed workflow: source=%+v error=%v", source, err)
		}
	}
	originalWork, originalTurns := blocked.ExecutionReceipt.ResultCollection.Work, blocked.ExecutionReceipt.ResultCollection.TurnsUsed
	// Resume is a new CLI invocation: no original model/generation/timeout
	// flags survive. Rehydrate them from the selected SQLite event lineage.
	opts.modelOverride, opts.maxTokensOverride, opts.temperatureOverride = "", "", ""
	opts.timeoutOverride, opts.contextWindowOverride = 0, 0
	before := runtimeStoreFiles(t, workspace)
	beforeCalls := collectorCalls.Load()
	opts.dryRun, opts.outputFormat = true, "json"
	cmd := &cobra.Command{Use: "resume"}
	cmd.SetContext(t.Context())
	var output bytes.Buffer
	cmd.SetOut(&output)
	t.Chdir(project)
	if succeeds && !delegated && !noInputs && !workflow {
		opts.temperatureOverride = "0.8"
		if err := runResumeCommand(cmd, nil); err != nil {
			t.Fatal(err)
		}
		var rejected team.ResultCollectionRecoveryAssessment
		if err := json.Unmarshal(output.Bytes(), &rejected); err != nil || rejected.Disposition != team.RecoveryBlocked {
			t.Fatalf("changed launch override was not blocked: %+v %v", rejected, err)
		}
		if !reflect.DeepEqual(before, runtimeStoreFiles(t, workspace)) || beforeCalls != collectorCalls.Load() {
			t.Fatal("blocked launch override wrote state or invoked a model")
		}
		opts.temperatureOverride = ""
		output.Reset()
	}
	if err := runResumeCommand(cmd, nil); err != nil {
		t.Fatal(err)
	}
	var a team.ResultCollectionRecoveryAssessment
	if err := json.Unmarshal(output.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.Disposition != team.RecoveryRepairClaims || a.TaskID != blocked.ID {
		t.Fatalf("preview=%+v; subject=%q cwd=%q", a, project, currentWorkingDir())
	}
	if !reflect.DeepEqual(before, runtimeStoreFiles(t, workspace)) || beforeCalls != collectorCalls.Load() {
		t.Fatal("positive CLI preview wrote state or called model")
	}
	repair.Store(true)
	opts.dryRun, opts.outputFormat = false, ""
	t.Chdir(project)
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
	expectedWork, expectedCollections := int32(1), beforeCalls+int32(additional)
	if workflow && succeeds {
		expectedWork++
		expectedCollections++
		if source.Session.WorkflowState != team.PhaseDone {
			t.Fatalf("repaired task did not settle the failed workflow: %s", source.Session.WorkflowState)
		}
	}
	if done == nil || done.Status != expectedStatus || done.ExecutionReceipt.ResultCollection.Work != originalWork || done.ExecutionReceipt.ResultCollection.TurnsUsed != originalTurns+additional || workCalls.Load() != expectedWork || collectorCalls.Load() != expectedCollections {
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
