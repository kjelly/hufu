package team

import (
	"strings"
	"testing"

	"time"

	"github.com/kjelly/hufu/internal/utils"
)

func TestCoordinator_GetCurrentStatus_DefaultIdle(t *testing.T) {
	c := &Coordinator{}
	if got := c.GetCurrentStatus(); got != "idle" {
		t.Errorf("GetCurrentStatus() = %q, want %q", got, "idle")
	}
}

func TestCoordinator_GetCurrentStatus_ModelStage(t *testing.T) {
	c := &Coordinator{}
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "coordinator" })
	c.updateSnapshot(func(s *currentSnapshot) { s.Model = "ollama/qwen3:8b" })
	c.SetCurrentStage("model")
	c.updateSnapshot(func(s *currentSnapshot) { s.Step = 3 })

	got := c.GetCurrentStatus()
	wantSubstrings := []string{"model", "agent=coordinator", "model=ollama/qwen3:8b", "step=3"}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("GetCurrentStatus() = %q, missing substring %q", got, s)
		}
	}
}

func TestCoordinator_GetCurrentStatus_ToolStage(t *testing.T) {
	c := &Coordinator{}
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "helper" })
	c.SetCurrentStage("tool")
	c.updateSnapshot(func(s *currentSnapshot) { s.Tool = "bash" })

	got := c.GetCurrentStatus()
	wantSubstrings := []string{"tool", "agent=helper", "tool=bash"}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("GetCurrentStatus() = %q, missing substring %q", got, s)
		}
	}
}

func TestCoordinator_GetCurrentStatus_TruncatesLongTask(t *testing.T) {
	c := &Coordinator{}
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "helper" })
	c.SetCurrentStage("model")
	longTask := strings.Repeat("a", 200)
	c.updateSnapshot(func(s *currentSnapshot) { s.Task = longTask })

	got := c.GetCurrentStatus()
	if !strings.Contains(got, "task=") {
		t.Errorf("GetCurrentStatus() = %q, missing task= substring", got)
	}
	// Task should be truncated, not contain all 200 chars verbatim.
	if strings.Contains(got, longTask) {
		t.Errorf("GetCurrentStatus() = %q, expected task to be truncated", got)
	}
}

func TestCoordinator_GetCurrentStatus_ElapseTime(t *testing.T) {
	c := &Coordinator{}
	c.SetCurrentStage("model")
	// Force the start time to be 2 seconds ago.
	c.currentStageStartMu.Lock()
	c.currentStageStart = time.Now().Add(-2 * time.Second)
	c.currentStageStartMu.Unlock()

	got := c.GetCurrentStatus()
	if !strings.Contains(got, "elapsed") {
		t.Errorf("GetCurrentStatus() = %q, expected to contain 'elapsed' substring", got)
	}
}

func TestCoordinator_GetCurrentStatus_IdleResetsStart(t *testing.T) {
	c := &Coordinator{}
	c.SetCurrentStage("model")
	c.updateSnapshot(func(s *currentSnapshot) { s.Tool = "bash" })

	// Stage is set; should be non-idle.
	if got := c.GetCurrentStatus(); got == "idle" {
		t.Errorf("after SetCurrentStage('model'), status = %q, want non-idle", got)
	}

	// Setting stage back to idle should reset the start time.
	c.SetCurrentStage("idle")
	if got := c.GetCurrentStatus(); got != "idle" {
		t.Errorf("after SetCurrentStage('idle'), status = %q, want %q", got, "idle")
	}
	c.currentStageStartMu.RLock()
	if !c.currentStageStart.IsZero() {
		t.Error("currentStageStart should be zero after SetCurrentStage('idle')")
	}
	c.currentStageStartMu.RUnlock()
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		input  string
		max    int
		expect string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hell…"},
		{"abc", 0, "…"},
		{"", 5, ""},
	}
	for _, tc := range cases {
		got := utils.TruncateString(tc.input, tc.max)
		if got != tc.expect {
			t.Errorf("utils.TruncateString(%q, %d) = %q, want %q", tc.input, tc.max, got, tc.expect)
		}
	}
}

func TestStatusToolResultRedactsSecretsBeyondCollapsedPreview(t *testing.T) {
	const secret = "status-result-credential-123456789"
	raw := strings.Repeat("x", 4100) + "\napi_token=" + secret + "\nvisible tail"
	var reported StatusEvent
	c := &Coordinator{reportStatus: func(event StatusEvent) { reported = event }}
	c.report(c.newEvent("tool_result").withToolResult("view", raw))

	if reported.Type != "tool_result" || reported.ToolName != "view" {
		t.Fatalf("reported unexpected event: type=%q tool=%q", reported.Type, reported.ToolName)
	}
	if strings.Contains(reported.ToolResult, secret) {
		t.Fatal("status event exposed a credential beyond the collapsed preview")
	}
	if !strings.Contains(reported.ToolResult, "api_token=[REDACTED]") || !strings.Contains(reported.ToolResult, "visible tail") {
		t.Fatal("status event did not preserve non-secret result context")
	}
	if !strings.Contains(raw, secret) {
		t.Fatal("status projection modified the original tool result")
	}
}
