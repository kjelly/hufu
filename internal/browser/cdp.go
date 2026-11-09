// Package browser connects to an operator-owned Chrome without launching a
// browser or accepting model-supplied JavaScript or CDP commands.
package browser

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/kjelly/hufu/internal/waitbudget"
)

const maxResponse = 4 << 20

// CDP owns one fresh tab per declaration, never an existing personal tab.
// Operations are serialized by the caller. Connections are request-scoped.
type CDP struct {
	endpoint *url.URL
	http     *http.Client
	targetID string
	buttons  map[string]buttonTarget
}

// ValidateEndpoint restricts debugging authority to an explicit local endpoint.
func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("chrome CDP requires http://127.0.0.1:<port> or http://[::1]:<port>")
	}
	ip := net.ParseIP(u.Hostname())
	port, portErr := strconv.Atoi(u.Port())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("chrome CDP requires a loopback HTTP URL with an explicit port and no credentials, path, query or fragment")
	}
	return nil
}

func NewCDP(endpoint string) (*CDP, error) {
	if err := ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	u, _ := url.Parse(endpoint)
	return &CDP{endpoint: u, http: &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("CDP redirects are not allowed") },
	}}, nil
}

type connection struct {
	ws      *websocket.Conn
	id      int
	session string
}

type frameIdentity struct {
	ID       string `json:"id"`
	Loader   string `json:"loaderId"`
	URL      string `json:"url"`
	Fragment string `json:"urlFragment"`
}

func (c *connection) frame(ctx context.Context) (frameIdentity, error) {
	var result struct {
		Tree struct {
			Frame frameIdentity `json:"frame"`
		} `json:"frameTree"`
	}
	err := c.call(ctx, "Page.getFrameTree", map[string]any{}, &result)
	return result.Tree.Frame, err
}

func (c *connection) call(ctx context.Context, method string, params any, result any) error {
	c.id++
	request, err := json.Marshal(struct {
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
		Session string `json:"sessionId,omitempty"`
	}{c.id, method, params, c.session})
	if err != nil {
		return err
	}
	if err := c.ws.Write(ctx, websocket.MessageText, request); err != nil {
		return fmt.Errorf("CDP %s request: %w", method, err)
	}
	for {
		_, payload, err := c.ws.Read(ctx)
		if err != nil {
			return fmt.Errorf("CDP %s response: %w", method, err)
		}
		var response struct {
			ID     int            `json:"id"`
			Result jsontext.Value `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &response); err != nil {
			return err
		}
		if response.ID != c.id {
			continue
		} // CDP lifecycle events are unsolicited.
		if response.Error != nil {
			return fmt.Errorf("CDP %s: %s", method, response.Error.Message)
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(response.Result, result)
	}
}

func (b *CDP) connect(ctx context.Context) (*connection, error) {
	u := b.endpoint.Clone()
	u.Path = "/json/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connect Chrome CDP; start Chrome with --remote-debugging-port and a dedicated --user-data-dir: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chrome CDP discovery returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, errors.New("chrome CDP discovery exceeds 64 KiB")
	}
	var version struct {
		WebSocketURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return nil, err
	}
	wsURL, err := url.Parse(version.WebSocketURL)
	if err != nil || wsURL.Scheme != "ws" || wsURL.Host != b.endpoint.Host || wsURL.User != nil || wsURL.RawQuery != "" || wsURL.Fragment != "" || !strings.HasPrefix(wsURL.Path, "/devtools/browser/") {
		return nil, errors.New("chrome returned a debugging socket outside the configured endpoint")
	}
	ws, _, err := websocket.Dial(ctx, wsURL.String(), &websocket.DialOptions{HTTPClient: b.http})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxResponse)
	return &connection{ws: ws}, nil
}

func (b *CDP) page(ctx context.Context) (*connection, error) {
	c, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.ws.CloseNow()
		}
	}()
	if b.targetID == "" {
		var target struct {
			ID string `json:"targetId"`
		}
		if err := c.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
			return nil, err
		}
		if target.ID == "" {
			return nil, errors.New("chrome created no tab")
		}
		b.targetID = target.ID
	}
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := c.call(ctx, "Target.attachToTarget", map[string]any{"targetId": b.targetID, "flatten": true}, &session); err != nil {
		return nil, err
	}
	if session.ID == "" {
		return nil, errors.New("chrome attached no session")
	}
	c.session = session.ID
	ok = true
	return c, nil
}

// Navigate admits only HTTP(S) URLs. Site redirects and subresources retain
// Chrome's network authority; this is not a browser network sandbox.
func (b *CDP) Navigate(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || len(raw) > 4096 {
		return errors.New("browser navigation requires an HTTP(S) URL without credentials, at most 4096 bytes")
	}
	b.buttons = nil
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(30*time.Second))
	defer cancel()
	c, err := b.page(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.ws.CloseNow() }() // Disconnecting does not close Chrome.
	var navigation struct {
		Error  string `json:"errorText"`
		Loader string `json:"loaderId"`
	}
	if err := c.call(ctx, "Page.navigate", map[string]any{"url": raw}, &navigation); err != nil {
		return err
	}
	if navigation.Error != "" {
		return fmt.Errorf("chrome navigation: %s", navigation.Error)
	}
	for {
		frame, err := c.frame(ctx)
		if err != nil {
			return err
		}
		if navigation.Loader == "" || frame.Loader == navigation.Loader {
			var ready struct {
				Result struct {
					Value string `json:"value"`
				} `json:"result"`
			}
			if err := c.call(ctx, "Runtime.evaluate", map[string]any{"expression": "document.readyState", "returnByValue": true}, &ready); err != nil {
				return err
			}
			if ready.Result.Value == "interactive" || ready.Result.Value == "complete" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type Snapshot struct {
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Text       string    `json:"text"`
	Truncated  bool      `json:"truncated"`
	Links      []Link    `json:"links"`
	Buttons    []Button  `json:"buttons"`
	CapturedAt time.Time `json:"captured_at"`
}

type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Snapshot reads the current tab, including changes made by the operator.
// The fixed expression exposes no model-controlled script execution.
func (b *CDP) Snapshot(ctx context.Context) (Snapshot, error) {
	b.buttons = nil
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(30*time.Second))
	defer cancel()
	c, err := b.page(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = c.ws.CloseNow() }()
	before, err := c.frame(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	pageURL := before.URL + before.Fragment
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return Snapshot{}, errors.New("current Chrome tab is not an HTTP(S) page")
	}
	var world struct {
		ID int `json:"executionContextId"`
	}
	if err := c.call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": before.ID, "worldName": "hufu-observation"}, &world); err != nil {
		return Snapshot{}, err
	}
	if world.ID == 0 {
		return Snapshot{}, errors.New("chrome created no observation context")
	}
	var result struct {
		Result struct {
			Value Snapshot `json:"value"`
		} `json:"result"`
		Exception jsontext.Value `json:"exceptionDetails"`
	}
	const expression = `(() => { const text = document.body?.innerText ?? ''; const links = Array.from(document.querySelectorAll('a[href]')).filter(a => /^https?:/.test(a.href) && a.href.length <= 4096 && a.getClientRects().length).slice(0, 128).map(a => ({text: (a.innerText || a.getAttribute('aria-label') || '').slice(0, 512), url: a.href})); return {title: document.title, text: text.slice(0, 262144), truncated: text.length > 262144, links}; })()`
	if err := c.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "contextId": world.ID}, &result); err != nil {
		return Snapshot{}, err
	}
	if len(result.Exception) > 0 {
		return Snapshot{}, errors.New("chrome could not read the current page")
	}
	buttons, targets, err := readButtons(ctx, c, before)
	if err != nil {
		return Snapshot{}, err
	}
	after, err := c.frame(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if before != after {
		return Snapshot{}, errors.New("chrome document changed during snapshot; take a fresh snapshot")
	}
	result.Result.Value.URL = pageURL
	result.Result.Value.Buttons = buttons
	result.Result.Value.CapturedAt = time.Now().UTC()
	b.buttons = targets
	return result.Result.Value, nil
}

// Focus identifies the exact tab the operator should verify.
func (b *CDP) Focus(ctx context.Context) error {
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(5*time.Second))
	defer cancel()
	c, err := b.page(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.ws.CloseNow() }()
	return c.call(ctx, "Page.bringToFront", map[string]any{}, nil)
}

// CurrentURL reads browser-owned target metadata without executing page script.
// Human intervention must remain available when the document cannot be read.
func (b *CDP) CurrentURL(ctx context.Context) (string, error) {
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(5*time.Second))
	defer cancel()
	c, err := b.page(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.ws.CloseNow() }()
	c.session = ""
	var info struct {
		Target struct {
			URL string `json:"url"`
		} `json:"targetInfo"`
	}
	if err := c.call(ctx, "Target.getTargetInfo", map[string]any{"targetId": b.targetID}, &info); err != nil {
		return "", err
	}
	u, err := url.Parse(info.Target.URL)
	if err != nil || u.User != nil || (((u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "") && info.Target.URL != "about:blank") {
		return "", errors.New("verification tab is not an HTTP(S) page or a fresh blank tab")
	}
	return info.Target.URL, nil
}

// Close closes only the tab created by this instance, never the browser.
func (b *CDP) Close() error {
	b.buttons = nil
	defer b.http.CloseIdleConnections()
	if b.targetID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.ws.CloseNow() }()
	err = c.call(ctx, "Target.closeTarget", map[string]any{"targetId": b.targetID}, nil)
	if err == nil {
		b.targetID = ""
	}
	return err
}
