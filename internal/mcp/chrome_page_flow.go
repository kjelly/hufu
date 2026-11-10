package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
)

type chromePage interface {
	Navigate(context.Context, string) error
	Snapshot(context.Context) (browser.Snapshot, error)
	Click(context.Context, string) error
}

// chromePageFlow is serialized by the declaration's ordinary operation gate.
// A navigation opens one pending capture/dwell; rereads and click verification
// never silently add a second dwell or return a cached document.
type chromePageFlow struct {
	page          chromePage
	dwell         *ChromePageDwellConfig
	pendingDwell  bool
	captureFailed bool
	verification  bool
	humanRequests map[string]bool
	waitForHuman  func(context.Context) error
}

func (f *chromePageFlow) navigate(ctx context.Context, url string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if f.pendingDwell && !f.captureFailed {
		return "", errors.New("take browser_snapshot to capture this page and complete its configured dwell before another navigation")
	}
	if err := f.page.Navigate(ctx, url); err != nil {
		return "", err
	}
	f.pendingDwell = f.dwell != nil
	f.captureFailed = false
	return "Navigation completed. Call browser_snapshot now to capture the page before the configured dwell; navigation alone is not source evidence.", nil
}

func (f *chromePageFlow) read(ctx context.Context) (string, error) {
	page, err := f.page.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	if f.verification {
		return f.verifyPage(ctx, page, false)
	}
	return formatChromeSnapshot(page)
}

func (f *chromePageFlow) snapshot(ctx context.Context) (string, error) {
	var delay *ChromePageDwellConfig
	if f.pendingDwell {
		delay = f.dwell
	}
	var captured browser.Snapshot
	text, err := snapshotChromeWithDwell(ctx, func(ctx context.Context) (string, error) {
		page, err := f.page.Snapshot(ctx)
		f.captureFailed = err != nil
		if err != nil {
			return "", err
		}
		captured = page
		return formatChromeSnapshot(page)
	}, delay)
	if err == nil || ctx.Err() != nil {
		// A cancelled call may belong to a finished chat turn while the
		// manager remains alive. Do not carry its idle debt into the next turn.
		f.pendingDwell = false
	}
	if err == nil && f.verification {
		verified, verifyErr := f.verifyPage(ctx, captured, delay != nil)
		if verifyErr != nil {
			return "", verifyErr
		}
		// Keep the capture-before-dwell receipt for readable initial pages.
		if verified != "" {
			text = verified
		}
	}
	return text, err
}

func (f *chromePageFlow) click(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	allowed, _ := ctx.Value(tools.BrowserInteractionAllowedKey).(bool)
	if !allowed {
		return "", errors.New("browser_click requires runtime browser_interaction authority")
	}
	if f.pendingDwell {
		return "", errors.New("take browser_snapshot before clicking")
	}
	if err := f.page.Click(ctx, ref); err != nil {
		return "", err
	}
	fresh, err := f.read(ctx)
	if err != nil {
		return "", fmt.Errorf("click was dispatched, but the fresh snapshot failed; do not repeat the click without inspecting the page: %w", err)
	}
	return "Click dispatched. Fresh post-click snapshot follows; confirm article content and actual URL. Call browser_snapshot for citation evidence.\n" + fresh, nil
}
