package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/waitbudget"
)

type chromePageFixture struct {
	mu                            sync.Mutex
	text                          string
	navigations, captures, clicks int
	failCapture                   bool
}

func (p *chromePageFixture) Navigate(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.navigations++
	return nil
}
func (p *chromePageFixture) Snapshot(context.Context) (browser.Snapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.captures++
	if p.failCapture {
		return browser.Snapshot{}, errors.New("capture failed")
	}
	return browser.Snapshot{URL: "https://example.test/article", Text: p.text, CapturedAt: time.Now().UTC()}, nil
}
func (p *chromePageFixture) Click(context.Context, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clicks++
	p.text = "Fresh article after popup dismissal"
	return nil
}

func TestChromePageFlowCapturesBeforeDwellAndRereadsWithoutDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		page := &chromePageFixture{text: "Article before late popup"}
		flow := chromePageFlow{page: page, dwell: &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}}
		var idle waitbudget.RunIdleClock
		total, endTotal := idle.WithTimeout(t.Context(), time.Minute)
		defer endTotal()
		ctx, end := tools.WithInteractiveAwareTimeout(total, 30*time.Second)
		defer end()
		start := time.Now()
		if _, err := flow.navigate(ctx, "https://example.test/article"); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 0 || page.captures != 0 {
			t.Fatal("navigation captured or waited instead of returning immediately")
		}
		if _, err := flow.navigate(ctx, "https://example.test/other"); err == nil || page.navigations != 1 {
			t.Fatal("navigation skipped the required capture/dwell")
		}
		time.AfterFunc(time.Minute, func() { page.mu.Lock(); page.text = "Late popup replaced visible text"; page.mu.Unlock() })
		text, err := flow.snapshot(ctx)
		if err != nil || !strings.Contains(text, "Article before late popup") || strings.Contains(text, "Late popup replaced") || time.Since(start) != 5*time.Minute || total.Err() != nil || ctx.Err() != nil {
			t.Fatalf("initial capture=%q error=%v elapsed=%v", text, err, time.Since(start))
		}
		text, err = flow.snapshot(ctx)
		if err != nil || !strings.Contains(text, "Late popup replaced") || time.Since(start) != 5*time.Minute {
			t.Fatalf("fresh capture=%q error=%v", text, err)
		}
		if page.captures != 2 {
			t.Fatal("snapshot was cached or recaptured after the dwell")
		}
	})
}

func TestChromePageFlowClickRequiresAuthorityAndFreshPostClickCapture(t *testing.T) {
	for _, scenario := range []string{"allowed", "unauthorized", "capture_failure"} {
		t.Run(scenario, func(t *testing.T) {
			page := &chromePageFixture{text: "Popup"}
			flow := chromePageFlow{page: page}
			ctx := context.WithValue(t.Context(), tools.BrowserInteractionAllowedKey, scenario != "unauthorized")
			page.failCapture = scenario == "capture_failure"
			text, err := flow.click(ctx, "snapshot-ref")
			if scenario == "allowed" {
				if err != nil || !strings.Contains(text, "Fresh article after popup dismissal") || page.clicks != 1 || page.captures != 1 {
					t.Fatalf("click=%q error=%v", text, err)
				}
			} else if err == nil {
				t.Fatal("failure became a successful click")
			}
			if scenario == "unauthorized" && (page.clicks != 0 || page.captures != 0) {
				t.Fatal("unauthorized click reached the browser")
			}
			if scenario == "capture_failure" && (page.clicks != 1 || page.captures != 1 || !strings.Contains(err.Error(), "do not repeat")) {
				t.Fatal("uncertain post-click capture concealed the dispatched action")
			}
		})
	}
}

func TestChromePageFlowFailedCaptureCanRetryOrLeaveTheSource(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "retry"}[retry], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				page := &chromePageFixture{failCapture: true, text: "Retry source"}
				flow := chromePageFlow{page: page, dwell: &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}}
				start := time.Now()
				if _, err := flow.navigate(t.Context(), "https://example.test/article"); err != nil {
					t.Fatal(err)
				}
				if _, err := flow.snapshot(t.Context()); err == nil || time.Since(start) != 0 {
					t.Fatal("failed capture started a dwell")
				}
				page.failCapture = false
				if retry {
					if _, err := flow.snapshot(t.Context()); err != nil || time.Since(start) != 5*time.Minute {
						t.Fatal("successful retry skipped its first dwell")
					}
				} else if _, err := flow.navigate(t.Context(), "https://example.test/replacement"); err != nil {
					t.Fatal("failed capture trapped the worker on an unreadable source", err)
				}
			})
		})
	}
}

func TestChromePageFlowCancelledDwellAllowsNextNavigation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		page := &chromePageFixture{text: "Captured source"}
		flow := chromePageFlow{page: page, dwell: &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if _, err := flow.navigate(ctx, "https://example.test/article"); err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(time.Minute, cancel)
		if _, err := flow.snapshot(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled dwell=%v", err)
		}
		// A retained chat manager must not carry cancelled idle into the next
		// invocation. The next source still receives its own initial dwell.
		start := time.Now()
		if _, err := flow.navigate(t.Context(), "https://example.test/next"); err != nil {
			t.Fatal("cancelled dwell blocked the next navigation", err)
		}
		if time.Since(start) != 0 {
			t.Fatal("navigation inherited a cancelled dwell")
		}
		if _, err := flow.snapshot(t.Context()); err != nil || time.Since(start) != 5*time.Minute {
			t.Fatal("new source skipped its capture/dwell", err)
		}
	})
}
