package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestResumeCommandActivatesVersionedBranchBeforeProvider(t *testing.T) {
	oldOpts := opts
	oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel := resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel
	t.Cleanup(func() {
		opts = oldOpts
		resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = oldTeam, oldSearch, oldBranch, oldRun, oldTask, oldModel
	})
	opts = runOptions{}
	resumeTeamName, resumeSearch, resumeBranch, resumeRun, resumeRepairResult, resumeRepairModel = "", "", "", "", "", ""
	f := newVersionedCLIFixture(t)
	t.Chdir(f.subject)
	search := t.TempDir()
	dir := filepath.Join(search, "dev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"team.yaml":      "name: dev\nworker-model: ollama/version-resume-fixture\ncoordinator-model: ollama/version-resume-fixture\ngoal-mode: exploratory\nmax-steps: 3\ntimeout: 10\nacceptance:\n  required-workers: [worker]\n  require-no-unresolved-tasks: true\n",
		"worker.md":      "---\nname: worker\nrole: worker\n---\nDo not repeat accepted work.\n",
		"coordinator.md": "---\nname: coordinator\nrole: coordinator\n---\nFinish after restoring the checkpoint.\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.write(t, "version.txt", "A")
	var calls, workCalls atomic.Int32
	var resuming atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resuming.Load() {
			tree, err := team.LoadSessionTree(f.control)
			store, storeErr := versionstore.Open(r.Context(), versionstore.Options{StateDir: f.state, ReadOnly: true})
			if storeErr != nil {
				t.Errorf("provider reached without readable version state: %v", storeErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			state, ok, stateErr := store.GetSubjectState(r.Context(), f.subject)
			_ = store.Close()
			if err != nil || tree.ActiveBranch != "main" || f.read(t, "version.txt") != "A" || stateErr != nil || !ok || state.MaterializedBranchID != "main" {
				t.Errorf("provider reached before exact activation: tree=%+v state=%+v errors=%v/%v", tree, state, err, stateErr)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"version-resume-fixture","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		delta := map[string]any{"role": "assistant", "content": "Accepted original work"}
		finish := "stop"
		if strings.Contains(fmt.Sprint(request["messages"]), "supplied model-authored claims schema") {
			delta["content"] = `{"status":"success","summary":"Accepted original work"}`
		} else if resuming.Load() {
			if !strings.Contains(fmt.Sprint(request["tools"]), "name:finish") {
				t.Error("resume replayed the accepted worker")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			calls.Add(1)
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "fixture-finish", "type": "function", "function": map[string]any{"name": "finish", "arguments": `{"response":"Resumed checkpoint"}`}}}}
			finish = "tool_calls"
		} else {
			workCalls.Add(1)
		}
		chunk := map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "version-resume-fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		body, err := json.Marshal(chunk)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	opts = runOptions{workspace: f.control, workspaceMode: "exact", canonicalRun: true, providerURL: server.URL + "/v1", contextWindowOverride: 131072, displayMode: "plain", eventFormat: "text", autoApprove: true, quietMode: true}
	registry := team.NewTeamRegistry([]string{search})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	binding, _, err := bindManagedResumeWorkspace(t.Context(), &commandWorkspaceBinding{Resolution: workspacepkg.Resolution{ControlRoot: f.control}}, "dev", false)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := loadTeamByNameWithWorkspaceBinding(t.Context(), "dev", binding, registry, opts.providerURL, "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.Close() })
	result, err := tc.coordinator.RunDirectAgent(t.Context(), "worker", "produce original evidence")
	if err != nil || result.Error != nil {
		t.Fatalf("initial worker: result=%+v err=%v", result, err)
	}
	runID := ""
	for _, item := range tc.coordinator.TaskTracker().TodoList().Items() {
		if item.Status == team.TaskDone && item.ExecutionReceipt != nil {
			runID = item.ExecutionReceipt.RunID
		}
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	if runID == "" || workCalls.Load() != 1 {
		t.Fatalf("initial accepted work: run=%s calls=%d", runID, workCalls.Load())
	}
	if _, err := runCLI(t, "session", "fork", "--name", "b", "--workspace", f.control); err != nil {
		t.Fatal(err)
	}
	f.write(t, "version.txt", "B")
	resuming.Store(true)
	_, err = runCLI(t, "resume", "--workspace", f.control, "--branch", "main", "--run", runID, "--team", "dev", "--agent-team-search-path", search, "--provider-url", server.URL+"/v1", "--context-window", "131072", "--quiet")
	if err != nil || calls.Load() == 0 {
		t.Fatalf("public resume: calls=%d err=%v", calls.Load(), err)
	}
}
