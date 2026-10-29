package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

const historicalProcessBody = "AUTHORIZED_HISTORICAL_BODY_938172"

type historicalRunProcessFixture struct {
	versionedCLIFixture
	search, resultID, sourceEventID, sourceHash string
}

func historicalRunProcessSetup(t *testing.T, endpoint string, semantic bool, variant string) historicalRunProcessFixture {
	t.Helper()
	f := newVersionedCLIFixture(t)
	t.Chdir(f.subject)
	// Grant intake is independent of file versioning mode. The derived-fork
	// matrix separately exercises required-mode snapshot materialization.
	configuration := "workspace-versioning:\n  mode: off\n"
	if variant == "fallback" {
		configuration += "execution-routes:\n  research:\n    candidates: [ollama/test, ollama/test-fallback]\n    fallback-on: [rate_limited]\n"
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".config", "hufu", "hufu.yaml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	search := filepath.Join(f.subject, ".agent-teams")
	dir := filepath.Join(search, "dev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`name: dev
worker-model: ollama/test
coordinator-model: ollama/test
provider-url: %s/v1
context-window: 65536
result-transport: tool
goal-mode: outcome
timeout: 15
max-rounds: 6
max-retries: 0
workflow:
  phases: [prepare, verify]
policies:
  allow_phase_skip: true
  require_phase_success: true
delegation:
  bind-task-goal-contracts: true
  allowed-workers: [researcher, verifier, observer]
acceptance:
  commands: ["true"]
  required-workers: [researcher, verifier]
  require-no-unresolved-tasks: true
contracts:
  - id: prepare-reference
    agent: researcher
    phase: prepare
    goal: Prepare a fresh analysis from the authorized reference.
    execution: {requires-result: true}
    verify: "true"
  - id: verify-analysis
    agent: verifier
    phase: verify
    goal: Verify the fresh analysis.
    execution: {requires-result: true}
    verify: "true"
`, endpoint)
	if semantic {
		manifest += "semantic-retrieval:\n  mode: shadow\n  endpoint: " + endpoint + "\n"
	}
	if variant == "extra_model" {
		// Fanout produces the existing runtime merge rather than a submitted
		// worker result. The submitted verifier attests the merged analysis.
		manifest = strings.Replace(manifest, "required-workers: [researcher, verifier]", "required-workers: [verifier]", 1)
	}
	files := map[string]string{
		"team.yaml":      manifest,
		"coordinator.md": "---\nname: coordinator\nrole: coordinator\n---\nFollow the declared contracts, then finish.\n",
		"researcher.md":  "---\nname: researcher\nrole: worker\n---\nResearch reference worker. Return a fresh analysis.\n",
		"verifier.md":    "---\nname: verifier\nrole: worker\n---\nVerifier current-cycle worker. Verify the fresh output.\n",
		"observer.md":    "---\nname: observer\nrole: worker\n---\nObserver receives work only if needed.\n",
	}
	switch variant {
	case "fallback":
		files["researcher.md"] = strings.Replace(files["researcher.md"], "role: worker\n", "role: worker\nexecution-route: research\n", 1)
	case "extra_model":
		files["researcher.md"] = strings.Replace(files["researcher.md"], "role: worker\n", "role: worker\nextra-models: [ollama/test-extra]\n", 1)
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previous := opts
	opts = runOptions{canonicalRun: true, intent: "execute", agentTeamName: "dev", workspaceMode: "default", startDir: f.subject, subjectRoot: f.subject}
	defer func() { opts = previous }()
	registry := team.NewTeamRegistry([]string{search})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	tc, err := loadTeamByName(t.Context(), "dev", registry, endpoint+"/v1", "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(f.control, "source-run", filepath.Base(f.control))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var source team.RunEvent
	for _, event := range []team.RunEvent{
		{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{"team":"dev"}`)},
		{Type: string(team.EventTaskCreated), Actor: "coordinator", TaskID: "1", Payload: []byte(`{"id":"1","agent":"researcher","desc":"original accepted analysis","status":"pending","model":"ollama/test","execution_target":{"backend":"ollama","model":"test"},"execution_topology":[{"backend":"ollama","model":"test"}]}`)},
		{Type: string(team.EventTaskCompleted), Actor: "researcher", TaskID: "1", Payload: []byte(`{"id":"1","agent":"researcher","desc":"original accepted analysis","status":"done","typed_result":{"task_id":"1","agent":"researcher","attempt":1,"source":"submitted","status":"success","summary":"accepted source analysis","details":"` + historicalProcessBody + `","future_detail":{"count":9007199254740993}}}`)},
		{Type: string(team.EventWorkflowStateChanged), Actor: "runtime", Payload: []byte(`{"state":"DONE","phase_results":{"VERIFY":{"status":"SUCCESS"}}}`)},
		{Type: string(team.EventRunFinished), Actor: "coordinator", Payload: []byte(`{"outcome":"completed","goal_satisfied":true,"response":"original accepted report"}`)},
	} {
		durable, err := store.AppendPersisted(event)
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == string(team.EventTaskCompleted) {
			source = durable
		}
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(f.control, team.ReduceToSessionData(events)); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSessionTree(f.control, team.NewSessionTree()); err != nil {
		t.Fatal(err)
	}
	return historicalRunProcessFixture{versionedCLIFixture: f, search: search, resultID: team.HistoricalResultID(source), sourceEventID: source.ID, sourceHash: source.Hash}
}

func (f historicalRunProcessFixture) run(t *testing.T, binary string, flags []string, prompt string) (jsonRunOutput, error, string) {
	t.Helper()
	args := []string{"run", "--team", "dev", "--agent-team-search-path", f.search, "--output", "json"}
	args = append(args, flags...)
	args = append(args, "--", prompt)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	childTemp, err := os.MkdirTemp(os.TempDir(), "hist-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(childTemp); err != nil {
			t.Error(err)
		}
	})
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = f.subject
	command.Env = []string{"HOME=" + os.Getenv("HOME"), "HUFU_STATE_HOME=" + os.Getenv("HUFU_STATE_HOME"), "PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTemp, "NO_COLOR=1", "TERM=dumb"}
	for _, name := range []string{"GOCACHE", "GOTMPDIR", "GOMODCACHE", "GOTOOLCHAIN", "GOMAXPROCS"} {
		if value := os.Getenv(name); value != "" {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var output jsonRunOutput
	if stdout.Len() > 0 {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &output); decodeErr != nil {
			t.Fatalf("invalid CLI JSON: %v\n%s\n%s", decodeErr, stdout.String(), stderr.String())
		}
	}
	return output, err, stderr.String()
}

func TestHistoricalResultRunBinaryAdmissionAndDelivery(t *testing.T) {
	for _, variant := range []string{"single", "fallback", "extra_model"} {
		t.Run(variant, func(t *testing.T) { historicalResultRunBinaryAdmissionAndDelivery(t, variant) })
	}
}

func historicalResultRunBinaryAdmissionAndDelivery(t *testing.T, variant string) {
	t.Helper()
	var requests, coordinatorCalls, researcherCalls, verifierCalls atomic.Int64
	var alternateCalls atomic.Int64
	var rateLimited atomic.Bool
	var expectSource atomic.Bool
	expectSource.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/models" {
				_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"test","object":"model"}]}`)
			} else {
				_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 32768","model_info":{"general.architecture":"qwen2","qwen2.context_length":32768},"capabilities":["completion","tools"]}`)
			}
			return
		}
		var request struct {
			contractChatRequest
			Model string `json:"model"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		encoded, _ := json.Marshal(request)
		text := string(encoded)
		coordinator := false
		for _, tool := range request.Tools {
			if tool.Function.Name == "agent" || tool.Function.Name == "finish" || tool.Name == "agent" || tool.Name == "finish" {
				coordinator = true
			}
		}
		tool, arguments := "submit_result", `{"status":"success","summary":"fresh accepted analysis","details":"new cycle output"}`
		if coordinator {
			if strings.Contains(text, historicalProcessBody) {
				t.Error("coordinator received a historical source body")
			}
			switch coordinatorCalls.Add(1) {
			case 1:
				tool, arguments = "agent", `{"tasks":[{"agent":"researcher","goal":"Prepare a fresh analysis from the authorized reference.","contract_id":"prepare-reference"}]}`
			case 2:
				tool, arguments = "agent", `{"tasks":[{"agent":"verifier","goal":"Verify the fresh analysis.","contract_id":"verify-analysis"}]}`
			default:
				tool, arguments = "finish", `{"response":"new work completed"}`
			}
		} else if strings.Contains(text, "Verifier current-cycle worker") {
			verifierCalls.Add(1)
			if strings.Contains(text, historicalProcessBody) {
				t.Error("ungranted verifier received a historical source body")
			}
		} else {
			researcherCalls.Add(1)
			if strings.Contains(text, historicalProcessBody) != expectSource.Load() || (expectSource.Load() && !strings.Contains(text, "9007199254740993")) {
				t.Error("worker request lost or inherited an unauthorized historical presentation")
			}
			if strings.HasPrefix(request.Model, "test-") {
				alternateCalls.Add(1)
			}
			if variant == "fallback" && request.Model == "test" && rateLimited.CompareAndSwap(false, true) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"historical\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-%d\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"historical\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", requests.Load(), tool, arguments)
	}))
	t.Cleanup(server.Close)
	f := historicalRunProcessSetup(t, server.URL, false, variant)
	binary := buildTestBinary(t)
	output, err, diagnostic := f.run(t, binary, []string{"--inherit-result", "researcher=" + f.resultID, "--inherit-result", "observer=" + f.resultID}, "Review the authorized evidence as a new request.")
	wantFirst, wantTotal := int64(1), int64(2)
	if variant == "fallback" {
		wantFirst, wantTotal = 2, 3
	} else if variant == "extra_model" {
		wantFirst, wantTotal = 2, 4
	}
	if err != nil || output.Outcome != "completed" || !output.GoalSatisfied || len(output.Teams) != 1 || researcherCalls.Load() != wantFirst || verifierCalls.Load() != 1 {
		t.Fatalf("historical CLI run: %v, researcher=%d verifier=%d\n%s", err, researcherCalls.Load(), verifierCalls.Load(), diagnostic)
	}
	if variant != "single" && alternateCalls.Load() != 1 {
		t.Fatalf("authorized alternate model calls = %d", alternateCalls.Load())
	}
	states := make(map[string]string)
	for _, grant := range output.Teams[0].ContextRouting.HistoricalGrants {
		states[grant.Grant.Recipient] = grant.State
	}
	if states["researcher"] != "dispatched" || states["observer"] != "not_dispatched" {
		t.Fatalf("historical grant usage = %v, evidence error=%q, requests=%d", states, output.Teams[0].ContextRouting.HistoricalGrantsError, output.Teams[0].ContextRouting.Requests)
	}
	coordinatorCalls.Store(0)
	expectSource.Store(false)
	output, err, diagnostic = f.run(t, binary, nil, "Start another independent follow-up.")
	if err != nil || len(output.Teams) != 1 || researcherCalls.Load() != wantTotal || verifierCalls.Load() != 2 || len(output.Teams[0].ContextRouting.HistoricalGrants) != 0 {
		t.Fatalf("follow-up inherited the prior grant: %v\n%s", err, diagnostic)
	}
	store, err := team.OpenEventStoreReadOnly(f.control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	foundSource := false
	for _, event := range events {
		if event.ID == f.sourceEventID {
			foundSource = true
			if event.Hash != f.sourceHash {
				t.Fatal("follow-up rewrote the original accepted result")
			}
		}
	}
	if !foundSource {
		t.Fatal("follow-up lost the original accepted result")
	}
}

func TestHistoricalResultRunBinaryRefusesBeforeModelStartup(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 32768","model_info":{"general.architecture":"qwen2","qwen2.context_length":32768},"capabilities":["completion","tools"]}`)
	}))
	t.Cleanup(server.Close)
	f := historicalRunProcessSetup(t, server.URL, true, "single")
	binary := buildTestBinary(t)
	for _, test := range []struct{ name, binding, prompt, diagnostic string }{
		{name: "unknown_recipient", binding: "unknown=" + f.resultID, prompt: "new request", diagnostic: "resolve historical result recipient"},
		{name: "coordinator", binding: "coordinator=" + f.resultID, prompt: "new request", diagnostic: "resolve historical result recipient"},
		{name: "invisible_source", binding: "researcher=res-" + strings.Repeat("0", 64), prompt: "new request", diagnostic: "not visible"},
		{name: "direct_agent", binding: "researcher=" + f.resultID, prompt: "@researcher new request", diagnostic: "does not support direct-agent"},
		{name: "pipeline", binding: "researcher=" + f.resultID, prompt: "first request | @verifier second request", diagnostic: "does not support direct-agent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := requests.Load()
			_, err, diagnostic := f.run(t, binary, []string{"--inherit-result", test.binding}, test.prompt)
			if err == nil || requests.Load() != before || !strings.Contains(diagnostic, test.diagnostic) {
				t.Fatalf("rejected intake reached provider/semantic startup: %v, requests=%d\n%s", err, requests.Load()-before, diagnostic)
			}
		})
	}
}
