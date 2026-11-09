package browser

import (
	"slices"
	"testing"
)

func buttonAX(name string, disabled bool) map[string]any {
	return map[string]any{"role": map[string]string{"value": "button"}, "name": map[string]string{"value": name}, "backendDOMNodeId": 12,
		"properties": []any{map[string]any{"name": "disabled", "value": map[string]bool{"value": disabled}}}}
}

func TestCDPClickBindsLatestEnabledButtonAndReadsFreshContent(t *testing.T) {
	for _, scenario := range []string{"success", "unknown", "new_snapshot", "disabled", "renamed", "document_changed", "obscured", "navigation", "closed", "dispatch_failure"} {
		t.Run(scenario, func(t *testing.T) {
			b, fixture := newCDPFixture(t)
			fixture.axNodes = []map[string]any{buttonAX("關閉彈窗", scenario == "disabled")}
			page, err := b.Snapshot(t.Context())
			if err != nil || len(page.Buttons) != 1 || page.Buttons[0].Name != "關閉彈窗" || page.CapturedAt.IsZero() {
				t.Fatalf("snapshot=%+v error=%v", page, err)
			}
			ref := page.Buttons[0].Ref
			switch scenario {
			case "unknown":
				ref = "invented"
			case "new_snapshot":
				if _, err := b.Snapshot(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "navigation":
				if err := b.Navigate(t.Context(), "https://example.test/other"); err != nil {
					t.Fatal(err)
				}
			case "closed":
				if err := b.Close(); err != nil {
					t.Fatal(err)
				}
			case "renamed":
				fixture.mu.Lock()
				fixture.axNodes = []map[string]any{buttonAX("Purchase", false)}
				fixture.mu.Unlock()
			case "document_changed":
				fixture.mu.Lock()
				fixture.frameURL = "https://example.test/other"
				fixture.mu.Unlock()
			case "obscured":
				fixture.mu.Lock()
				fixture.obstructed = true
				fixture.mu.Unlock()
			case "dispatch_failure":
				fixture.mu.Lock()
				fixture.fail = "Input.dispatchMouseEvent"
				fixture.mu.Unlock()
			}
			err = b.Click(t.Context(), ref)
			if (err == nil) != (scenario == "success") {
				t.Fatalf("click error=%v", err)
			}
			if scenario == "success" {
				fresh, err := b.Snapshot(t.Context())
				if err != nil || fresh.Text != "Fresh article after dismissing popup" {
					t.Fatalf("post-click=%+v (%v)", fresh, err)
				}
			}
			// Never replay the consumed ref, even after an uncertain dispatch.
			if err := b.Click(t.Context(), ref); err == nil {
				t.Fatal("old button reference replayed")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.clicks != 0 && scenario != "success" {
				t.Fatal("invalid reference changed browser state")
			}
			if scenario != "success" && scenario != "dispatch_failure" && slices.Contains(fixture.methods, "Input.dispatchMouseEvent") {
				t.Fatal("invalid click reached input dispatch")
			}
		})
	}
}
