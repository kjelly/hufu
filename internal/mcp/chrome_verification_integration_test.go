package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/tools"
)

func TestChromeSnapshotAutomaticallyRequestsAuthorizedHumanVerification(t *testing.T) {
	for _, scenario := range []string{"confirmed", "explicit", "denied", "unattended", "cancelled", "still_challenged", "classifier_error", "uncertain", "readable"} {
		t.Run(scenario, func(t *testing.T) {
			var verified atomic.Bool
			var captures, focuses atomic.Int32
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
						text, title := challengePageText, "Just a moment..."
						if verified.Load() {
							text, title = "Fresh public article after verification", "Article"
						}
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
			manager := NewMCPToolManager("", "")
			t.Cleanup(func() { _ = manager.Close() })
			cfg := MCPServerConfig{Type: "chrome-cdp", URL: server.URL, PageVerification: true, AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}}
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
				if !strings.Contains(question, "https://example.test/source") || captures.Load() != 1 {
					t.Error("human prompt lost actual target or capture order")
				}
				if scenario == "explicit" && !strings.Contains(question, "Verify this public security challenge") {
					t.Error("explicit wait lost its authorized reason")
				}
				if scenario == "cancelled" {
					return `{"answers":["cancel"]}`, true
				}
				if scenario != "still_challenged" {
					verified.Store(true)
				}
				return `{"answers":["continue"]}`, true
			})
			t.Cleanup(func() { tools.SetOnAskUserTUI(nil) })
			descriptor := manager.GetTools()[0]
			input := "{}"
			if scenario == "explicit" {
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
			if err != nil || isError != (scenario != "confirmed" && scenario != "readable" && scenario != "explicit") {
				t.Fatalf("text=%s isError=%v error=%v", text, isError, err)
			}
			if scenario == "confirmed" && (!strings.Contains(text, "Fresh public article") || strings.Contains(text, challengePageText) || captures.Load() != 2) {
				t.Fatalf("stale evidence after human confirmation: %s", text)
			}
			wantPrompts := 1
			noChallenge := scenario == "classifier_error" || scenario == "uncertain" || scenario == "readable"
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
				if (!denied && (err != nil || !waitError)) || prompts != wantPrompts {
					t.Fatalf("explicit wait bypassed classification: tool error=%v error=%v prompts=%d", waitError, err, prompts)
				}
			}
		})
	}
}
