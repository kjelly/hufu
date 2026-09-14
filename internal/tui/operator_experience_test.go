package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/operator"
)

func TestTUIProductionFilesStayBounded(t *testing.T) {
	t.Parallel()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve TUI source directory")
	}
	directory := filepath.Dir(source)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		if lines >= 800 {
			t.Errorf("%s has %d lines; HF-UX-039 requires every production TUI file to stay below 800", entry.Name(), lines)
		}
	}
}

func TestThemeModeValidationAndAutoFallback(t *testing.T) {
	for _, value := range []string{"auto", "light", "dark", "mono"} {
		if _, err := ParseThemeMode(value); err != nil {
			t.Fatalf("ParseThemeMode(%q): %v", value, err)
		}
	}
	if _, err := ParseThemeMode("paper"); err == nil {
		t.Fatal("invalid theme was accepted")
	}
	if got := resolveAutoTheme("0;15"); got != ThemeLight {
		t.Fatalf("light COLORFGBG resolved to %q", got)
	}
	if got := resolveAutoTheme("0;0"); got != ThemeDark {
		t.Fatalf("dark COLORFGBG resolved to %q", got)
	}
	if got := resolveAutoTheme("invalid"); got != ThemeDark {
		t.Fatalf("invalid COLORFGBG fallback = %q", got)
	}
}

func TestThemeSwitchIsInstanceScopedAndPreservesInteractionState(t *testing.T) {
	light := NewWithOptions("prompt", TeamInfo{}, Options{Theme: ThemeLight, Spinner: true, Owner: true})
	dark := NewWithOptions("prompt", TeamInfo{}, Options{Theme: ThemeDark, Spinner: true, Owner: true})
	light.width, light.height = 100, 30
	light.inDetail, light.detailID, light.cursorLine = true, "task-1", 7
	light.promptInput.SetValue("draft remains")
	light.terminals["task-1"] = "pty-1"
	darkBefore := dark.styles.prompt.GetForeground()

	updated, cmd := light.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	got := updated.(Model)
	if cmd == nil || got.themeMode != ThemeDark {
		t.Fatalf("theme switch = (%q, %v), want dark with redraw", got.themeMode, cmd)
	}
	if !got.inDetail || got.detailID != "task-1" || got.cursorLine != 7 || got.promptInput.Value() != "draft remains" || got.terminals["task-1"] != "pty-1" {
		t.Fatalf("theme switch lost interaction state: %#v", got)
	}
	if dark.styles.prompt.GetForeground() != darkBefore {
		t.Fatal("switching one model mutated another model's palette")
	}
}

func TestNoColorAndEpaperDisableColorAndMotion(t *testing.T) {
	m := NewWithOptions("prompt", TeamInfo{}, Options{
		Theme: ThemeLight, DisplayPreset: DisplayEpaper, NoColor: true, Spinner: true, Owner: true,
	})
	if _, ok := m.styles.prompt.GetForeground().(lipgloss.NoColor); !ok {
		t.Fatalf("no-color foreground = %T, want lipgloss.NoColor", m.styles.prompt.GetForeground())
	}
	if m.spinnerEnabled || m.spinnerTickCmd() != nil {
		t.Fatal("epaper must disable spinner animation")
	}
	if m.promptInput.Cursor.Mode() != cursor.CursorStatic || m.searchInput.Cursor.Mode() != cursor.CursorStatic {
		t.Fatal("epaper text inputs must use a static cursor")
	}
}

func TestAutoThemeWatcherIsIdleUntilBackgroundChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var light atomic.Bool
	m := NewWithOptions("prompt", TeamInfo{}, Options{
		Theme: ThemeAuto, Spinner: false, Owner: true, Context: ctx, ThemePoll: 5 * time.Millisecond,
		ThemeDetector: func(context.Context) ThemeMode {
			if light.Load() {
				return ThemeLight
			}
			return ThemeDark
		},
	})
	result := make(chan tea.Msg, 1)
	go func() { result <- m.watchThemeCmd()() }()
	select {
	case message := <-result:
		t.Fatalf("unchanged auto theme produced an idle repaint message: %#v", message)
	case <-time.After(20 * time.Millisecond):
	}
	light.Store(true)
	select {
	case message := <-result:
		changed, ok := message.(themeDetectedMsg)
		if !ok || changed.mode != ThemeLight {
			t.Fatalf("theme change message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("auto theme watcher did not observe a background change")
	}
}

func TestRealTmuxThemeDetection(t *testing.T) {
	if os.Getenv("HUFU_TEST_TMUX_THEME") != "1" {
		t.Skip("set HUFU_TEST_TMUX_THEME=1 inside a -eink tmux session")
	}
	t.Setenv("HUFU_THEME", "")
	t.Setenv("LC_IS_EINK", "")
	t.Setenv("COLORFGBG", "")
	if got := detectRuntimeTheme(t.Context()); got != ThemeLight {
		t.Fatalf("tmux -eink session resolved to %q, want light", got)
	}
}

func TestOperatorSnapshotSummaryMatchesSharedRenderer(t *testing.T) {
	snapshot := operator.OperatorSnapshot{
		Activity:  operator.ActivityView{State: operator.ActivityVerifying},
		Freshness: operator.FreshnessView{EventID: "evt-9", LiveState: "connected"},
		PrimaryAction: &operator.ActionSuggestion{
			ID: "wait-runtime", Kind: "wait", Availability: "available",
		},
	}
	want := operator.BuildSummary(snapshot)
	m := New("prompt", TeamInfo{})
	m.width, m.height = 100, 30
	updated, _ := m.Update(OperatorSnapshotMsg{Snapshot: snapshot})
	got := updated.(Model)
	view := ansi.Strip(got.renderSummaryStrip(100))
	for _, value := range []string{want.State, want.What, want.Next, want.Data} {
		if !strings.Contains(view, value) {
			t.Fatalf("summary strip missing shared value %q:\n%s", value, view)
		}
	}
}

func TestBackgroundEventsDoNotStealDetailFocus(t *testing.T) {
	m := New("prompt", TeamInfo{})
	m.inDetail, m.detailID, m.cursorLine = true, "task-1", 3
	updated, _ := m.Update(TaskLogMsg{TodoID: "task-2", Line: "new output"})
	got := updated.(Model)
	if !got.inDetail || got.detailID != "task-1" || got.cursorLine != 3 {
		t.Fatalf("background event stole detail focus: %#v", got)
	}
	if got.unread["task-2"] != 1 {
		t.Fatalf("unread count = %d, want 1", got.unread["task-2"])
	}
}

func TestOperatorSnapshotGenerationRejectsLateOldScope(t *testing.T) {
	m := New("prompt", TeamInfo{})
	oldSnapshot := operator.OperatorSnapshot{Scope: operator.ResolvedScope{RunID: "old-run", BranchID: "old-branch"}}
	newSnapshot := operator.OperatorSnapshot{Scope: operator.ResolvedScope{RunID: "new-run", BranchID: "new-branch"}}
	updated, _ := m.Update(OperatorSnapshotMsg{Generation: 1, Snapshot: oldSnapshot})
	m = updated.(Model)
	updated, _ = m.Update(OperatorSnapshotMsg{Generation: 2, Snapshot: newSnapshot})
	m = updated.(Model)
	updated, _ = m.Update(OperatorSnapshotMsg{Generation: 1, Snapshot: oldSnapshot})
	m = updated.(Model)
	updated, _ = m.Update(OperatorDetailsMsg{Generation: 1, Promotions: []OperatorPromotionDetail{{ID: "old-proposal"}}})
	m = updated.(Model)
	if m.operatorScope.RunID != "new-run" || m.operatorScope.BranchID != "new-branch" || len(m.operatorPromotions) != 0 {
		t.Fatalf("late operator query replaced newer scope: %#v", m)
	}
	updated, _ = m.Update(OperatorDetailsMsg{Generation: 2, Promotions: []OperatorPromotionDetail{{ID: "new-proposal"}}})
	m = updated.(Model)
	if len(m.operatorPromotions) != 1 || m.operatorPromotions[0].ID != "new-proposal" {
		t.Fatalf("current operator details were not accepted: %#v", m.operatorPromotions)
	}
}

func TestPassiveQuitDoesNotRequestOwnerWrapUp(t *testing.T) {
	m := NewWithOptions("prompt", TeamInfo{}, Options{Theme: ThemeMono, Spinner: false, Owner: false})
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("passive q did not close the view")
	}
	if updated.(Model).wrapUpRequested || len(m.WrapUpCh) != 0 {
		t.Fatal("passive q requested owner runtime mutation")
	}

	owner := New("prompt", TeamInfo{})
	_, ownerCmd := owner.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if ownerCmd != nil {
		t.Fatal("active owner q must not claim a detach or terminate the runtime")
	}
}

func TestTerminalTextSanitizationAndCellWidth(t *testing.T) {
	dangerous := "safe\x1b]52;c;ZXhmaWw=\x07\n\x1b[31mred\x1b[0m\x1bPpayload\x1b\\"
	got := sanitizeTerminalText(dangerous)
	if got != "safe\nred" || strings.ContainsRune(got, '\x1b') {
		t.Fatalf("sanitizeTerminalText() = %q", got)
	}
	truncated := truncateCells("狀態正常", 6)
	if ansi.StringWidth(truncated) > 6 {
		t.Fatalf("CJK truncation width = %d, want <= 6: %q", ansi.StringWidth(truncated), truncated)
	}
}

func TestEightyColumnLayoutUsesReadableCompactGroups(t *testing.T) {
	m := New("prompt", TeamInfo{})
	m.width, m.height = 80, 24
	if !m.isCompact() {
		t.Fatal("80-column terminal must use compact task groups")
	}
	view := ansi.Strip(m.View())
	for _, label := range []string{"State:", "What:", "Next:", "Queued", "Active", "Finished"} {
		if !strings.Contains(view, label) {
			t.Fatalf("80-column view missing %q:\n%s", label, view)
		}
	}
}

func TestSmallTerminalLayoutsKeepStateWhatNextPath(t *testing.T) {
	for _, size := range []struct{ width, height int }{{60, 20}, {40, 12}} {
		m := New("檢查任務", TeamInfo{})
		m.width, m.height = size.width, size.height
		view := ansi.Strip(m.View())
		for _, label := range []string{"State:", "What:", "Next:"} {
			if !strings.Contains(view, label) {
				t.Fatalf("%dx%d view missing %q:\n%s", size.width, size.height, label, view)
			}
		}
	}
}
