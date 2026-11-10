package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/browser"
)

// ChromePageState is a validated semantic decision, never a substring match on
// untrusted page text. Classification does not authorize browser interaction.
type ChromePageState string

const (
	ChromePageReadable    ChromePageState = "readable"
	ChromePageNeedsHuman  ChromePageState = "needs_human_verification"
	ChromePageAccessError ChromePageState = "access_error"
	ChromePageUncertain   ChromePageState = "uncertain"
)

type ChromePageClassifier func(context.Context, browser.Snapshot) (ChromePageState, error)

// ErrChromePageVerificationDenied prevents a rejected runtime contract from
// falling back to a hidden human interaction or another page read.
var ErrChromePageVerificationDenied = errors.New("browser page verification denied by execution policy")

type chromePageClassifierKey struct{}

// WithChromePageClassifier attaches the runtime's durable decision dispatcher.
// Direct tools, dynamic tools and native actions share this boundary.
func WithChromePageClassifier(ctx context.Context, classifier ChromePageClassifier) context.Context {
	return context.WithValue(ctx, chromePageClassifierKey{}, classifier)
}

func classifyChromePage(ctx context.Context, page browser.Snapshot) (ChromePageState, error) {
	if err := ctx.Err(); err != nil {
		return ChromePageUncertain, err
	}
	classify, _ := ctx.Value(chromePageClassifierKey{}).(ChromePageClassifier)
	if classify == nil {
		return ChromePageUncertain, errors.New("browser page classifier unavailable")
	}
	state, err := classify(ctx, page)
	if err != nil {
		return ChromePageUncertain, err
	}
	switch state {
	case ChromePageReadable, ChromePageNeedsHuman, ChromePageAccessError, ChromePageUncertain:
		return state, nil
	default:
		return ChromePageUncertain, errors.New("invalid browser page classification")
	}
}

// verifyPage holds the declaration's operation gate throughout classification,
// human input and recapture. The worker cannot navigate away during that wait.
// At most one human request is made per unresolved URL, including cancelled
// waits and renavigation. A readable capture clears that URL's request marker.
func (f *chromePageFlow) verifyPage(ctx context.Context, page browser.Snapshot, afterDwell bool) (string, error) {
	state, classificationErr := classifyChromePage(ctx, page)
	if classificationErr != nil {
		return "", chromePageVerificationError(page, state, classificationErr)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if state == ChromePageReadable {
		delete(f.humanRequests, page.URL)
		if afterDwell {
			return "", nil // preserve initial capture and dwell metadata
		}
		return formatChromeSnapshot(page)
	}
	// A challenge can clear itself while dwelling. Check the actual page before
	// interrupting an operator; no second dwell is owed for this fresh capture.
	if afterDwell {
		fresh, err := f.page.Snapshot(ctx)
		if err != nil {
			return "", err
		}
		page = fresh
		state, classificationErr = classifyChromePage(ctx, page)
	}
	if classificationErr != nil {
		return "", chromePageVerificationError(page, state, classificationErr)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if state == ChromePageReadable {
		delete(f.humanRequests, page.URL)
		return formatChromeSnapshot(page)
	}
	if state != ChromePageNeedsHuman {
		return "", chromePageVerificationError(page, state, classificationErr)
	}
	if err := f.requestHumanVerification(ctx, page, f.waitForHuman); err != nil {
		return "", chromePageVerificationError(page, state, err)
	}
	fresh, err := f.page.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	state, classificationErr = classifyChromePage(ctx, fresh)
	if state != ChromePageReadable || classificationErr != nil {
		return "", chromePageVerificationError(fresh, state, classificationErr)
	}
	delete(f.humanRequests, page.URL)
	return formatChromeSnapshot(fresh)
}

// Explicit waits obey the same classification and per-URL request boundary as
// automatic waits. A worker cannot turn a classifier failure into a prompt by
// calling browser_wait_for_user itself.
func (f *chromePageFlow) waitForVerification(ctx context.Context, wait func(context.Context) error) error {
	page, err := f.page.Snapshot(ctx)
	if err != nil {
		return err
	}
	state, err := classifyChromePage(ctx, page)
	if err != nil {
		return chromePageVerificationError(page, state, err)
	}
	if state == ChromePageReadable {
		delete(f.humanRequests, page.URL)
		return errors.New("browser_page_readable: no human verification is required; call browser_snapshot for source evidence")
	}
	if state != ChromePageNeedsHuman {
		return chromePageVerificationError(page, state, nil)
	}
	return f.requestHumanVerification(ctx, page, wait)
}

func (f *chromePageFlow) requestHumanVerification(ctx context.Context, page browser.Snapshot, wait func(context.Context) error) error {
	if f.humanRequests[page.URL] {
		return errors.New("human verification already requested for this unresolved URL")
	}
	if f.humanRequests == nil {
		f.humanRequests = make(map[string]bool)
	}
	f.humanRequests[page.URL] = true
	if wait == nil {
		return errors.New("human verification unavailable")
	}
	return wait(ctx)
}

func chromePageVerificationError(page browser.Snapshot, state ChromePageState, cause error) error {
	text, err := formatChromeSnapshot(page)
	if err != nil {
		return err
	}
	if state == ChromePageUncertain {
		text = "Classification did not establish a security challenge. Do not call browser_wait_for_user to repair a classifier failure or uncertainty.\n" + text
	}
	if cause != nil {
		return fmt.Errorf("browser_page_%s: content is not verified source evidence: %w\n%s", state, cause, text)
	}
	return fmt.Errorf("browser_page_%s: content is not verified source evidence.\n%s", state, text)
}
