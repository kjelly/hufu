package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
)

func TestChromeSnapshotAutomaticallyRequestsAuthorizedHumanVerification(t *testing.T) {
	for _, scenario := range []string{"confirmed", "explicit", "denied", "unattended", "cancelled", "still_challenged", "classifier_error", "uncertain", "readable", "access_error", "declined", "declined_storage_error", "explicit_declined_unclassified"} {
		t.Run(scenario, func(t *testing.T) {
			var verified atomic.Bool
			var captures, focuses atomic.Int32
			server := newChromeVerificationServer(t, &captures, &focuses, func() (string, string) {
				if verified.Load() {
					return "Fresh public article after verification", "Article"
				}
				return challengePageText, "Just a moment..."
			})
			manager := NewMCPToolManager("", "")
			t.Cleanup(func() { _ = manager.Close() })
			cfg := MCPServerConfig{Type: "chrome-cdp", URL: server.URL, PageVerification: scenario != "explicit_declined_unclassified", AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}}
			if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": cfg}); err != nil {
				t.Fatal(err)
			}
			ctx := WithChromePageClassifier(t.Context(), func(_ context.Context, page browser.Snapshot) (ChromePageState, error) {
				if scenario == "classifier_error" {
					return "", errors.New("classification unavailable")
				}
				if scenario == "uncertain" {
					return ChromePageUncertain, nil
				}
				if scenario == "readable" {
					return ChromePageReadable, nil
				}
				if scenario == "access_error" {
					return ChromePageAccessError, nil
				}
				if page.Text == challengePageText {
					return ChromePageNeedsHuman, nil
				}
				return ChromePageReadable, nil
			})
			waitAuthorizations, prompts := 0, 0
			ctx = WithToolAuthorizer(ctx, func(_ context.Context, server, tool, _ string) error {
				if server != "chrome" {
					t.Fatal("wrong browser declaration")
				}
				if tool == "browser_wait_for_user" {
					waitAuthorizations++
					if scenario == "denied" {
						return errors.New("human wait denied by policy")
					}
				}
				return nil
			})
			if scenario == "unattended" {
				ctx = context.WithValue(ctx, tools.UnattendedKey, true)
			}
			tools.SetOnAskUserTUI(func(_ context.Context, question, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
				prompts++
				if !strings.Contains(question, "https://example.test/source") || captures.Load() < 1 && scenario != "explicit_declined_unclassified" {
					t.Error("human prompt lost actual target or capture order")
				}
				if scenario == "explicit" && !strings.Contains(question, "Verify this public security challenge") {
					t.Error("explicit wait lost its authorized reason")
				}
				if scenario == "cancelled" {
					return `{"free_text":"cancel"}`, true
				}
				if strings.HasPrefix(scenario, "declined") || scenario == "explicit_declined_unclassified" {
					return `{"free_text":""}`, true
				}
				if scenario != "still_challenged" {
					verified.Store(true)
				}
				return chromeConfirmationAnswer(question), true
			})
			t.Cleanup(func() { tools.SetOnAskUserTUI(nil) })
			if scenario == "declined_storage_error" {
				ctx = tools.WithHumanVerificationObserver(ctx, func(_ context.Context, event tools.HumanVerificationObservation) error {
					if event.Status == "declined" {
						return errors.New("cannot persist decline")
					}
					return nil
				})
			}
			descriptor := manager.GetTools()[0]
			input := "{}"
			if strings.HasPrefix(scenario, "explicit") {
				input = `{"reason":"Verify this public security challenge"}`
				for _, tool := range manager.GetTools() {
					if strings.HasSuffix(tool.Name, "__browser_wait_for_user") {
						descriptor = tool
					}
				}
			}
			digest, err := MCPToolDescriptorSHA256(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			text, isError, err := manager.ExecuteAuthorizedTool(ctx, descriptor.Name, digest, input)
			if err != nil || isError != (scenario != "confirmed" && scenario != "readable" && scenario != "explicit" && scenario != "access_error" && scenario != "declined" && scenario != "explicit_declined_unclassified") {
				t.Fatalf("text=%s isError=%v error=%v", text, isError, err)
			}
			if scenario == "declined" || scenario == "explicit_declined_unclassified" {
				var observation chromeSourceAccess
				if json.Unmarshal([]byte(text), &observation) != nil || observation.Citable || observation.Status != "source_unavailable" {
					t.Fatalf("not an availability outcome: %s", text)
				}
				if observation.Reason != "verification_declined" {
					t.Fatalf("reason=%s", observation.Reason)
				}
			}
			if scenario == "confirmed" && (!strings.Contains(text, "Fresh public article") || strings.Contains(text, challengePageText) || captures.Load() != 2) {
				t.Fatalf("stale evidence after human confirmation: %s", text)
			}
			wantPrompts := 1
			noChallenge := scenario == "classifier_error" || scenario == "uncertain" || scenario == "readable" || scenario == "access_error"
			if scenario == "denied" || scenario == "unattended" || noChallenge {
				wantPrompts = 0
			}
			wantAuthorizations := 1
			if noChallenge {
				wantAuthorizations = 0
			}
			if waitAuthorizations != wantAuthorizations || prompts != wantPrompts || focuses.Load() != int32(wantPrompts) {
				t.Fatalf("authorizations=%d prompts=%d focuses=%d", waitAuthorizations, prompts, focuses.Load())
			}
			if scenario != "confirmed" {
				_, _, _ = manager.ExecuteAuthorizedTool(ctx, descriptor.Name, digest, "{}")
				if prompts != wantPrompts {
					t.Fatal("unresolved challenge prompted twice")
				}
			}
			if scenario == "access_error" || scenario == "declined" {
				before := captures.Load()
				result, err := manager.ExecuteAuthorizedToolResult(ctx, descriptor.Name, digest, "{}")
				var outcome chromeSourceAccess
				wantAttempts := 2
				if scenario == "declined" {
					wantAttempts = 1
				}
				if err != nil || result.IsError || result.InvocationID == "" || json.Unmarshal(result.StructuredContent, &outcome) != nil ||
					outcome.Status != "source_unavailable" || outcome.Citable || outcome.Executed || !outcome.Exhausted || outcome.Attempts != wantAttempts || captures.Load() != before {
					t.Fatalf("unavailable source was retried or lost its native status: %+v err=%v", result, err)
				}
			}
			// The model's explicit tool call must not bypass a failed automatic
			// classification or the unresolved challenge's request limit.
			for _, waitTool := range manager.GetTools() {
				if !strings.HasSuffix(waitTool.Name, "__browser_wait_for_user") {
					continue
				}
				waitDigest, err := MCPToolDescriptorSHA256(waitTool)
				if err != nil {
					t.Fatal(err)
				}
				_, waitError, err := manager.ExecuteAuthorizedTool(ctx, waitTool.Name, waitDigest, `{"reason":"Please confirm the page"}`)
				denied := scenario == "denied" && err != nil
				expectedPrompts := wantPrompts
				if scenario == "still_challenged" {
					expectedPrompts++
				}
				availability := scenario == "access_error" || scenario == "declined" || scenario == "explicit_declined_unclassified"
				if (!denied && (err != nil || waitError == availability)) || prompts != expectedPrompts {
					t.Fatalf("explicit wait bypassed classification: tool error=%v error=%v prompts=%d", waitError, err, prompts)
				}
			}
		})
	}
}

func TestChromeAuthenticationRequiresOperatorChoiceAndFreshEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, answer, reason                                                               string
		explicit, denied, unattended, noTerminal, storageFailure, stillRequired, wantError bool
	}{
		{name: "declined", reason: "authentication_declined"},
		{name: "invalid", answer: "old-code", reason: "authentication_declined"},
		{name: "confirmed", answer: "confirm"},
		{name: "explicit_confirmed", answer: "confirm", explicit: true},
		{name: "explicit_declined", explicit: true, reason: "authentication_declined"},
		{name: "still_required", answer: "confirm", stillRequired: true, reason: "authentication_required"},
		{name: "denied", answer: "confirm", denied: true, wantError: true},
		{name: "cancelled", answer: "cancel", wantError: true},
		{name: "unattended", unattended: true, reason: "authentication_required"},
		{name: "no_terminal", noTerminal: true, reason: "authentication_required"},
		{name: "decline_storage_failure", storageFailure: true, wantError: true},
		{name: "confirmation_storage_failure", answer: "confirm", storageFailure: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noTerminal {
				previous := os.Stdin
				null, err := os.Open(os.DevNull)
				if err != nil {
					t.Fatal(err)
				}
				os.Stdin = null
				t.Cleanup(func() { os.Stdin = previous; _ = null.Close() })
			}
			var loggedIn atomic.Bool
			var captures, focuses atomic.Int32
			server := newChromeVerificationServer(t, &captures, &focuses, func() (string, string) {
				if loggedIn.Load() && !tc.stillRequired {
					return "Fresh intended article after operator login", "Article"
				}
				return "Sign in to read this source", "Login"
			})
			manager := NewMCPToolManager("", "")
			t.Cleanup(func() { _ = manager.Close() })
			if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": {Type: "chrome-cdp", URL: server.URL, PageVerification: true, AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}}}); err != nil {
				t.Fatal(err)
			}
			ctx := WithChromePageClassifier(t.Context(), func(_ context.Context, page browser.Snapshot) (ChromePageState, error) {
				if strings.HasPrefix(page.Text, "Sign in") {
					return ChromePageAuthenticationRequired, nil
				}
				return ChromePageReadable, nil
			})
			ctx = context.WithValue(ctx, tools.AutoApproveKey, true)
			authorizations, prompts := 0, 0
			ctx = WithToolAuthorizer(ctx, func(_ context.Context, _, tool, _ string) error {
				if tool == "browser_wait_for_user" {
					authorizations++
					if tc.denied {
						return errors.New("operator wait denied")
					}
				}
				return nil
			})
			if tc.unattended {
				ctx = context.WithValue(ctx, tools.UnattendedKey, true)
			}
			var observations []tools.HumanVerificationObservation
			ctx = tools.WithHumanVerificationObserver(ctx, func(_ context.Context, event tools.HumanVerificationObservation) error {
				observations = append(observations, event)
				if tc.storageFailure && (event.Status == "declined" || event.Status == "confirmed") {
					return errors.New("cannot persist login decision")
				}
				return nil
			})
			tools.SetOnAskUserTUI(func(ctx context.Context, question, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
				if tc.noTerminal {
					return "", false
				}
				prompts++
				if !tools.HumanVerificationRequiresAuthentication(ctx) || !strings.Contains(question, "Would you like to log in") || !strings.Contains(question, "Never enter credentials here") || !strings.Contains(question, "https://example.test/source") || captures.Load() != 1 {
					t.Error("login decision lost its purpose, URL or initial capture")
				}
				if tools.IsAutoApprove(ctx) {
					t.Error("login decision could auto-approve")
				}
				if tc.answer == "confirm" {
					loggedIn.Store(true)
					return chromeConfirmationAnswer(question), true
				}
				answer, err := json.Marshal(tools.AskUserResponse{Free: tc.answer})
				if err != nil {
					t.Fatal(err)
				}
				return string(answer), true
			})
			t.Cleanup(func() { tools.SetOnAskUserTUI(nil) })
			toolName, input := "chrome__browser_snapshot", "{}"
			if tc.explicit {
				toolName, input = "chrome__browser_wait_for_user", `{"reason":"Read the intended source after manual login"}`
			}
			var digest, snapshotDigest string
			for _, tool := range manager.GetTools() {
				if tool.Name == toolName || tool.Name == "chrome__browser_snapshot" {
					hash, err := MCPToolDescriptorSHA256(tool)
					if err != nil {
						t.Fatal(err)
					}
					if tool.Name == toolName {
						digest = hash
					}
					if tool.Name == "chrome__browser_snapshot" {
						snapshotDigest = hash
					}
				}
			}
			result, err := manager.ExecuteAuthorizedToolResult(ctx, toolName, digest, input)
			if err != nil || result.IsError != tc.wantError || result.InvocationID == "" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			wantPrompts := 1
			if tc.denied || tc.unattended || tc.noTerminal {
				wantPrompts = 0
			}
			wantFocuses := wantPrompts
			if tc.noTerminal {
				wantFocuses = 1
			}
			if prompts != wantPrompts || focuses.Load() != int32(wantFocuses) || authorizations != 1 {
				t.Fatalf("prompts=%d focuses=%d authorizations=%d", prompts, focuses.Load(), authorizations)
			}
			for _, event := range observations {
				if event.Purpose != "authentication" || event.RequestID == "" {
					t.Fatalf("login decision lost durable metadata: %+v", event)
				}
			}
			if tc.reason != "" {
				var outcome chromeSourceAccess
				if json.Unmarshal(result.StructuredContent, &outcome) != nil || outcome.Status != "source_unavailable" || outcome.Reason != tc.reason || outcome.Citable || !outcome.Exhausted || outcome.Attempts != 1 {
					t.Fatalf("wrong unavailable outcome: %+v", result)
				}
				before := captures.Load()
				again, err := manager.ExecuteAuthorizedToolResult(ctx, toolName, digest, input)
				if err != nil || again.IsError || json.Unmarshal(again.StructuredContent, &outcome) != nil || outcome.Executed || prompts != wantPrompts || captures.Load() != before {
					t.Fatalf("declined source was prompted or read again: %+v %v", again, err)
				}
			} else if !tc.wantError {
				if captures.Load() != 2 {
					t.Fatal("operator handoff did not recapture")
				}
				if tc.explicit {
					result, err = manager.ExecuteAuthorizedToolResult(ctx, "chrome__browser_snapshot", snapshotDigest, "{}")
					if err != nil || result.IsError || captures.Load() != 3 || prompts != 1 {
						t.Fatalf("handoff snapshot failed: %+v %v", result, err)
					}
				}
				text, _, err := textOnlyToolResult(result)
				if err != nil || !strings.Contains(text, "Fresh intended article") || strings.Contains(text, "Sign in to read") {
					t.Fatalf("stale login page became evidence: %+v", result)
				}
			}
		})
	}
}

func newChromeVerificationServer(t *testing.T, captures, focuses *atomic.Int32, capture func() (string, string)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://` + r.Host + `/devtools/browser/test"}`))
	})
	mux.HandleFunc("/devtools/browser/test", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var call struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(data, &call); err != nil {
				t.Error(err)
				return
			}
			var result any = map[string]any{}
			switch call.Method {
			case "Target.createTarget":
				result = map[string]string{"targetId": "owned"}
			case "Target.attachToTarget":
				result = map[string]string{"sessionId": "session"}
			case "Target.getTargetInfo":
				result = map[string]any{"targetInfo": map[string]string{"url": "https://example.test/source"}}
			case "Page.bringToFront":
				focuses.Add(1)
			case "Page.getFrameTree":
				result = map[string]any{"frameTree": map[string]any{"frame": map[string]string{"id": "main", "loaderId": "loader", "url": "https://example.test/source"}}}
			case "Page.createIsolatedWorld":
				result = map[string]int{"executionContextId": 42}
			case "Runtime.evaluate":
				captures.Add(1)
				text, title := capture()
				result = map[string]any{"result": map[string]any{"value": map[string]any{"url": "https://example.test/source", "title": title, "text": text, "buttons": []any{}}}}
			}
			response, _ := json.Marshal(map[string]any{"id": call.ID, "result": result})
			if err := ws.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
