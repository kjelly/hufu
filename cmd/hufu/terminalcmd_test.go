package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

func TestRenderTerminalScreenRendersOnlyChanges(t *testing.T) {
	var output bytes.Buffer
	last := ""
	renderTerminalScreen(&output, "ready", &last)
	renderTerminalScreen(&output, "ready", &last)
	if got, want := output.String(), "\x1b[H\x1b[2Jready"; got != want {
		t.Fatalf("rendered output = %q, want %q", got, want)
	}
}

func TestListTerminalSessionsRendersSafeCleanupDiagnostics(t *testing.T) {
	workspace := t.TempDir()
	logs := filepath.Join(workspace, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	sessions := []team.TerminalSession{{
		ID: "session-1", OwnerTaskID: "task-1", State: team.TerminalSessionClosed,
		Custodian: team.TerminalCustodianCoordinator, CleanupState: team.TerminalCleanupCompleted,
		CleanupCompletedAt: time.Now().UTC(), OutputRefs: []team.ArtifactRef{{Path: "logs/terminal/session-1.log", Type: "terminal_output"}},
	}}
	data, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "terminal_sessions.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	if err := listTerminalSessions(&text, workspace, false); err != nil {
		t.Fatal(err)
	}
	if got := text.String(); !strings.Contains(got, "contained; safe to retry") || strings.Contains(got, "terminal_output") {
		t.Fatalf("text list should give safe guidance without output content: %q", got)
	}
	var jsonOutput bytes.Buffer
	if err := listTerminalSessions(&jsonOutput, workspace, true); err != nil {
		t.Fatal(err)
	}
	var entries []terminalListEntry
	if err := json.Unmarshal(jsonOutput.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].CleanupState != team.TerminalCleanupCompleted || entries[0].Guidance != "contained; safe to retry" {
		t.Fatalf("JSON terminal diagnostics = %#v", entries)
	}
}

func TestListTerminalSessionsDoesNotReconcileOrMutateActiveSession(t *testing.T) {
	workspace := t.TempDir()
	logs := filepath.Join(workspace, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	original := []team.TerminalSession{{
		ID: "active-session", State: team.TerminalSessionRunning, Running: true,
	}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "terminal_sessions.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := listTerminalSessions(&output, workspace, false); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(data) {
		t.Fatalf("terminal list mutated durable session state: got %s, want %s", stored, data)
	}
	if !strings.Contains(output.String(), "active; wait for exit") {
		t.Fatalf("active session guidance = %q", output.String())
	}
}

func TestTerminalCommandIncludesAttach(t *testing.T) {
	command, _, err := terminalCmd.Find([]string{"attach"})
	if err != nil {
		t.Fatal(err)
	}
	if command != terminalAttachCmd {
		t.Fatalf("attach command = %p, want %p", command, terminalAttachCmd)
	}
}

func TestTerminalCommandIncludesList(t *testing.T) {
	command, _, err := terminalCmd.Find([]string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	if command != terminalListCmd {
		t.Fatalf("list command = %p, want %p", command, terminalListCmd)
	}
}
