package browser

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestValidateEndpoint(t *testing.T) {
	for _, tt := range []struct {
		url   string
		valid bool
	}{
		{"http://127.0.0.1:9222", true}, {"http://[::1]:9222/", true},
		{"http://localhost:9222", false}, {"http://192.0.2.1:9222", false},
		{"http://127.0.0.1", false}, {"ws://127.0.0.1:9222", false},
		{"http://127.0.0.1:0", false}, {"http://127.0.0.1:65536", false},
		{"http://user:password@127.0.0.1:9222", false},
		{"http://127.0.0.1:9222/json", false}, {"http://127.0.0.1:9222/?secret=yes", false},
		{"http://127.0.0.1:9222/#fragment", false}, {"%", false},
	} {
		t.Run(tt.url, func(t *testing.T) {
			if err := ValidateEndpoint(tt.url); (err == nil) != tt.valid {
				t.Fatalf("valid=%t, error=%v", tt.valid, err)
			}
		})
	}
}

type cdpFixture struct {
	mu              sync.Mutex
	methods         []string
	created, closed int
	page            Snapshot
	fail            string
	discovery       string
	frameURL        string
	changeDocument  bool
	frameReads      int
	axNodes         []map[string]any
	clicks          int
	obstructed      bool
}

func newCDPFixture(t *testing.T) (*CDP, *cdpFixture) {
	t.Helper()
	f := &cdpFixture{page: Snapshot{URL: "https://example.test/article", Title: "Title", Text: "Original quote\n第二段"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		discovery := f.discovery
		f.mu.Unlock()
		if discovery == "" {
			discovery = "ws://" + r.Host + "/devtools/browser/test"
		}
		payload, _ := json.Marshal(map[string]string{"webSocketDebuggerUrl": discovery})
		_, _ = w.Write(payload)
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
			var request struct {
				ID      int            `json:"id"`
				Method  string         `json:"method"`
				Params  map[string]any `json:"params"`
				Session string         `json:"sessionId"`
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
				return
			}
			f.mu.Lock()
			f.methods = append(f.methods, request.Method)
			var result any = map[string]any{}
			switch request.Method {
			case "Target.createTarget":
				f.created++
				result = map[string]string{"targetId": fmt.Sprintf("owned-%d", f.created)}
			case "Target.attachToTarget":
				result = map[string]string{"sessionId": "session"}
			case "Target.getTargetInfo":
				result = map[string]any{"targetInfo": map[string]string{"url": f.page.URL}}
			case "Page.navigate":
				result = map[string]string{"loaderId": "new-loader"}
			case "Page.getFrameTree":
				f.frameReads++
				frameURL := f.frameURL
				if frameURL == "" {
					frameURL = f.page.URL
				}
				loader := "new-loader"
				if f.changeDocument && f.frameReads > 1 {
					loader = "changed-loader"
				}
				result = map[string]any{"frameTree": map[string]any{"frame": map[string]string{"id": "main", "loaderId": loader, "url": frameURL}}}
			case "Page.createIsolatedWorld":
				result = map[string]int{"executionContextId": 42}
			case "Runtime.evaluate":
				if request.Params["expression"] == "document.readyState" {
					result = map[string]any{"result": map[string]any{"value": "complete"}}
				} else {
					if request.Params["contextId"] != float64(42) {
						t.Error("snapshot did not use the isolated observation context")
					}
					result = map[string]any{"result": map[string]any{"value": f.page}}
				}
			case "Accessibility.getFullAXTree":
				result = map[string]any{"nodes": f.axNodes}
			case "DOM.resolveNode":
				result = map[string]any{"object": map[string]string{"objectId": "button-object"}}
			case "Runtime.callFunctionOn":
				result = map[string]any{"result": map[string]any{"value": map[string]any{"x": 12, "y": 24, "valid": true}}}
				if f.obstructed {
					result = map[string]any{"exceptionDetails": map[string]string{"text": "obscured"}}
				}
			case "Input.dispatchMouseEvent":
				if request.Params["type"] == "mouseReleased" {
					f.clicks++
					f.page.Text = "Fresh article after dismissing popup"
				}
			case "Target.closeTarget":
				f.closed++
				if !strings.HasPrefix(fmt.Sprint(request.Params["targetId"]), "owned-") {
					t.Error("closed a foreign tab")
				}
			}
			response := map[string]any{"id": request.ID, "result": result}
			if request.Method == f.fail {
				response = map[string]any{"id": request.ID, "error": map[string]string{"message": "fixture failure"}}
			}
			f.mu.Unlock()
			// Real CDP interleaves unsolicited events with responses.
			_ = ws.Write(r.Context(), websocket.MessageText, []byte(`{"method":"Page.frameNavigated","params":{}}`))
			encoded, _ := json.Marshal(response)
			if err := ws.Write(r.Context(), websocket.MessageText, encoded); err != nil {
				return
			}
		}
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	b, err := NewCDP(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b, f
}

func TestCDPOwnsTabAndReadsManualChanges(t *testing.T) {
	b, f := newCDPFixture(t)
	if err := b.Navigate(t.Context(), "https://example.test/article"); err != nil {
		t.Fatal(err)
	}
	page, err := b.Snapshot(t.Context())
	if err != nil || page.Text != "Original quote\n第二段" {
		t.Fatalf("snapshot=%+v, error=%v", page, err)
	}
	f.mu.Lock()
	f.page.URL = "https://example.test/verified"
	f.page.Text = "Manually verified"
	f.mu.Unlock()
	page, err = b.Snapshot(t.Context())
	if err != nil || page.URL != "https://example.test/verified" || page.Text != "Manually verified" {
		t.Fatalf("snapshot=%+v, error=%v", page, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created != 1 || f.closed != 1 {
		t.Fatalf("created=%d closed=%d", f.created, f.closed)
	}
}

func TestCDPSnapshotBindsBrowserDocumentIdentity(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			b, f := newCDPFixture(t)
			f.mu.Lock()
			f.page.URL = "https://forged.invalid/claimed-source"
			f.frameURL = "https://example.test/actual-source"
			f.changeDocument = changed
			f.mu.Unlock()
			page, err := b.Snapshot(t.Context())
			if changed {
				if err == nil {
					t.Fatal("snapshot admitted content across different document loaders")
				}
			} else if err != nil || page.URL != "https://example.test/actual-source" {
				t.Fatalf("snapshot trusted a page-supplied URL: %+v (%v)", page, err)
			}
		})
	}
}

func TestCDPRejectsInvalidNavigationBeforeConnecting(t *testing.T) {
	b, err := NewCDP("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://user:secret@example.test/", "https://", strings.Repeat("x", 4097)} {
		if err := b.Navigate(t.Context(), raw); err == nil || b.targetID != "" {
			t.Fatalf("admitted %q: %v", raw, err)
		}
	}
}

func TestCDPFailuresRemainErrors(t *testing.T) {
	for _, scenario := range []string{"discovery_escape", "protocol", "non_web_page", "malformed_web_page", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			b, f := newCDPFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.mu.Lock()
			switch scenario {
			case "discovery_escape":
				f.discovery = "ws://192.0.2.1:9222/devtools/browser/foreign"
			case "protocol":
				f.fail = "Runtime.evaluate"
			case "non_web_page":
				f.page.URL = "file:///private"
			case "malformed_web_page":
				f.page.URL = "https:missing-host"
			case "cancelled":
				cancel()
			}
			f.mu.Unlock()
			_, err := b.Snapshot(ctx)
			if err == nil {
				t.Fatal("failure became a successful snapshot")
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if scenario == "protocol" {
				url, err := b.CurrentURL(t.Context())
				if err != nil || url != "https://example.test/article" {
					t.Fatalf("manual verification metadata depends on broken page script: %q (%v)", url, err)
				}
			}
			if scenario == "non_web_page" || scenario == "malformed_web_page" {
				f.mu.Lock()
				read := slices.Contains(f.methods, "Runtime.evaluate")
				f.mu.Unlock()
				if read {
					t.Fatal("read document content before admitting the browser-owned URL")
				}
			}
		})
	}
}

func TestCDPReadHonorsCancellation(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /json/version", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	s := httptest.NewServer(mux)
	defer s.Close()
	b, err := NewCDP(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.Snapshot(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
