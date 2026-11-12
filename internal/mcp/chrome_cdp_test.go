package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/tools"
)

func TestChromeCDPHumanWaitOutlivesDefaultToolTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tools.SetOnAskUserTUI(func(ctx context.Context, question, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
			time.Sleep(time.Hour)
			if ctx.Err() != nil {
				t.Fatal("MCP timeout expired during manual verification")
			}
			return chromeConfirmationAnswer(question), true
		})
		defer tools.SetOnAskUserTUI(nil)
		fake := newRuntimeTestServer(t, nil)
		fake.server.AddTool(mcpgo.NewTool("browser_wait_for_user"), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if err := tools.WaitForHumanVerification(ctx, "Verify"); err != nil {
				return nil, err
			}
			return mcpgo.NewToolResultText("confirmed"), nil
		})
		manager := NewMCPToolManager("", "")
		attachRuntimeTestServer(t, manager, "chrome", MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222"}, fake)
		text, isError, err := manager.ExecuteTool(t.Context(), "chrome__browser_wait_for_user", "{}")
		if err != nil || isError || text != "confirmed" {
			t.Fatalf("text=%s isError=%t error=%v", text, isError, err)
		}
	})
}

func TestChromeCDPShutdownCancelsInFlightIO(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer s.Close()
	manager := NewMCPToolManager("", "")
	defer manager.Close()
	if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": {Type: "chrome-cdp", URL: s.URL}}); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _, _ = manager.ExecuteTool(t.Context(), "chrome__browser_snapshot", "{}")
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("CDP request never started")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown left CDP request running")
	}
}

func TestChromeCDPConfigurationAndCatalog(t *testing.T) {
	for _, scenario := range []string{"valid", "remote", "command", "environment", "temporary", "oauth"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222"}
			switch scenario {
			case "remote":
				cfg.URL = "http://192.0.2.1:9222"
			case "command":
				cfg.Command = []string{"npx"}
			case "environment":
				cfg.Environment = map[string]string{"TOKEN": "not-a-real-credential"}
			case "temporary":
				cfg.TemporaryDirectoryEnv = []string{"OUTPUT"}
			case "oauth":
				cfg.NoOAuth = true
			}
			if err := ValidateWorkerToolPolicies(cfg); (err == nil) != (scenario == "valid") {
				t.Fatalf("validation: %v", err)
			}
			if scenario != "valid" {
				return
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip MCPServerConfig
			if err := json.Unmarshal(data, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if roundtrip.Type != cfg.Type || roundtrip.URL != cfg.URL {
				t.Fatal("lost native connection configuration")
			}
			manager := NewMCPToolManager("", "")
			defer manager.Close()
			// Loading and listing launch no process and require no live Chrome.
			if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": roundtrip}); err != nil {
				t.Fatal(err)
			}
			want := []string{"chrome__browser_click", "chrome__browser_navigate", "chrome__browser_snapshot", "chrome__browser_wait_for_user"}
			if got := descriptorNames(manager.GetTools()); !slices.Equal(got, want) {
				t.Fatalf("tools=%v", got)
			}
			if !manager.GetTools()[3].pausableTimeout || manager.GetTools()[0].pausableTimeout {
				t.Fatal("native human wait identity was not preserved in catalog")
			}
		})
	}
}

func TestChromeClickPolicyRejectsReadOnlyAndExternalImpersonation(t *testing.T) {
	for _, scenario := range []string{"native", "read_only", "external", "wrong_tool", "missing_snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			policy := WorkerToolPolicy{SideEffect: "browser_interaction", Filesystem: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}}
			cfg := MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222", ToolPolicies: map[string]WorkerToolPolicy{"browser_click": policy}}
			switch scenario {
			case "read_only":
				policy.SideEffect = "none"
				cfg.ToolPolicies["browser_click"] = policy
			case "external":
				cfg.Type = "remote"
			case "wrong_tool":
				cfg.ToolPolicies = map[string]WorkerToolPolicy{"browser_snapshot": policy}
			case "missing_snapshot":
				cfg.AllowedTools = []string{"browser_click"}
			}
			if err := ValidateWorkerToolPolicies(cfg); (err == nil) != (scenario == "native") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestChromeCDPAuthorizationAndEvidence(t *testing.T) {
	var commands, closed atomic.Int64
	var snapshotReads atomic.Int64
	var brokenDocument atomic.Bool
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
			commands.Add(1)
			var result any = map[string]any{}
			switch call.Method {
			case "Target.createTarget":
				result = map[string]string{"targetId": "owned"}
			case "Target.attachToTarget":
				result = map[string]string{"sessionId": "session"}
			case "Target.getTargetInfo":
				result = map[string]any{"targetInfo": map[string]string{"url": "https://example.test/source"}}
			case "Page.getFrameTree":
				result = map[string]any{"frameTree": map[string]any{"frame": map[string]string{"id": "main", "loaderId": "source-loader", "url": "https://example.test/source"}}}
			case "Page.createIsolatedWorld":
				result = map[string]int{"executionContextId": 42}
			case "Runtime.evaluate":
				snapshotReads.Add(1)
				result = map[string]any{"result": map[string]any{"value": map[string]any{"url": "https://example.test/source", "title": "Source", "text": "Verbatim source quote", "links": []map[string]string{{"text": "Next source", "url": "https://example.test/next"}}}}}
			case "Target.closeTarget":
				closed.Add(1)
			}
			payload := map[string]any{"id": call.ID, "result": result}
			if call.Method == "Runtime.evaluate" && brokenDocument.Load() {
				payload = map[string]any{"id": call.ID, "error": map[string]string{"message": "document unavailable"}}
			}
			response, _ := json.Marshal(payload)
			if err := ws.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	})
	s := httptest.NewServer(mux)
	defer s.Close()
	manager := NewMCPToolManager("", "")
	defer manager.Close()
	policy := WorkerToolPolicy{SideEffect: "none", Filesystem: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}}
	cfg := MCPServerConfig{Type: "chrome-cdp", URL: s.URL, AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}, ToolPolicies: map[string]WorkerToolPolicy{"browser_snapshot": policy}}
	if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": cfg}); err != nil {
		t.Fatal(err)
	}
	descriptor := manager.GetTools()[0]
	hash, err := MCPToolDescriptorSHA256(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithToolAuthorizer(t.Context(), func(context.Context, string, string, string) error { return errors.New("denied") })
	if _, _, err := manager.ExecuteAuthorizedTool(ctx, descriptor.Name, hash, "{}"); !IsToolAuthorizationError(err) || commands.Load() != 0 {
		t.Fatalf("denial reached Chrome: %v", err)
	}
	if _, _, err := manager.ExecuteTool(t.Context(), descriptor.Name, "{}"); !IsToolAuthorizationError(err) {
		t.Fatal("worker policy was bypassed")
	}
	ctx = WithToolAuthorizer(t.Context(), allowAll)
	result, err := manager.ExecuteAuthorizedToolResult(ctx, descriptor.Name, hash, "{}")
	if err != nil || result.IsError || result.InvocationID == "" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	text, _, err := textOnlyToolResult(result)
	if err != nil || !strings.Contains(text, "Page URL: https://example.test/source") || !strings.Contains(text, "```yaml\n") || !strings.Contains(text, "Verbatim source quote") || !strings.Contains(text, "https://example.test/next") {
		t.Fatalf("evidence lost: %s (%v)", text, err)
	}
	before := commands.Load()
	unattended := context.WithValue(t.Context(), tools.UnattendedKey, true)
	if _, isError, err := manager.ExecuteTool(unattended, "chrome__browser_wait_for_user", `{"reason":"challenge"}`); err != nil || !isError || commands.Load() != before {
		t.Fatalf("unattended verification: error=%v isError=%v", err, isError)
	}
	if _, _, err := manager.ExecuteTool(t.Context(), "chrome__browser_navigate", `{"url":"https://example.test"}`); err == nil {
		t.Fatal("excluded tool was callable")
	}
	brokenDocument.Store(true)
	if failed, err := manager.ExecuteAuthorizedToolResult(ctx, descriptor.Name, hash, "{}"); err != nil || !failed.IsError {
		t.Fatalf("unreadable document: %+v (%v)", failed, err)
	}
	readsBeforeWait := snapshotReads.Load()
	tools.SetOnAskUserTUI(func(_ context.Context, question, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
		if !strings.Contains(question, "https://example.test/source") {
			t.Error("manual prompt lost actual target URL")
		}
		brokenDocument.Store(false)
		return chromeConfirmationAnswer(question), true
	})
	defer tools.SetOnAskUserTUI(nil)
	if _, isError, err := manager.ExecuteTool(t.Context(), "chrome__browser_wait_for_user", `{"reason":"manual verification"}`); err != nil || isError || snapshotReads.Load() != readsBeforeWait {
		t.Fatalf("manual intervention depended on document reads: error=%v isError=%t", err, isError)
	}
	if fresh, err := manager.ExecuteAuthorizedToolResult(ctx, descriptor.Name, hash, "{}"); err != nil || fresh.IsError {
		t.Fatalf("post-verification snapshot: %+v (%v)", fresh, err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if closed.Load() != 1 {
		t.Fatalf("closed tabs=%d", closed.Load())
	}
}

// Type the freshly displayed code, never synthesize a preselected choice.
func chromeConfirmationAnswer(question string) string {
	_, code, _ := strings.Cut(question, "\nConfirmation code: ")
	answer, _ := json.Marshal(map[string]string{"free_text": code})
	return string(answer)
}
