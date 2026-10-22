package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Exercise the size matrix without launching a provider or terminal. The PTY
// lifecycle test separately checks real SIGWINCH delivery and process survival.
func TestResizeRenderingContract(t *testing.T) {
	m := New("resize-contract-prompt", TeamInfo{})
	sizes := []struct{ width, height int }{
		{80, 24}, {200, 50}, {40, 10}, {120, 30}, {60, 20},
		{30, 10}, {1, 1}, {100, 30},
	}
	for i := range 20 {
		sizes = append(sizes, struct{ width, height int }{80 + i*5, 20 + i})
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			updated, _ := m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			m = updated.(Model)
			if m.width != size.width || m.height != size.height || !m.vpReady {
				t.Fatalf("resize not applied: width=%d height=%d ready=%t", m.width, m.height, m.vpReady)
			}
			view := ansi.Strip(m.View().Content)
			if size.width >= 9 && size.width < 60 && !strings.Contains(view, "Terminal too narrow") {
				t.Fatalf("narrow warning missing: %q", view)
			}
			if size.width >= 60 {
				if strings.Contains(view, "Terminal too narrow") || !strings.Contains(view, "resize-contract-prompt") {
					t.Fatalf("wide rendering did not recover: %q", view)
				}
			}
		})
	}
}
