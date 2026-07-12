//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func runWaitFor(t *testing.T, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := executeWaitFor(context.Background(), fantasy.ToolCall{ID: "1", Name: "wait_for", Input: input}, ToolConfig{})
	if err != nil {
		t.Fatalf("executeWaitFor returned unexpected error: %v", err)
	}
	return resp
}

func TestWaitForValidation(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "missing command",
			input:   `{}`,
			wantErr: "command parameter is required",
		},
		{
			name:    "invalid json",
			input:   `{not json`,
			wantErr: "command parameter is required",
		},
		{
			name:    "cd is blocked",
			input:   `{"command":"cd /tmp && ls"}`,
			wantErr: "'cd' is not allowed",
		},
		{
			name:    "banned builtin is blocked",
			input:   `{"command":"jobs -l"}`,
			wantErr: "not allowed",
		},
		{
			name:    "sudo prefix is rejected with hint",
			input:   `{"command":"sudo virsh list"}`,
			wantErr: "set sudo:true",
		},
		{
			name:    "ssh prefix is rejected with hint",
			input:   `{"command":"ssh host uptime"}`,
			wantErr: "ssh tool",
		},
		{
			name:    "invalid success_pattern",
			input:   `{"command":"true","success_pattern":"("}`,
			wantErr: "invalid success_pattern",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := runWaitFor(t, tt.input)
			if !resp.IsError {
				t.Fatalf("expected error response, got success: %s", resp.Content)
			}
			if !strings.Contains(resp.Content, tt.wantErr) {
				t.Errorf("error %q missing %q", resp.Content, tt.wantErr)
			}
		})
	}
}

func TestWaitForImmediateSuccess(t *testing.T) {
	resp := runWaitFor(t, `{"command":"echo ready"}`)
	if resp.IsError {
		t.Fatalf("expected success, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "ready") {
		t.Errorf("output missing command stdout: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "condition met after 1 attempt(s)") {
		t.Errorf("output missing attempt summary: %s", resp.Content)
	}
}

func TestWaitForSucceedsAfterRetries(t *testing.T) {
	// The command fails until its counter file accumulates 3 lines, so the
	// tool must poll exactly 3 times before succeeding.
	counter := filepath.Join(t.TempDir(), "count")
	cmd := fmt.Sprintf(`echo x >> %[1]s; [ "$(wc -l < %[1]s)" -ge 3 ] && echo done`, counter)
	input := fmt.Sprintf(`{"command":%q,"interval_seconds":1,"timeout_seconds":30}`, cmd)

	start := time.Now()
	resp := runWaitFor(t, input)
	if resp.IsError {
		t.Fatalf("expected success, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "condition met after 3 attempt(s)") {
		t.Errorf("expected 3 attempts, got: %s", resp.Content)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second {
		t.Errorf("expected at least 2 interval waits, finished in %s", elapsed)
	}
}

func TestWaitForSuccessPattern(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantSub string
	}{
		{
			name:   "exit 0 with matching pattern succeeds",
			input:  `{"command":"echo state: running","success_pattern":"state:\\s*running"}`,
			wantOK: true,
		},
		{
			name:    "exit 0 with non-matching pattern times out",
			input:   `{"command":"echo state: booting","interval_seconds":1,"timeout_seconds":2,"success_pattern":"state:\\s*running"}`,
			wantOK:  false,
			wantSub: "state: booting",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := runWaitFor(t, tt.input)
			if tt.wantOK && resp.IsError {
				t.Fatalf("expected success, got error: %s", resp.Content)
			}
			if !tt.wantOK && !resp.IsError {
				t.Fatalf("expected timeout error, got success: %s", resp.Content)
			}
			if tt.wantSub != "" && !strings.Contains(resp.Content, tt.wantSub) {
				t.Errorf("response missing %q: %s", tt.wantSub, resp.Content)
			}
		})
	}
}

func TestWaitForTimeoutIncludesLastOutput(t *testing.T) {
	resp := runWaitFor(t, `{"command":"echo still booting; exit 1","interval_seconds":1,"timeout_seconds":2}`)
	if !resp.IsError {
		t.Fatalf("expected timeout error, got success: %s", resp.Content)
	}
	for _, want := range []string{"timed out", "still booting", "exit code 1", "Condition never met"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("timeout error missing %q: %s", want, resp.Content)
		}
	}
}

func TestWaitForStderrVisibleInOutput(t *testing.T) {
	resp := runWaitFor(t, `{"command":"echo warn >&2; exit 1","interval_seconds":1,"timeout_seconds":2}`)
	if !resp.IsError {
		t.Fatalf("expected timeout error, got success: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "STDERR:") || !strings.Contains(resp.Content, "warn") {
		t.Errorf("stderr missing from last output: %s", resp.Content)
	}
}

func TestWaitForParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	resp, err := executeWaitFor(ctx, fantasy.ToolCall{ID: "1", Name: "wait_for",
		Input: `{"command":"exit 1","interval_seconds":2,"timeout_seconds":60}`}, ToolConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("expected error response after cancellation, got success: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "cancelled") {
		t.Errorf("expected cancellation reason, got: %s", resp.Content)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took too long: %s", elapsed)
	}
}

func TestWaitForSudoRequiresAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		wantErr bool
	}{
		{"allowlist without sudo denies escalation", []string{"bash", "wait_for"}, true},
		{"allowlist with sudo permits", []string{"bash", "wait_for", "sudo"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), AgentToolsAllowedKey, tt.allowed)
			resp, err := executeWaitFor(ctx, fantasy.ToolCall{ID: "1", Name: "wait_for",
				Input: `{"command":"echo ok","sudo":true,"timeout_seconds":2,"interval_seconds":1}`}, ToolConfig{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr {
				if !resp.IsError || !strings.Contains(resp.Content, "sudo tool in this agent's tools allowlist") {
					t.Errorf("expected allowlist denial, got: %s", resp.Content)
				}
				return
			}
			// With sudo allowed the command attempts to run; depending on the
			// environment sudo may prompt/fail, so only assert the allowlist
			// check itself did not trigger.
			if strings.Contains(resp.Content, "tools allowlist") {
				t.Errorf("allowlist denial fired unexpectedly: %s", resp.Content)
			}
		})
	}
}

func TestWaitForWorkDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := executeWaitFor(context.Background(), fantasy.ToolCall{ID: "1", Name: "wait_for",
		Input: `{"command":"test -f marker && echo found"}`}, ToolConfig{WorkDir: dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("expected success, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "found") {
		t.Errorf("workdir not honored: %s", resp.Content)
	}
}
