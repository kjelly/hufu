package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestRewriteBashRedirects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple redirect",
			input: "echo hello > file.txt",
			want:  "echo hello | tee file.txt",
		},
		{
			name:  "append redirect",
			input: "echo hello >> file.txt",
			want:  "echo hello | tee -a file.txt",
		},
		{
			name:  "no redirect",
			input: "ls -la",
			want:  "ls -la",
		},
		{
			name:  "redirect with path",
			input: "cat input.txt > /tmp/output.txt",
			want:  "cat input.txt | tee /tmp/output.txt",
		},
		{
			name:  "append with path",
			input: "echo log >> /var/log/app.log",
			want:  "echo log | tee -a /var/log/app.log",
		},
		{
			name:  "multiline with redirect",
			input: "echo a\necho b > out.txt\necho c",
			want:  "echo a\necho b | tee out.txt\necho c",
		},
		{
			name:  "complex command with pipe",
			input: "grep pattern file | sort > output.txt",
			want:  "grep pattern file | sort > output.txt",
		},
		{
			name:  "complex command with &&",
			input: "cd dir && echo done > out.txt",
			want:  "cd dir && echo done > out.txt",
		},
		{
			name:  "complex command with ||",
			input: "cmd1 || echo fail > err.txt",
			want:  "cmd1 || echo fail > err.txt",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "redirect with quoted filename containing spaces",
			input: `echo data > "my file.txt"`,
			want:  `echo data > "my file.txt"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteBashRedirects(tt.input)
			if got != tt.want {
				t.Errorf("rewriteBashRedirects(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBashAndSudoRuntimeWorkflowWriteIsolation(t *testing.T) {
	ctx := context.WithValue(context.Background(), AgentAllowedWritePathsKey, []string{"/workspace/runtime"})
	cfg := ToolConfig{}

	tmpDir := t.TempDir()
	tests := []struct {
		name    string
		command string
		target  string
	}{
		{"nonexistent absolute redirection", "printf x > " + filepath.Join(tmpDir, "escape1"), filepath.Join(tmpDir, "escape1")},
		{"relative redirection", "printf x > " + filepath.Join(tmpDir, "escape2"), filepath.Join(tmpDir, "escape2")},
		{"tee absolute", "echo x | tee " + filepath.Join(tmpDir, "escape3"), filepath.Join(tmpDir, "escape3")},
		{"mkdir absolute", "mkdir " + filepath.Join(tmpDir, "escape-dir"), filepath.Join(tmpDir, "escape-dir")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, toolFn := range []func(context.Context, fantasy.ToolCall, ToolConfig) (fantasy.ToolResponse, error){executeBash, executeSudo} {
				resp, err := toolFn(ctx, fantasy.ToolCall{Input: `{"command": "` + tt.command + `"}`}, cfg)
				if err != nil {
					t.Fatalf("tool function returned system error instead of tool error: %v", err)
				}

				if !resp.IsError {
					t.Errorf("expected resp.IsError to be true")
				}

				if !strings.Contains(resp.Content, "disabled in this runtime workflow") {
					t.Errorf("expected tool to be rejected in runtime workflow, got: %s", resp.Content)
				}

				if _, err := os.Stat(tt.target); err == nil {
					t.Errorf("write isolation failed, target was created: %s", tt.target)
				}
			}
		})
	}
}

func TestBashRuntimeWorkflowAllowsOnlyCanonicalBoundedCommand(t *testing.T) {
	ctx := context.WithValue(context.Background(), AgentAllowedWritePathsKey, []string{t.TempDir()})
	ctx = context.WithValue(ctx, WorkflowBoundedBashKey, WorkflowBoundedBash{Command: "printf bounded"})

	resp, err := executeBash(ctx, fantasy.ToolCall{Input: `{"command":"printf bounded"}`}, ToolConfig{})
	if err != nil || resp.IsError || !strings.Contains(resp.Content, "bounded") {
		t.Fatalf("canonical bounded command response=%+v err=%v, want successful bounded output", resp, err)
	}

	resp, err = executeBash(ctx, fantasy.ToolCall{Input: `{"command":"printf escaped"}`}, ToolConfig{})
	if err != nil || !resp.IsError || !strings.Contains(resp.Content, "disabled in this runtime workflow") {
		t.Fatalf("non-canonical bounded command response=%+v err=%v, want workflow isolation denial", resp, err)
	}
}

func TestBashReadScopeDoesNotEnableWorkflowWriteIsolation(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.WithValue(context.Background(), AgentAllowedPathsKey, []string{workDir})
	resp, err := executeBash(ctx, fantasy.ToolCall{Input: `{"command":"pwd","working_directory":"` + workDir + `"}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeBash returned system error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("read-scoped bash was incorrectly treated as write-isolated: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, workDir) {
		t.Fatalf("pwd output = %q, want %q", resp.Content, workDir)
	}
}

func TestBashReadOnlyExecutionRejectsGitWrites(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.WithValue(context.Background(), AgentReadOnlyExecutionKey, true)
	resp, err := executeBash(ctx, fantasy.ToolCall{Input: `{"command":"git stash push -m blocked","working_directory":"` + workDir + `"}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeBash returned system error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "read-only bash policy denied git command") {
		t.Fatalf("git stash write was not rejected: %s", resp.Content)
	}
}

func TestBashReadOnlyExecutionReportsPolicyDenialBeforeExecution(t *testing.T) {
	workDir := t.TempDir()
	var got []ToolExecutionDisposition
	ctx := context.WithValue(context.Background(), AgentReadOnlyExecutionKey, true)
	ctx = context.WithValue(ctx, ToolExecutionDispositionReporterKey, ToolExecutionDispositionReporter(func(disposition ToolExecutionDisposition) {
		got = append(got, disposition)
	}))
	resp, err := executeBash(ctx, fantasy.ToolCall{ID: "unsafe-shell", Input: `{"command":"go test ./... 2>&1","working_directory":"` + workDir + `"}`}, ToolConfig{})
	if err != nil || !resp.IsError {
		t.Fatalf("unsafe shell response=%+v err=%v, want policy denial", resp, err)
	}
	if len(got) != 1 || got[0].Kind != "policy_denied" || got[0].ReasonCode != "read_only_shell_redirect_denied" || got[0].Executed || got[0].ToolCallID != "unsafe-shell" {
		t.Fatalf("disposition = %+v, want structured pre-execution policy denial", got)
	}
}

func TestBashReadOnlyExecutionPermitsGitDiff(t *testing.T) {
	workDir := t.TempDir()
	ctx := context.WithValue(context.Background(), AgentReadOnlyExecutionKey, true)
	resp, err := executeBash(ctx, fantasy.ToolCall{Input: `{"command":"git diff --stat","working_directory":"` + workDir + `"}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeBash returned system error: %v", err)
	}
	if resp.IsError && strings.Contains(resp.Content, "read-only bash policy") {
		t.Fatalf("read-only git diff was rejected by policy: %s", resp.Content)
	}
}

func TestReadOnlyBashPolicyPermitsQuotedRegexAndEcho(t *testing.T) {
	if err := checkReadOnlyBashCommand(`grep -rn "func (c *Coordinator) Sidecar" internal/team | head -20 && echo done`); err != nil {
		t.Fatalf("safe inspection command was rejected: %v", err)
	}
}

func TestIsReadOnlyBashCommand(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{command: `git diff --stat`, want: true},
		{command: `cd internal/team && grep -rn "submit_result" .`, want: true},
		{command: `touch unsafe.txt`, want: false},
		{command: `cd internal/team && rm unsafe.txt`, want: false},
	}
	for _, tt := range tests {
		if got := IsReadOnlyBashCommand(tt.command); got != tt.want {
			t.Errorf("IsReadOnlyBashCommand(%q) = %t, want %t", tt.command, got, tt.want)
		}
	}
}

func TestRewriteLineRedirects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple redirect",
			input: "echo hello > file.txt",
			want:  "echo hello | tee file.txt",
		},
		{
			name:  "append redirect",
			input: "echo hello >> file.txt",
			want:  "echo hello | tee -a file.txt",
		},
		{
			name:  "no redirect",
			input: "ls -la",
			want:  "ls -la",
		},
		{
			name:  "pipe command not rewritten",
			input: "grep foo | sort > out.txt",
			want:  "grep foo | sort > out.txt",
		},
		{
			name:  "&& command not rewritten",
			input: "cd /tmp && ls > out.txt",
			want:  "cd /tmp && ls > out.txt",
		},
		{
			name:  "|| command not rewritten",
			input: "cmd || echo fail > err.txt",
			want:  "cmd || echo fail > err.txt",
		},
		{
			name:  "redirect with absolute path",
			input: "echo data > /tmp/out.txt",
			want:  "echo data | tee /tmp/out.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteLineRedirects(tt.input)
			if got != tt.want {
				t.Errorf("rewriteLineRedirects(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// A timed-out command must take its whole process group with it: the default
// CommandContext kill reaches only the direct child, so a backgrounded
// grandchild (guestfish appliances, daemonized helpers) survives holding the
// pipes and, in the worst case, blocks the tool forever.
func TestRunShellCommandTimeoutKillsProcessGroup(t *testing.T) {
	marker := fmt.Sprintf("299.%06d", os.Getpid()%1000000)
	cmdStr := fmt.Sprintf("sleep %s & sleep %s", marker, marker)

	start := time.Now()
	resp, err := runShellCommand(context.Background(), 500*time.Millisecond, "", false, "bash", []string{"-c", cmdStr}, nil)
	if err != nil {
		t.Fatalf("runShellCommand error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > commandReapDelay+5*time.Second {
		t.Fatalf("runShellCommand blocked %s after the timeout", elapsed)
	}
	if !strings.Contains(resp.Content, "timed out") {
		t.Fatalf("expected timeout response, got: %s", resp.Content)
	}

	// The backgrounded grandchild must be dead too, not just the direct bash.
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", "sleep "+marker).Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			exec.Command("pkill", "-f", "sleep "+marker).Run()
			t.Fatalf("grandchild survived the timeout kill: pids %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Regression test for a Wait/pipe-read race: Cmd.Wait closes the pipes it
// handed back from StdoutPipe/StderrPipe as soon as it reaps the child (see
// the Cmd.StdoutPipe doc), so calling Wait before the reader goroutines have
// been scheduled at all can truncate a successful command's real output to
// nothing. Repeated iterations are needed because the race window is a
// goroutine-scheduling gap, not something a single run reliably hits.
func TestRunShellCommandDoesNotTruncateOutput(t *testing.T) {
	for i := 0; i < 200; i++ {
		resp, err := runShellCommand(context.Background(), 5*time.Second, "", false, "bash", []string{"-c", "echo hello-world"}, nil)
		if err != nil {
			t.Fatalf("iteration %d: runShellCommand error: %v", i, err)
		}
		if !strings.Contains(resp.Content, "hello-world") {
			t.Fatalf("iteration %d: expected output to contain %q, got: %q", i, "hello-world", resp.Content)
		}
	}
}

func TestRunShellCommandRedactsInheritedSecretEnvironment(t *testing.T) {
	secret := "bash-tool-secret-4c2e"
	t.Setenv("HUFU_TEST_PASSWORD", secret)

	resp, err := runShellCommand(
		context.Background(),
		5*time.Second,
		"",
		false,
		"bash",
		[]string{"-c", "printf '%s\\n' \"$HUFU_TEST_PASSWORD\"; env | grep '^HUFU_TEST_PASSWORD='"},
		nil,
	)
	if err != nil {
		t.Fatalf("runShellCommand error: %v", err)
	}
	if strings.Contains(resp.Content, secret) {
		t.Fatal("bash tool response exposed an inherited secret value")
	}
	if !strings.Contains(resp.Content, "[REDACTED]") {
		t.Fatalf("bash tool response did not show a redaction marker: %q", resp.Content)
	}
}

// A command that intentionally backgrounds a long-running process (a model
// starting a dev server with `server &`, say) must return once its direct
// child exits, not block for the full timeout waiting on the backgrounded
// grandchild to close the pipe it inherited. This guards the grace window in
// waitAndDrain: it must stay short enough that this still returns promptly.
func TestRunShellCommandBackgroundedProcessReturnsPromptly(t *testing.T) {
	marker := fmt.Sprintf("299.%06d", os.Getpid()%1000000)
	cmdStr := fmt.Sprintf("sleep %s & echo done", marker)

	start := time.Now()
	resp, err := runShellCommand(context.Background(), 30*time.Second, "", false, "bash", []string{"-c", cmdStr}, nil)
	if err != nil {
		t.Fatalf("runShellCommand error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("runShellCommand blocked %s on a backgrounded grandchild instead of returning promptly", elapsed)
	}
	if !strings.Contains(resp.Content, "done") {
		t.Fatalf("expected output to contain %q, got: %q", "done", resp.Content)
	}

	exec.Command("pkill", "-f", "sleep "+marker).Run()
}

// writeFakeSudo installs a "sudo" on PATH that just execs its remaining
// arguments unprivileged, so forwarding tests can assert on routing without
// depending on the test machine's real sudo configuration (password prompt,
// NOPASSWD policy, etc).
func writeFakeSudo(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake sudo: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func bashCallCtx(allowedTools []string) context.Context {
	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	return context.WithValue(ctx, AgentToolsAllowedKey, allowedTools)
}

func TestExecuteBashForwardsSudoToSudoTool(t *testing.T) {
	writeFakeSudo(t)
	ctx := bashCallCtx([]string{"bash", "sudo"})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID: "1", Name: "bash",
		Input: `{"command": "sudo echo forwarded-ok"}`,
	}, ApplyOptions(nil))
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("expected the forwarded command to succeed, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "forwarded-ok") {
		t.Errorf("forwarded command output missing, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "automatically routed through the sudo tool") {
		t.Errorf("expected a routing note in the response, got: %s", resp.Content)
	}
}

func TestExecuteBashSudoWithoutPermissionStillRejected(t *testing.T) {
	// No fake sudo needed: without permission this must never execute.
	ctx := bashCallCtx([]string{"bash"})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID: "1", Name: "bash",
		Input: `{"command": "sudo echo should-not-run"}`,
	}, ApplyOptions(nil))
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected rejection without sudo permission, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "not available in the bash tool") {
		t.Errorf("expected the standard reject message, got: %s", resp.Content)
	}
}

func TestExecuteBashSSHAndSudoCombinedNotForwarded(t *testing.T) {
	// A remote "sudo" inside an ssh command must not be rerouted to local
	// sudo — that would run the wrong command as the wrong privilege on the
	// wrong host. No fake sudo binary here: it must never be invoked.
	ctx := bashCallCtx([]string{"bash", "sudo", "ssh"})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID: "1", Name: "bash",
		Input: `{"command": "ssh remote-host 'sudo systemctl restart nginx'"}`,
	}, ApplyOptions(nil))
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected rejection for an ssh+sudo command, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "not available in the bash tool") {
		t.Errorf("expected the standard reject message, got: %s", resp.Content)
	}
}

func TestExecuteBashSSHAloneStillRejected(t *testing.T) {
	ctx := bashCallCtx([]string{"bash", "ssh"})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID: "1", Name: "bash",
		Input: `{"command": "ssh remote-host uptime"}`,
	}, ApplyOptions(nil))
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected rejection for a bare ssh command, got: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "use the ssh tool instead") {
		t.Errorf("expected an ssh-specific hint, got: %s", resp.Content)
	}
}

func TestExecuteBashRestrictedDoesNotForwardSudo(t *testing.T) {
	// Restricted (rbash) mode is its own sandboxed execution path; forwarding
	// would bypass the restrictions it exists to enforce. No fake sudo
	// binary: it must never be invoked here either.
	cfg := ApplyOptions([]ToolOption{WithRestrictedBash(true)})
	ctx := bashCallCtx([]string{"bash", "sudo"})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID: "1", Name: "bash",
		Input: `{"command": "sudo echo should-not-run"}`,
	}, cfg)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected rejection in restricted mode, got: %s", resp.Content)
	}
}
