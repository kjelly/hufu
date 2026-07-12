package tui

import "testing"

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
	m.clearAllOverlays()
	if m.currentOverlay() != OverlayNone {
		t.Errorf("expected OverlayNone after clearAllOverlays, got %v", m.currentOverlay())
	}
}
