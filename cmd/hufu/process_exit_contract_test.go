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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
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
			"--default", "--unattended", "--model", "test",
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
		var requestNumber atomic.Int32
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				http.NotFound(w, r)
				return
			}
			var request struct {
				Stream bool `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// The coordinator contract requires stm_write immediately before
			// finish. Make the fixture complete that normal wrap-up sequence so
			// this test reaches the real acceptance gate rather than testing an
			// agent/tool-loop failure.
			toolName := "finish"
			arguments := `{"response":"finish fixture"}`
			requestIndex := requestNumber.Add(1)
			if requestIndex == 1 {
				toolName = ""
			} else if requestIndex == 2 {
				toolName = "stm_write"
				arguments = `{"content":"fixture completed the request","mode":"append"}`
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

		teamRoot := t.TempDir()
		teamDir := filepath.Join(teamRoot, "acceptance-fixture")
		if err := os.MkdirAll(teamDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: acceptance-fixture\nmodel: test\nprovider-url: "+server.URL+"/v1\nacceptance: \"false\"\nmax-rounds: 1\ntimeout: 10\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte("---\nname: coordinator\nrole: coordinator\ntools: ask_user\n---\nCall finish when the request is complete.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Two workers force the CLI through the coordinator path instead of
		// its single-worker fast path; the fixture then exercises finish and
		// the acceptance evaluator in the real child process.
		for _, name := range []string{"helper", "reviewer"} {
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

func truncateContractOutput(data []byte) string {
	const max = 5000
	if len(data) <= max {
		return string(data)
	}
	return fmt.Sprintf("%s…", data[:max])
}
