package team

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file implements the "fake app-server subprocess fixture" spec.md §36
// PR-08 asks for: a real, separate OS process (this same test binary,
// re-exec'd — see TestMain in main_test.go) speaking JSON-RPC over its own
// stdin/stdout, scripted per test. Every Phase 4 test (PR-08/09/10) drives a
// real subprocess through this fixture; none use an in-process fake.

const fakeCodexServerScriptEnvVar = "HUFU_TEST_FAKE_CODEX_SERVER_SCRIPT"

// fakeCodexNotification is one notification to emit.
type fakeCodexNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// fakeCodexStep describes how the fake server reacts to the Nth request it
// receives, in strict arrival order (regardless of method name — each test
// constructs its own client call sequence deterministically). Exactly one
// of Result/ErrorMessage/RawLine/NoResponse/CloseAfter applies.
type fakeCodexStep struct {
	Result        json.RawMessage         `json:"result,omitempty"`
	ErrorMessage  string                  `json:"error_message,omitempty"`
	RawLine       string                  `json:"raw_line,omitempty"`
	NoResponse    bool                    `json:"no_response,omitempty"`
	CloseAfter    bool                    `json:"close_after,omitempty"`
	Notifications []fakeCodexNotification `json:"notifications,omitempty"`
}

// fakeCodexScript is the whole scripted program: steps plus where to log
// each received request's method, in arrival order, so a test can assert on
// the exact call sequence (e.g. "thread/resume, not thread/start" or "no
// turn/start after a persist failure").
type fakeCodexScript struct {
	LogPath string          `json:"log_path,omitempty"`
	Steps   []fakeCodexStep `json:"steps"`
}

// runFakeCodexAppServer is the fake server's entire program: read the
// script, then react to each incoming JSON-RPC request in order. It never
// returns except by os.Exit (TestMain calls it then exits).
func runFakeCodexAppServer(scriptPath string) {
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex server: read script:", err)
		os.Exit(1)
	}
	var script fakeCodexScript
	if err := json.Unmarshal(data, &script); err != nil {
		fmt.Fprintln(os.Stderr, "fake codex server: decode script:", err)
		os.Exit(1)
	}
	steps := script.Steps

	var logFile *os.File
	if script.LogPath != "" {
		logFile, err = os.OpenFile(script.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake codex server: open log:", err)
			os.Exit(1)
		}
		defer logFile.Close()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	out := os.Stdout
	stepIndex := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(line, &req)
		if logFile != nil {
			fmt.Fprintln(logFile, req.Method)
		}

		if stepIndex >= len(steps) {
			continue
		}
		step := steps[stepIndex]
		stepIndex++

		for _, notif := range step.Notifications {
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "method": notif.Method, "params": notif.Params})
		}
		switch {
		case step.RawLine != "":
			_, _ = out.WriteString(step.RawLine)
			_, _ = out.WriteString("\n")
		case step.CloseAfter:
			os.Exit(0)
		case step.NoResponse:
			// Deliberately never respond to this request; keep reading.
		case step.ErrorMessage != "":
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": step.ErrorMessage}})
		default:
			result := step.Result
			if result == nil {
				result = json.RawMessage(`{}`)
			}
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
	}
}

func writeFakeLine(out *os.File, v any) {
	line, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex server: marshal line:", err)
		return
	}
	_, _ = out.Write(line)
	_, _ = out.Write([]byte("\n"))
}

// fakeCodexServer is a started fake app-server subprocess and its client.
type fakeCodexServer struct {
	Client  *CodexRPCClient
	cmd     *exec.Cmd
	logPath string
}

// CallLog returns the methods this server has received so far, in arrival
// order.
func (s *fakeCodexServer) CallLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// startFakeCodexServer spawns this same test binary re-exec'd as a fake
// app-server (via HUFU_TEST_FAKE_CODEX_SERVER_SCRIPT / TestMain), scripted
// with steps, and wires a CodexRPCClient to its real stdin/stdout pipes.
func startFakeCodexServer(t *testing.T, steps []fakeCodexStep) *fakeCodexServer {
	t.Helper()
	return startFakeCodexServerWithFrameBound(t, steps, 0)
}

// startFakeCodexServerWithFrameBound is startFakeCodexServer with an
// explicit maxFrameBytes (0 = CodexRPCClient's default).
func startFakeCodexServerWithFrameBound(t *testing.T, steps []fakeCodexStep, maxFrameBytes int) *fakeCodexServer {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	logPath := filepath.Join(dir, "calls.log")
	data, err := json.Marshal(fakeCodexScript{LogPath: logPath, Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), fakeCodexServerScriptEnvVar+"="+scriptPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	client := NewCodexRPCClient(stdin, stdout, maxFrameBytes)
	return &fakeCodexServer{Client: client, cmd: cmd, logPath: logPath}
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
