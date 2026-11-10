package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/waitbudget"
)

const challengePageText = "Performing security verification\nThis website verifies you are not a bot.\nPerformance and Security by Cloudflare"

func TestChromePageVerificationRequiresFreshContentAfterHumanAnswer(t *testing.T) {
	for _, scenario := range []string{"confirmed", "cancelled", "still_challenged", "uncertain", "invalid_decision", "classifier_error", "policy_denied", "access_error", "readable"} {
		t.Run(scenario, func(t *testing.T) {
			page := &chromePageFixture{text: challengePageText}
			flow := chromePageFlow{page: page, verification: true}
			waits, decisions := 0, 0
			flow.waitForHuman = func(context.Context) error {
				waits++
				if page.captures != 1 {
					t.Fatal("human wait did not follow initial capture")
				}
				if scenario == "cancelled" {
					return context.Canceled
				}
				if scenario == "confirmed" {
					page.text = "Fresh public article after verification"
				}
				return nil
			}
			ctx := WithChromePageClassifier(t.Context(), func(_ context.Context, captured browser.Snapshot) (ChromePageState, error) {
				decisions++
				if captured.Text != page.text {
					t.Fatal("classification used stale page text")
				}
				if captured.Text == "Fresh public article after verification" || scenario == "readable" {
					return ChromePageReadable, nil
				}
				switch scenario {
				case "uncertain":
					return ChromePageUncertain, nil
				case "invalid_decision":
					return "unknown", nil
				case "classifier_error":
					return "", errors.New("provider unavailable")
				case "policy_denied":
					return "", ErrChromePageVerificationDenied
				case "access_error":
					return ChromePageAccessError, nil
				default:
					return ChromePageNeedsHuman, nil
				}
			})
			text, err := flow.snapshot(ctx)
			wantSuccess := scenario == "confirmed" || scenario == "readable"
			if (err == nil) != wantSuccess {
				t.Fatalf("text=%q error=%v", text, err)
			}
			if wantSuccess && scenario != "readable" && (!strings.Contains(text, "Fresh public article") || strings.Contains(text, challengePageText)) {
				t.Fatalf("confirmation became evidence without fresh content: %s", text)
			}
			wantWaits := 1
			if scenario == "readable" || scenario == "access_error" || scenario == "policy_denied" || scenario == "uncertain" || scenario == "invalid_decision" || scenario == "classifier_error" {
				wantWaits = 0
			}
			if waits != wantWaits || page.clicks != 0 || decisions < 1 {
				t.Fatalf("waits=%d clicks=%d decisions=%d", waits, page.clicks, decisions)
			}
			if !wantSuccess {
				_, _ = flow.snapshot(ctx)
				_, _ = flow.navigate(ctx, "https://example.test/article")
				_, _ = flow.snapshot(ctx)
				if waits != wantWaits {
					t.Fatal("repeated an unresolved human request")
				}
			}
		})
	}
}

func TestChromePageVerificationRejectsClassifierFailuresWithoutHumanWait(t *testing.T) {
	for _, path := range []string{"snapshot", "read", "explicit_wait", "after_dwell"} {
		t.Run(path, func(t *testing.T) {
			page := &chromePageFixture{text: "Already readable public article"}
			flow := chromePageFlow{page: page, verification: true}
			flow.waitForHuman = func(context.Context) error { t.Fatal("classifier failure requested human input"); return nil }
			failure := errors.New("decision_invalid_backend_output")
			decisions := 0
			ctx := WithChromePageClassifier(t.Context(), func(context.Context, browser.Snapshot) (ChromePageState, error) {
				decisions++
				if path == "after_dwell" && decisions == 1 {
					return ChromePageNeedsHuman, nil
				}
				return ChromePageUncertain, failure
			})
			var err error
			switch path {
			case "snapshot":
				_, err = flow.snapshot(ctx)
			case "read":
				_, err = flow.read(ctx)
			case "explicit_wait":
				err = flow.waitForVerification(ctx, flow.waitForHuman)
			case "after_dwell":
				captured, captureErr := page.Snapshot(ctx)
				if captureErr != nil {
					t.Fatal(captureErr)
				}
				_, err = flow.verifyPage(ctx, captured, true)
			}
			if !errors.Is(err, failure) || len(flow.humanRequests) != 0 {
				t.Fatalf("error=%v human requests=%v", err, flow.humanRequests)
			}
			wantDecisions := 1
			if path == "after_dwell" {
				wantDecisions = 2
			}
			if decisions != wantDecisions {
				t.Fatalf("classifications=%d want=%d", decisions, wantDecisions)
			}
		})
	}
}

func TestChromePageExplicitVerificationRequiresChallengeAndSharesRequestLimit(t *testing.T) {
	for _, state := range []ChromePageState{ChromePageReadable, ChromePageUncertain, ChromePageAccessError, ChromePageNeedsHuman} {
		t.Run(string(state), func(t *testing.T) {
			page := &chromePageFixture{text: "Current public page"}
			waits := 0
			flow := chromePageFlow{page: page, verification: true, waitForHuman: func(context.Context) error { waits++; return nil }}
			ctx := WithChromePageClassifier(t.Context(), func(context.Context, browser.Snapshot) (ChromePageState, error) { return state, nil })
			err := flow.waitForVerification(ctx, flow.waitForHuman)
			challenge := state == ChromePageNeedsHuman
			if (err == nil) != challenge || waits != map[bool]int{false: 0, true: 1}[challenge] {
				t.Fatalf("waits=%d error=%v", waits, err)
			}
			_, _ = flow.snapshot(ctx)
			_ = flow.waitForVerification(ctx, flow.waitForHuman)
			if waits > 1 || (!challenge && waits != 0) {
				t.Fatalf("repeated or unjustified prompt: %d", waits)
			}
		})
	}
}

func TestChromePageVerificationRefreshesChallengeAfterDwell(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		page := &chromePageFixture{text: challengePageText}
		flow := chromePageFlow{page: page, verification: true, dwell: &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}}
		if _, err := flow.navigate(t.Context(), "https://example.test/article"); err != nil {
			t.Fatal(err)
		}
		classifications := 0
		ctx := WithChromePageClassifier(t.Context(), func(_ context.Context, _ browser.Snapshot) (ChromePageState, error) {
			classifications++
			if classifications == 1 {
				page.text = "Article became readable during dwell"
				return ChromePageNeedsHuman, nil
			}
			return ChromePageReadable, nil
		})
		flow.waitForHuman = func(context.Context) error { t.Fatal("self-cleared challenge requested human input"); return nil }
		text, err := flow.snapshot(ctx)
		if err != nil || !strings.Contains(text, "Article became readable") || page.captures != 2 || classifications != 2 {
			t.Fatalf("text=%q error=%v captures=%d classifications=%d", text, err, page.captures, classifications)
		}
	})
}

func TestChromePageVerificationMissingRuntimeAndCancellationFailClosed(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_runtime", true: "cancelled"}[cancelled], func(t *testing.T) {
			page := &chromePageFixture{text: challengePageText}
			flow := chromePageFlow{page: page, verification: true}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			if text, err := flow.snapshot(ctx); err == nil || text != "" {
				t.Fatalf("unverified page accepted: text=%q error=%v", text, err)
			}
		})
	}
}

func TestChromePageVerificationConfiguration(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong_backend", "no_snapshot", "no_wait"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222", PageVerification: true, AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}}
			switch scenario {
			case "wrong_backend":
				cfg.Type = "remote"
			case "no_snapshot":
				cfg.ExcludedTools = []string{"browser_snapshot"}
			case "no_wait":
				cfg.ExcludedTools = []string{"browser_wait_for_user"}
			}
			if err := ValidateWorkerToolPolicies(cfg); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation=%v", err)
			}
			if scenario != "valid" {
				return
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var restored MCPServerConfig
			if err := json.Unmarshal(data, &restored); err != nil || !restored.PageVerification {
				t.Fatalf("config=%+v error=%v", restored, err)
			}
			manager := NewMCPToolManager("", "")
			t.Cleanup(func() { _ = manager.Close() })
			if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": restored}); err != nil {
				t.Fatal(err)
			}
			for _, tool := range manager.GetTools() {
				if !tool.pausableTimeout {
					t.Fatalf("human wait still has a wall timeout: %s", tool.Name)
				}
			}
		})
	}
}

func TestChromePageVerificationDwellAndHumanWaitPreserveWorkingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		page := &chromePageFixture{text: challengePageText}
		flow := chromePageFlow{page: page, verification: true, dwell: &ChromePageDwellConfig{MinSeconds: 388, MaxSeconds: 388}}
		var idle waitbudget.RunIdleClock
		total, endTotal := idle.WithTimeout(t.Context(), time.Minute)
		defer endTotal()
		ctx, end := tools.WithInteractiveAwareTimeout(total, 30*time.Second)
		defer end()
		ctx = WithChromePageClassifier(ctx, func(_ context.Context, captured browser.Snapshot) (ChromePageState, error) {
			if captured.Text == challengePageText {
				return ChromePageNeedsHuman, nil
			}
			return ChromePageReadable, nil
		})
		tools.SetOnAskUserTUI(func(ctx context.Context, _, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
			time.Sleep(time.Hour)
			if ctx.Err() != nil {
				t.Fatal("human wait consumed working-time budget")
			}
			page.text = "Fresh article after manual verification"
			return `{"answers":["continue"]}`, true
		})
		defer tools.SetOnAskUserTUI(nil)
		flow.waitForHuman = func(ctx context.Context) error {
			return tools.WaitForHumanVerification(ctx, "Complete public page verification")
		}
		start := time.Now()
		if _, err := flow.navigate(ctx, "https://example.test/article"); err != nil {
			t.Fatal(err)
		}
		text, err := flow.snapshot(ctx)
		if err != nil || !strings.Contains(text, "Fresh article") || time.Since(start) != time.Hour+388*time.Second || ctx.Err() != nil || total.Err() != nil || page.captures != 3 {
			t.Fatalf("text=%q error=%v elapsed=%v captures=%d", text, err, time.Since(start), page.captures)
		}
	})
}
