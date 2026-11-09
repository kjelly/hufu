//go:build linux

package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Opt-in protocol compatibility check against a disposable local Chrome.
// Unit tests use the CDP fixture and require no installed browser.
func TestCDPChromeIntegration(t *testing.T) {
	if os.Getenv("HUFU_TEST_CHROME") != "1" {
		t.Skip("set HUFU_TEST_CHROME=1 to test installed Chrome")
	}
	executable, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Fatal(err)
	}
	// Go 1.27 t.TempDir follows GOTMPDIR. Keep the disposable live browser
	// profile under this task's private TMPDIR, outside shared build caches.
	profile, err := os.MkdirTemp(os.TempDir(), "chrome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(profile); err != nil {
			t.Error(err)
		}
	})
	log, err := os.Create(filepath.Join(profile, "chrome.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// The profile is empty and contains no credentials. Avoid waiting for a
	// desktop keyring while Chrome initializes the fixture's cookie store.
	cmd := exec.CommandContext(ctx, executable, "--headless=new", "--no-sandbox", "--disable-dev-shm-usage", "--password-store=basic", "--no-first-run", "--no-default-browser-check", "--user-data-dir="+profile, "--remote-debugging-port=0", "about:blank")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(profile, "chrome.log"))
			if len(data) > 8192 {
				data = data[len(data)-8192:]
			}
			t.Logf("Chrome stderr: %s", data)
		}
	})
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); cancel(); _ = cmd.Wait() })
	var port string
	for port == "" {
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err == nil {
			port, _, _ = strings.Cut(string(data), "\n")
		}
		if port != "" {
			break
		}
		select {
		case <-ctx.Done():
			data, _ := os.ReadFile(filepath.Join(profile, "chrome.log"))
			t.Fatalf("Chrome did not become ready: %v\n%s", ctx.Err(), data)
		case <-time.After(20 * time.Millisecond):
		}
	}
	var requests atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Native CDP fixture</title></head><body><p>Original source 原文</p><a href="/next">Next source</a><div id="popup" role="dialog" style="position:fixed;inset:0;background:white;z-index:1000"><p>Subscription popup</p><button aria-label="關閉彈窗" onclick="document.querySelector('#popup').remove();document.querySelector('p').textContent='Fresh article after closing popup'">×</button></div><form><button>Submit form</button></form><button disabled>Disabled control</button></body></html>`))
	}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("local fixture HTTP requests: %d", requests.Load())
		}
	})
	defer s.Close()
	b, err := NewCDP("http://127.0.0.1:" + port)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := b.Navigate(ctx, s.URL); err != nil {
		t.Fatal(err)
	}
	page, err := b.Snapshot(ctx)
	if err != nil || page.URL != s.URL+"/" || page.Title != "Native CDP fixture" || !strings.Contains(page.Text, "Original source 原文") || len(page.Links) != 1 || page.Links[0].URL != s.URL+"/next" {
		t.Fatalf("snapshot=%+v error=%v", page, err)
	}
	buttonRef := func(page Snapshot, name string) string {
		t.Helper()
		for _, button := range page.Buttons {
			if button.Name == name {
				return button.Ref
			}
		}
		t.Fatalf("button %q missing from snapshot: %+v", name, page.Buttons)
		return ""
	}
	if err := b.Click(ctx, buttonRef(page, "Submit form")); err == nil {
		t.Fatal("form submission admitted")
	}
	page, err = b.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := buttonRef(page, "關閉彈窗")
	if err := b.Click(ctx, ref); err != nil {
		t.Fatal(err)
	}
	page, err = b.Snapshot(ctx)
	if err != nil || !strings.Contains(page.Text, "Fresh article after closing popup") || strings.Contains(page.Text, "Subscription popup") {
		t.Fatalf("post-click snapshot=%+v error=%v", page, err)
	}
	if err := b.Click(ctx, ref); err == nil {
		t.Fatal("consumed button reference replayed")
	}
	if err := b.Focus(ctx); err != nil {
		t.Fatal(err)
	}
	if url, err := b.CurrentURL(ctx); err != nil || url != s.URL+"/" {
		t.Fatalf("target metadata: %q (%v)", url, err)
	}
	// Simulate a change made in Chrome outside Hufu's model-facing tool set.
	c, err := b.page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = c.call(ctx, "Runtime.evaluate", map[string]any{"expression": `document.querySelector('p').textContent = 'Verification completed by operator'; Object.prototype.toJSON = function() { return {url: 'https://forged.invalid/', text: 'forged page output'}; }`}, nil)
	_ = c.ws.CloseNow()
	if err != nil {
		t.Fatal(err)
	}
	page, err = b.Snapshot(ctx)
	if err != nil || page.URL != s.URL+"/" || !strings.Contains(page.Text, "Verification completed by operator") {
		t.Fatalf("manual change missing: %+v (%v)", page, err)
	}
	if err := b.Navigate(ctx, s.URL+"/#source"); err != nil {
		t.Fatal(err)
	}
	page, err = b.Snapshot(ctx)
	if err != nil || page.URL != s.URL+"/#source" {
		t.Fatalf("fragment identity lost: %+v (%v)", page, err)
	}
}
