package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// buildProcessContractBinary builds the real CLI so these tests exercise
// cobra, main's error handling, process exit status, and output streams
// together rather than calling an in-process helper.
func buildProcessContractBinary(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	binary := filepath.Join(t.TempDir(), "hufu-contract")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/hufu")
	cmd.Dir = moduleRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI binary: %v\n%s", err, output)
	}
	return binary
}

func runProcessContract(t *testing.T, binary string, args ...string) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	home := t.TempDir()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"TERM=dumb",
		"NO_COLOR=1",
		"HUFU_AGENT_TEAM_SEARCH_PATH=" + filepath.Join(home, ".agent-teams"),
		"PATH=" + os.Getenv("PATH"),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("CLI process timed out: %v\nstderr:\n%s", ctx.Err(), stderr.String())
	}
	if err == nil {
		return 0, stdout.Bytes(), stderr.Bytes()
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("CLI process failed without an exit status: %v\nstderr:\n%s", err, stderr.String())
	}
	return exitErr.ExitCode(), stdout.Bytes(), stderr.Bytes()
}

type contractChatRequest struct {
	Stream   bool `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type contractRequestPurpose string

const (
	contractPurposeWorker      contractRequestPurpose = "worker"
	contractPurposeAuxiliary   contractRequestPurpose = "auxiliary"
	contractPurposeCoordinator contractRequestPurpose = "coordinator"
)

func classifyContractRequest(request contractChatRequest) contractRequestPurpose {
	for _, tool := range request.Tools {
		name := tool.Name
		if name == "" {
			name = tool.Function.Name
		}
		if name == "finish" {
			return contractPurposeCoordinator
		}
	}

	var content strings.Builder
	for _, message := range request.Messages {
		content.WriteString(message.Role)
		content.WriteByte(' ')
		content.Write(message.Content)
		content.WriteByte('\n')
	}
	text := strings.ToLower(content.String())
	if strings.Contains(text, "compactor") || strings.Contains(text, "compress") || strings.Contains(text, "context") {
		return contractPurposeAuxiliary
	}
	return contractPurposeWorker
}

func TestCLIProcessExitContract(t *testing.T) {
	binary := buildProcessContractBinary(t)

	t.Run("successful command", func(t *testing.T) {
		code, stdout, stderr := runProcessContract(t, binary, "--version")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(string(stdout), "hufu version") {
			t.Fatalf("stdout = %q, want version output", stdout)
		}
		if len(stderr) != 0 {
			t.Fatalf("stderr = %q, want empty for successful command", stderr)
		}
	})

	t.Run("failed run emits JSON and nonzero exit", func(t *testing.T) {
		code, stdout, stderr := runProcessContract(t, binary,
			"--default", "--temp", "--unattended", "--model", "test",
			"--coordinator-model", "test",
			"--provider-url", "http://127.0.0.1:1", "--output", "json",
			"--max-rounds", "1", "--max-steps", "1", "--max-duration", "2",
			"contract failure")
		if code != 1 {
			t.Fatalf("exit code = %d, want 1; stdout=%q stderr prefix=%q", code, stdout, truncateContractOutput(stderr))
		}
		var output jsonRunOutput
		if err := json.Unmarshal(stdout, &output); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		if output.Outcome != string(team.RunOutcomeFailed) || output.GoalSatisfied {
			t.Fatalf("JSON result = outcome=%q goal_satisfied=%t, want failed/false", output.Outcome, output.GoalSatisfied)
		}
		if output.ExitCode != 1 {
			t.Fatalf("JSON exit_code = %d, want 1", output.ExitCode)
		}
		if output.Acceptance == nil || output.Acceptance.State != team.AcceptanceNotConfigured || output.Acceptance.Passed {
			t.Fatalf("JSON acceptance = %#v, want not_configured/not-passed", output.Acceptance)
		}
		if bytes.Contains(stderr, []byte("\"outcome\"")) {
			t.Fatalf("structured JSON leaked to stderr:\n%s", stderr)
		}
	})

	t.Run("acceptance failure emits partial JSON and exit 7", func(t *testing.T) {
		var coordinatorResponseSent atomic.Bool
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			var request contractChatRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Typed worker results are reduced by the runtime; the coordinator can
			// proceed directly to finish without a model-owned memory mutation.
			toolName := ""
			arguments := `{"response":"finish fixture"}`
			if classifyContractRequest(request) == contractPurposeCoordinator && !coordinatorResponseSent.Swap(true) {
				toolName = "finish"
			}
			if request.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, ok := w.(http.Flusher)
				if !ok {
					http.Error(w, "streaming unsupported", http.StatusInternalServerError)
					return
				}
				if toolName == "" {
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture worker completed\"},\"finish_reason\":null}]}\n\n")
				} else {
					fmt.Fprintf(w, "data: %s\n\n", `{"id":"fixture","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"fixture-call","type":"function","function":{"name":"`+toolName+`","arguments":"`+strings.ReplaceAll(arguments, `"`, `\"`)+`"}}]},"finish_reason":null}]} `)
				}
				fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			response := map[string]any{
				"id": "fixture", "object": "chat.completion", "created": 1, "model": "test",
				"choices": []any{map[string]any{
					"index": 0,
					"finish_reason": func() string {
						if toolName == "" {
							return "stop"
						}
						return "tool_calls"
					}(),
					"message": func() map[string]any {
						if toolName == "" {
							return map[string]any{"role": "assistant", "content": "fixture worker completed"}
						}
						return map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
							"id": "fixture-call", "type": "function", "function": map[string]string{"name": toolName, "arguments": arguments},
						}}}
					}(),
				}},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			}
			_ = json.NewEncoder(w).Encode(response)
		})
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			if errors.Is(err, syscall.EPERM) {
				t.Skipf("sandbox does not permit TCP fixture listener: %v", err)
			}
			t.Fatalf("start TCP fixture listener: %v", err)
		}
		server := httptest.NewUnstartedServer(handler)
		server.Listener = listener
		server.Start()
		defer server.Close()

		// Auxiliary model calls may occur before coordinator execution. They must
		// receive a deterministic non-terminal response without consuming the
		// coordinator's one-shot finish response.
		auxiliaryResponse, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"test","stream":false,"messages":[{"role":"system","content":"compactor"},{"role":"user","content":"Compress this context."}]}`))
		if err != nil {
			t.Fatalf("make auxiliary fixture request: %v", err)
		}
		defer auxiliaryResponse.Body.Close()
		if auxiliaryResponse.StatusCode != http.StatusOK {
			t.Fatalf("auxiliary fixture status = %d, want 200", auxiliaryResponse.StatusCode)
		}
		var auxiliaryCompletion struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(auxiliaryResponse.Body).Decode(&auxiliaryCompletion); err != nil {
			t.Fatalf("decode auxiliary fixture response: %v", err)
		}
		if len(auxiliaryCompletion.Choices) != 1 || auxiliaryCompletion.Choices[0].FinishReason != "stop" {
			t.Fatalf("auxiliary fixture response = %#v, want one stop completion", auxiliaryCompletion)
		}

		teamRoot := t.TempDir()
		teamDir := filepath.Join(teamRoot, "acceptance-fixture")
		if err := os.MkdirAll(teamDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: acceptance-fixture\nmodel: test\nprovider-url: "+server.URL+"/v1\ncontext-window: 32768\nacceptance: \"false\"\nmax-rounds: 1\ntimeout: 10\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte("---\nname: coordinator\nrole: coordinator\ntools: ask_user\n---\nCall finish when the request is complete.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Two workers force the CLI through the coordinator path instead of
		// its single-worker fast path; the fixture then exercises finish and
		// the acceptance evaluator in the real child process.
		for _, name := range []string{"worker", "reviewer"} {
			content := fmt.Sprintf("---\nname: %s\nrole: worker\ntools: ask_user\n---\nReturn a concise completion summary.\n", name)
			if err := os.WriteFile(filepath.Join(teamDir, name+".md"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		code, stdout, stderr := runProcessContract(t, binary,
			"--agent-team", "acceptance-fixture",
			"--agent-team-search-path", teamRoot,
			"--workspace", filepath.Join(t.TempDir(), "workspace"),
			"--provider-url", server.URL+"/v1", "--model", "test", "--output", "json",
			"--max-rounds", "1", "--timeout", "10", "acceptance contract")
		if code != 7 {
			t.Fatalf("exit code = %d, want 7; stdout=%q stderr=%q", code, stdout, truncateContractOutput(stderr))
		}
		var output jsonRunOutput
		if err := json.Unmarshal(stdout, &output); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		if output.Outcome != string(team.RunOutcomePartial) || output.GoalSatisfied {
			t.Fatalf("JSON result = outcome=%q goal_satisfied=%t, want partial/false", output.Outcome, output.GoalSatisfied)
		}
		if output.ExitCode != 7 {
			t.Fatalf("JSON exit_code = %d, want 7", output.ExitCode)
		}
		if output.Acceptance == nil || output.Acceptance.State != team.AcceptanceFailed || output.Acceptance.Passed {
			t.Fatalf("JSON acceptance = %#v, want failed/not-passed", output.Acceptance)
		}
	})
}

func TestCanonicalRunMatchesLegacyExecutionEffects(t *testing.T) {
	binary := buildProcessContractBinary(t)
	var chatCalls atomic.Int64
	server := newContractTextServer(t, &chatCalls)
	defer server.Close()

	teamRoot := t.TempDir()
	teamName := "equivalent-run"
	teamDir := filepath.Join(teamRoot, teamName)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: %s\nmodel: test\nprovider-url: %s/v1\ncontext-window: 32768\nmax-rounds: 2\ntimeout: 10\n", teamName, server.URL)
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyRoot := t.TempDir()
	legacyWorkspace := filepath.Join(legacyRoot, teamName)
	legacyCode, legacyStdout, legacyStderr := runProcessContract(t, binary,
		"--agent-team", teamName, "--agent-team-search-path", teamRoot,
		"--workspace", legacyRoot, "--provider-url", server.URL+"/v1", "--model", "test",
		"--context-window", "32768", "--route", "fast", "--output", "json", "--timeout", "10", "calculate")
	legacyCalls := chatCalls.Load()

	canonicalWorkspace := filepath.Join(t.TempDir(), "exact-workspace")
	canonicalCode, canonicalStdout, canonicalStderr := runProcessContract(t, binary,
		"run", "--team", teamName, "--agent-team-search-path", teamRoot,
		"--workspace", canonicalWorkspace, "--provider-url", server.URL+"/v1", "--model", "test",
		"--context-window", "32768", "--route", "fast", "--output", "json", "--timeout", "10", "--", "calculate")
	canonicalCalls := chatCalls.Load() - legacyCalls
	if canonicalCode != legacyCode {
		t.Fatalf("exit codes differ: legacy=%d canonical=%d\nlegacy stderr=%s\ncanonical stderr=%s", legacyCode, canonicalCode, legacyStderr, canonicalStderr)
	}
	if legacyCalls == 0 || canonicalCalls != legacyCalls {
		t.Fatalf("provider calls legacy=%d canonical=%d", legacyCalls, canonicalCalls)
	}

	var legacyOutput, canonicalOutput jsonRunOutput
	if err := json.Unmarshal(legacyStdout, &legacyOutput); err != nil {
		t.Fatalf("decode legacy output: %v\n%s", err, legacyStdout)
	}
	if err := json.Unmarshal(canonicalStdout, &canonicalOutput); err != nil {
		t.Fatalf("decode canonical output: %v\n%s", err, canonicalStdout)
	}
	if legacyOutput.Outcome != canonicalOutput.Outcome || legacyOutput.GoalSatisfied != canonicalOutput.GoalSatisfied || legacyOutput.Result != canonicalOutput.Result || legacyOutput.ExitCode != canonicalOutput.ExitCode {
		t.Fatalf("run results differ: legacy=%#v canonical=%#v", legacyOutput, canonicalOutput)
	}

	legacyEffects := contractExecutionEffects(t, legacyWorkspace)
	canonicalEffects := contractExecutionEffects(t, canonicalWorkspace)
	if !slices.Equal(legacyEffects, canonicalEffects) {
		t.Fatalf("durable effects differ:\nlegacy=%v\ncanonical=%v", legacyEffects, canonicalEffects)
	}
	if !slices.Contains(legacyEffects, "task_completed:receipt") {
		t.Fatalf("equivalent scenario did not persist an execution receipt: %v", legacyEffects)
	}
}

func TestEventFormatJSONLStderrContainsOnlyStatusEvents(t *testing.T) {
	binary := buildProcessContractBinary(t)
	var chatCalls atomic.Int64
	server := newContractTextServer(t, &chatCalls)
	defer server.Close()

	teamRoot := t.TempDir()
	teamName := "jsonl-run"
	teamDir := filepath.Join(teamRoot, teamName)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("name: %s\nmodel: test\nprovider-url: %s/v1\ncontext-window: 32768\nmax-rounds: 2\ntimeout: 10\n", teamName, server.URL)
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runProcessContract(t, binary,
		"run", "--team", teamName, "--agent-team-search-path", teamRoot,
		"--workspace", filepath.Join(t.TempDir(), "workspace"),
		"--provider-url", server.URL+"/v1", "--model", "test",
		"--context-window", "32768", "--route", "fast", "--output", "json",
		"--event-format", "jsonl", "--timeout", "10", "--", "calculate")
	if code == 0 {
		t.Fatalf("fixture unexpectedly completed; want a nonzero final outcome to exercise command error JSONL\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	var output jsonRunOutput
	if err := json.Unmarshal(stdout, &output); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}

	lines := bytes.Split(bytes.TrimSpace(stderr), []byte{'\n'})
	if len(lines) < 2 {
		t.Fatalf("stderr JSONL lines = %d, want runtime events plus command error\nstderr=%s", len(lines), stderr)
	}
	foundCommandError := false
	for index, line := range lines {
		var event jsonStatusEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("stderr line %d is not a JSON status event: %v\nline=%q\nall stderr=%s", index+1, err, line, stderr)
		}
		if event.Type == "" || event.Time == "" {
			t.Fatalf("stderr line %d lacks type/time: %#v", index+1, event)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Time); err != nil {
			t.Fatalf("stderr line %d time = %q: %v", index+1, event.Time, err)
		}
		if event.Type == "error" && strings.Contains(event.Message, "task") {
			foundCommandError = true
		}
	}
	if !foundCommandError {
		t.Fatalf("stderr lacks structured command-boundary error:\n%s", stderr)
	}
}

func newContractTextServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(writer, `{"object":"list","data":[{"id":"test","object":"model"}]}`)
			return
		case "/v1/chat/completions":
		default:
			http.NotFound(writer, request)
			return
		}
		var chat contractChatRequest
		if err := json.NewDecoder(request.Body).Decode(&chat); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		useSubmitResult := false
		for _, tool := range chat.Tools {
			name := tool.Name
			if name == "" {
				name = tool.Function.Name
			}
			if name == "submit_result" {
				useSubmitResult = true
				break
			}
		}
		arguments := `{"status":"success","summary":"fixture verified result"}`
		if chat.Stream {
			writer.Header().Set("Content-Type", "text/event-stream")
			if useSubmitResult {
				_, _ = fmt.Fprintf(writer, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-%d\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", call, arguments)
				_, _ = fmt.Fprint(writer, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			} else {
				_, _ = fmt.Fprint(writer, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture verified result\"},\"finish_reason\":null}]}\n\n")
				_, _ = fmt.Fprint(writer, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			}
			_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
			return
		}
		message := map[string]any{"role": "assistant", "content": "fixture verified result"}
		finishReason := "stop"
		if useSubmitResult {
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": fmt.Sprintf("submit-%d", call), "type": "function",
				"function": map[string]string{"name": "submit_result", "arguments": arguments},
			}}}
			finishReason = "tool_calls"
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id": "fixture", "object": "chat.completion", "created": 1, "model": "test",
			"choices": []any{map[string]any{"index": 0, "finish_reason": finishReason, "message": message}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit TCP fixture listener: %v", err)
		}
		t.Fatalf("start TCP fixture listener: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func contractExecutionEffects(t *testing.T, workspace string) []string {
	t.Helper()
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	effects := make([]string, 0, len(events))
	for _, event := range events {
		signature := event.Type
		if event.Type == string(team.EventTaskCompleted) {
			var payload struct {
				ExecutionReceipt *team.ExecutionReceipt `json:"execution_receipt"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ExecutionReceipt != nil {
				signature += ":receipt"
			}
		}
		effects = append(effects, signature)
	}
	return effects
}

func truncateContractOutput(data []byte) string {
	const max = 5000
	if len(data) <= max {
		return string(data)
	}
	return fmt.Sprintf("%s…", data[:max])
}
