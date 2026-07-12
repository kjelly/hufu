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
