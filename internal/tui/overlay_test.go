package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func TestOverlayString(t *testing.T) {
	cases := []struct {
		o    Overlay
		want string
	}{
		{OverlayNone, "none"},
		{OverlayAskUser, "ask_user"},
		{OverlayHelp, "help"},
		{OverlayInfo, "info"},
		{OverlaySearch, "search"},
		{OverlayPromptInput, "prompt_input"},
		{OverlayConfirm, "confirm"},
		{OverlayDetail, "detail"},
		{OverlayResult, "result"},
		{OverlayMemory, "memory"},
		{OverlayActivityLog, "activity_log"},
		{OverlayOperator, "operator"},
		{Overlay(999), "unknown"},
	}
	for _, c := range cases {
		if got := c.o.String(); got != c.want {
			t.Errorf("Overlay(%d).String() = %q, want %q", c.o, got, c.want)
		}
	}
}

func TestCurrentOverlay(t *testing.T) {
	m := &Model{}
	if m.currentOverlay() != OverlayNone {
		t.Errorf("fresh model should have OverlayNone, got %v", m.currentOverlay())
	}

	m.inHelp = true
	if m.currentOverlay() != OverlayHelp {
		t.Errorf("with inHelp=true, expected OverlayHelp, got %v", m.currentOverlay())
	}

	m.inHelp = false
	m.inInfo = true
	if m.currentOverlay() != OverlayInfo {
		t.Errorf("with inInfo=true, expected OverlayInfo, got %v", m.currentOverlay())
	}

	// Priority: inAskUser wins over inHelp
	m.inHelp = true
	m.inAskUser = true
	if m.currentOverlay() != OverlayAskUser {
		t.Errorf("with inAskUser+inHelp, expected OverlayAskUser (higher priority), got %v", m.currentOverlay())
	}
}

func TestSetOverlay(t *testing.T) {
	m := &Model{}
	m.width = 100
	m.height = 40

	// Activate a single overlay
	prev := m.setOverlay(OverlayHelp)
	if prev != OverlayNone {
		t.Errorf("expected previous OverlayNone, got %v", prev)
	}
	if m.currentOverlay() != OverlayHelp {
		t.Errorf("expected OverlayHelp, got %v", m.currentOverlay())
	}
	if !m.inHelp {
		t.Error("expected inHelp=true after setOverlay(OverlayHelp)")
	}

	// Switch to another overlay (mutual exclusion)
	prev = m.setOverlay(OverlayInfo)
	if prev != OverlayHelp {
		t.Errorf("expected previous OverlayHelp, got %v", prev)
	}
	if m.currentOverlay() != OverlayInfo {
		t.Errorf("expected OverlayInfo after switch, got %v", m.currentOverlay())
	}
	if m.inHelp {
		t.Error("expected inHelp=false after switching to OverlayInfo")
	}
	if !m.inInfo {
		t.Error("expected inInfo=true after setOverlay(OverlayInfo)")
	}

	// Clear overlay
	prev = m.setOverlay(OverlayNone)
	if prev != OverlayInfo {
		t.Errorf("expected previous OverlayInfo, got %v", prev)
	}
	if m.currentOverlay() != OverlayNone {
		t.Errorf("expected OverlayNone after clear, got %v", m.currentOverlay())
	}
}

func TestSetOverlayIsIdempotent(t *testing.T) {
	m := &Model{}
	m.setOverlay(OverlayHelp)
	prev := m.setOverlay(OverlayHelp)
	if prev != OverlayHelp {
		t.Errorf("expected previous OverlayHelp, got %v", prev)
	}
	if !m.inHelp {
		t.Error("expected inHelp to remain true")
	}
}

func TestClearAllOverlays(t *testing.T) {
	m := &Model{}
	m.inHelp = true
	m.inInfo = true
	m.inSearch = true
	m.inOperator = true
	m.clearAllOverlays()
	if m.currentOverlay() != OverlayNone {
		t.Errorf("expected OverlayNone after clearAllOverlays, got %v", m.currentOverlay())
	}
}

func TestOperatorPanelUsesVerifiedDetailsAndSeparatedLearningSignals(t *testing.T) {
	m := NewWithOptions("task", TeamInfo{}, Options{Theme: ThemeMono, NoColor: true, Owner: true})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(Model)
	m.tasks = []*team.TodoItem{{ID: "task-1", Status: team.TaskPending}}
	exposed, consulted, applied, verified := int64(7), int64(5), int64(3), int64(2)
	snapshot := operator.OperatorSnapshot{
		Scope: operator.ResolvedScope{WorkspaceExact: "/workspace", ProjectID: "project", TeamName: "team"},
		Learning: operator.LearningView{
			Status: "available", RequestedMode: "active", EffectiveMode: "observe", PolicyVersion: "policy-v1",
			Exposures: &exposed, Consulted: &consulted, Applied: &applied, VerifiedSupport: &verified,
		},
	}
	updated, _ = m.Update(OperatorSnapshotMsg{Snapshot: snapshot})
	m = updated.(Model)
	updated, _ = m.Update(OperatorDetailsMsg{
		Evidence:        OperatorEvidenceDetail{Available: true, ManifestStatus: "complete", Verdict: "verified", Acceptance: "passed", Requirements: 2, Artifacts: 1},
		Promotions:      []OperatorPromotionDetail{{ID: "proposal-1", Type: "skill", Status: "approved", TargetPath: "skills/safe/SKILL.md", SourceCount: 2}},
		PromotionStatus: "available",
		PromotionTotal:  1,
	})
	m = updated.(Model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	m = updated.(Model)
	if !m.inOperator || m.currentOverlay() != OverlayOperator {
		t.Fatalf("L did not open operator panel: overlay=%s", m.currentOverlay())
	}
	view := sanitizeTerminalText(m.View())
	for _, want := range []string{
		"Evidence", "verdict=verified", "raw claims remain claims", "Context", "task=task-1",
		"exposed=7 consulted=5", "applied=3", "verified=2", "approved", "context promotion review",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("operator panel missing %q:\n%s", want, view)
		}
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.inOperator {
		t.Fatal("escape did not close operator panel")
	}
}
