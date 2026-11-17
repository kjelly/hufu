package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
)

func TestChromeSourceAccessLimitsOnlyTheUnavailableSource(t *testing.T) {
	for _, failure := range []string{"access_error", "classifier_error", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			page := &chromePageFixture{text: "Unable to resolve handle"}
			flow := chromePageFlow{page: page, verification: true}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = WithChromePageClassifier(ctx, func(context.Context, browser.Snapshot) (ChromePageState, error) {
				if failure == "classifier_error" {
					return ChromePageUncertain, errors.New("classification unavailable")
				}
				return ChromePageAccessError, nil
			})
			if _, err := flow.navigate(ctx, "https://example.test/article?tracking=1"); err != nil {
				t.Fatal(err)
			}
			if failure == "cancelled" {
				cancel()
			}
			for attempt := range 2 {
				_, err := flow.snapshot(ctx)
				unavailable, ok := errors.AsType[*chromeSourceAccessError](err)
				if failure != "access_error" {
					if err == nil || ok || len(flow.unavailableSources) != 0 {
						t.Fatalf("failure was softened: %v", err)
					}
					return
				}
				if !ok || unavailable.outcome.Citable || unavailable.outcome.URL != "https://example.test/article" || unavailable.outcome.Attempts != attempt+1 {
					t.Fatalf("missing source outcome: %v", err)
				}
			}
			for _, operation := range []func() error{
				func() error { _, err := flow.snapshot(ctx); return err },
				func() error {
					_, err := flow.navigate(ctx, "https://EXAMPLE.test/article?tracking=2#again")
					return err
				},
				func() error {
					return flow.waitForVerification(ctx, func(context.Context) error { t.Fatal("unexpected human request"); return nil })
				},
				func() error {
					_, err := flow.click(context.WithValue(ctx, tools.BrowserInteractionAllowedKey, true), "ref")
					return err
				},
			} {
				if _, ok := errors.AsType[*chromeSourceAccessError](operation()); !ok {
					t.Fatal("source limit was bypassed")
				}
			}
			if page.captures != 2 || page.navigations != 1 || page.clicks != 0 {
				t.Fatal("exhausted source reached the browser")
			}
			if _, err := flow.navigate(ctx, "https://example.test/replacement"); err != nil || page.navigations != 2 {
				t.Fatalf("source gap blocked replacement navigation: %v", err)
			}
			ctx = WithChromePageClassifier(ctx, func(context.Context, browser.Snapshot) (ChromePageState, error) { return ChromePageReadable, nil })
			if _, err := flow.snapshot(ctx); err != nil {
				t.Fatalf("source gap blocked replacement research: %v", err)
			}
		})
	}
}

func TestChromeSourceAccessRetainsJoinedAndWrappedFailures(t *testing.T) {
	unavailable := unavailableChromeSource(browser.Snapshot{URL: "https://example.test/source", Text: "Unavailable"})
	storage := errors.New("cannot persist verification observation")
	for _, failure := range []error{errors.Join(unavailable, storage), fmt.Errorf("click was dispatched: %w", unavailable)} {
		flow := chromePageFlow{}
		if _, ok := chromeSourceOutcome(failure); ok {
			t.Fatal("a composite failure became a recoverable availability observation")
		}
		if got := flow.recordSourceAccess(failure); got != failure || len(flow.unavailableSources) != 0 {
			t.Fatal("source classification hid another failure")
		}
	}
}
