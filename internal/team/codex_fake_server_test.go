package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
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
	ThreadCWD                string                  `json:"thread_cwd,omitempty"`
	TurnDirectoryReplacement string                  `json:"turn_directory_replacement,omitempty"`
	ImageHashes              []string                `json:"image_hashes,omitempty"`
	Result                   json.RawMessage         `json:"result,omitempty"`
	ErrorMessage             string                  `json:"error_message,omitempty"`
	ErrorData                json.RawMessage         `json:"error_data,omitempty"`
	DetachedChildTermWrite   bool                    `json:"detached_child_term_write,omitempty"`
	DetachedChildReady       string                  `json:"detached_child_ready,omitempty"`
	StderrOnEOF              string                  `json:"stderr_on_eof,omitempty"`
	RawLine                  string                  `json:"raw_line,omitempty"`
	AfterResponseLine        string                  `json:"after_response_line,omitempty"`
	ReplyGate                string                  `json:"reply_gate,omitempty"`
	NoResponse               bool                    `json:"no_response,omitempty"`
	CloseAfter               bool                    `json:"close_after,omitempty"`
	CloseAfterResponse       bool                    `json:"close_after_response,omitempty"`
	Notifications            []fakeCodexNotification `json:"notifications,omitempty"`
	AfterResponse            []fakeCodexNotification `json:"after_response,omitempty"`
	// WriteFile, when set, is written (relative to the server process's own
	// CWD, i.e. the prepared workspace root) before this step's response —
	// simulating a real coding agent's side effect happening-before a
	// subsequent crash/response, with no test timing race.
	WriteFile         string `json:"write_file,omitempty"`
	WriteFileContent  string `json:"write_file_content,omitempty"`
	SymlinkPath       string `json:"symlink_path,omitempty"`
	SymlinkTarget     string `json:"symlink_target,omitempty"`
	ReplacementRoot   string `json:"replacement_root,omitempty"`
	LinkToReplacement bool   `json:"link_to_replacement,omitempty"`
}

// fakeCodexScript is the whole scripted program: steps plus where to log
// each received request's method, in arrival order, so a test can assert on
// the exact call sequence (e.g. "thread/resume, not thread/start" or "no
// turn/start after a persist failure").
type fakeCodexScript struct {
	ImageProtocolVersion string          `json:"image_protocol_version,omitempty"`
	LogPath              string          `json:"log_path,omitempty"`
	Steps                []fakeCodexStep `json:"steps"`
	// RawLogPath, when set, additionally records each inbound request's full
	// raw JSON line (not just its method) — used by tests that need to
	// assert on a specific request's params (e.g. the sandbox value a
	// result-repair resume actually sent), without changing LogPath's
	// existing method-only format that other tests already depend on.
	RawLogPath string `json:"raw_log_path,omitempty"`
}

// runFakeCodexAppServer is the fake server's entire program: read the
// script, then react to each incoming JSON-RPC request in order. It never
// returns except by os.Exit (TestMain calls it then exits).
const fakeCodexDetachedChildArg = "--fixture-codex-detached-child"

func runFakeCodexAppServer(scriptPath string) {
	if len(os.Args) == 4 && os.Args[1] == fakeCodexDetachedChildArg {
		runFakeCodexDetachedChild(os.Args[2], os.Args[3] == "true")
		return
	}
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
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		version := script.ImageProtocolVersion
		if version == "" {
			version = codexImageProtocolVersion
		}
		fmt.Fprintln(os.Stdout, "codex-cli "+version)
		os.Exit(0)
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
	var rawLogFile *os.File
	if script.RawLogPath != "" {
		rawLogFile, err = os.OpenFile(script.RawLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake codex server: open raw log:", err)
			os.Exit(1)
		}
		defer rawLogFile.Close()
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	out := os.Stdout
	// A single RunAttempt call may start a second app-server process after
	// stopping the first (e.g. §23's result-only repair, or a resumed
	// process after a restart) — a fresh OS process re-executing this same
	// script from disk, with no memory of what the prior process already
	// consumed. RawLogPath persists across that process boundary (it is
	// opened for append, on the same file, by every process reading this
	// script), so its already-written line count is the durable cursor: a
	// second process continues from where the first left off instead of
	// restarting the same steps from index 0.
	stepIndex := 0
	var stderrOnEOF string
	if script.RawLogPath != "" {
		if data, err := os.ReadFile(script.RawLogPath); err == nil {
			if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 {
				stepIndex = bytes.Count(trimmed, []byte("\n")) + 1
			}
		}
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				CWD string `json:"cwd"`
			} `json:"params"`
		}
		_ = json.Unmarshal(line, &req)
		if logFile != nil {
			fmt.Fprintln(logFile, req.Method)
		}
		if rawLogFile != nil {
			rawLogFile.Write(line)
			rawLogFile.Write([]byte("\n"))
		}

		if stepIndex >= len(steps) {
			continue
		}
		step := steps[stepIndex]
		stepIndex++
		if step.StderrOnEOF != "" {
			stderrOnEOF = step.StderrOnEOF
		}

		if step.ImageHashes != nil {
			if err := verifyFakeCodexImages(line, step.ImageHashes); err != nil {
				writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
				continue
			}
		}
		if step.WriteFile != "" {
			if err := os.WriteFile(step.WriteFile, []byte(step.WriteFileContent), 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "fake codex server: write file:", err)
			}
		}
		if step.SymlinkPath != "" {
			if err := os.Symlink(step.SymlinkTarget, step.SymlinkPath); err != nil {
				fmt.Fprintln(os.Stderr, "fake codex server: create symlink:", err)
				os.Exit(1)
			}
		}
		if step.ReplacementRoot != "" {
			if err := replaceFakeCodexRoot(step.ReplacementRoot, step.LinkToReplacement); err != nil {
				fmt.Fprintln(os.Stderr, "fake codex server: replace root:", err)
				os.Exit(1)
			}
		}
		if step.TurnDirectoryReplacement != "" {
			if err := checkFakeCodexTurnDirectory(req.Params.CWD, step.TurnDirectoryReplacement); err != nil {
				writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": jsonRPCError{Code: -32000, Message: err.Error()}})
				continue
			}
		}
		if step.DetachedChildReady != "" {
			if err := startFakeCodexDetachedChild(step.DetachedChildReady, step.DetachedChildTermWrite); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		if step.ReplyGate != "" {
			if err := awaitFakeCodexReplyGate(step.ReplyGate); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
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
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": jsonRPCError{Code: -32000, Message: step.ErrorMessage, Data: step.ErrorData}})
		default:
			result := step.Result
			if result == nil {
				result = json.RawMessage(`{}`)
			}
			if step.ThreadCWD != "" && sameFakeCodexDirectory(req.Params.CWD, step.ThreadCWD) {
				var response map[string]any
				_ = json.Unmarshal(result, &response)
				response["cwd"] = req.Params.CWD
				result, _ = json.Marshal(response)
			}
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		for _, notif := range step.AfterResponse {
			writeFakeLine(out, map[string]any{"jsonrpc": "2.0", "method": notif.Method, "params": notif.Params})
		}
		if step.AfterResponseLine != "" {
			_, _ = out.WriteString(step.AfterResponseLine + "\n")
		}
		if step.CloseAfterResponse {
			return
		}
	}
	if stderrOnEOF != "" {
		fmt.Fprint(os.Stderr, stderrOnEOF)
	}
}

func sameFakeCodexDirectory(first, second string) bool {
	firstInfo, firstErr := os.Stat(first)
	secondInfo, secondErr := os.Stat(second)
	return firstErr == nil && secondErr == nil && os.SameFile(firstInfo, secondInfo)
}

func checkFakeCodexTurnDirectory(cwd, replacement string) (err error) {
	original, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := replaceFakeCodexRoot(replacement, false); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, os.Rename(original, replacement), os.Rename(original+"-provider-original", original))
	}()
	if !sameFakeCodexDirectory(cwd, ".") {
		return errors.New("turn cwd resolved outside the admitted directory")
	}
	return nil
}

func replaceFakeCodexRoot(replacement string, link bool) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.Rename(root, root+"-provider-original"); err != nil {
		return err
	}
	if link {
		return os.Symlink(replacement, root)
	}
	return os.Rename(replacement, root)
}

func awaitFakeCodexReplyGate(path string) error {
	if err := os.WriteFile(path+".waiting", nil, 0o600); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for scripted response release")
}

// Nil standard streams keep the child in the inherited process group
// without prolonging any app-server protocol pipe.
func startFakeCodexDetachedChild(readyPath string, termWrite bool) error {
	child := exec.Command(os.Args[0], fakeCodexDetachedChildArg, readyPath, strconv.FormatBool(termWrite))
	if err := child.Start(); err != nil {
		return err
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if data, _ := os.ReadFile(readyPath); data != nil && len(bytes.TrimSpace(data)) > 0 {
			return child.Process.Release()
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	return fmt.Errorf("fake codex descendant did not become ready")
}

func runFakeCodexDetachedChild(readyPath string, termWrite bool) {
	terminate := make(chan os.Signal, 1)
	if termWrite {
		signal.Notify(terminate, syscall.SIGTERM)
		defer signal.Stop(terminate)
	} else {
		signal.Ignore(syscall.SIGTERM, os.Interrupt)
	}
	if err := os.WriteFile(readyPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(1)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-terminate:
			if err := os.WriteFile("descendant-termination.txt", []byte("final child effect"), 0o600); err != nil {
				os.Exit(1)
			}
			return
		case <-poll.C:
			if _, err := os.Stat(readyPath + ".release"); err == nil {
				if err := os.WriteFile("during-late-repair.txt", []byte("unattributed write"), 0o600); err != nil {
					os.Exit(1)
				}
				return
			}
		case <-timer.C:
			return
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
	return startFakeCodexServerWithReadLimits(t, steps, maxFrameBytes, codexMaxAdvisoryQueueBytes)
}

func startFakeCodexServerWithReadLimits(t *testing.T, steps []fakeCodexStep, maxFrameBytes int, advisoryQueueBytes int64) *fakeCodexServer {
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

	client := newCodexRPCClient(stdin, stdout, maxFrameBytes, advisoryQueueBytes, true)
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

// The separate app-server process reads and decodes every supplied image.
// Merely observing a localImage tag in the parent is insufficient wire proof.
func verifyFakeCodexImages(line []byte, hashes []string) error {
	var request struct {
		Method string `json:"method"`
		Params struct {
			Input []codexUserInput `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return err
	}
	if request.Method != codexMethodTurnStart || len(request.Params.Input) != len(hashes)+1 || request.Params.Input[0].Type != "text" {
		return fmt.Errorf("unexpected Codex native image input shape")
	}
	for index, hash := range hashes {
		input := request.Params.Input[index+1]
		if input.Type != "localImage" || input.Text != "" {
			return fmt.Errorf("expected localImage")
		}
		info, err := os.Lstat(input.Path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
			return fmt.Errorf("projection is not a private readonly image")
		}
		data, err := media.ReadFile(context.Background(), input.Path, media.Limits{})
		if err != nil {
			return err
		}
		ref, err := media.Validate(context.Background(), data, media.Limits{})
		clear(data)
		if err != nil || ref.SHA256 != hash {
			return fmt.Errorf("app-server received wrong image bytes")
		}
	}
	return nil
}
