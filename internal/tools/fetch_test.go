//go:build linux || darwin

package tools

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestConvertHTMLToMarkdown(t *testing.T) {
	input := `<html><head><style>hidden-style</style></head><body>
<h1>文件</h1><p>Hello &amp; <strong>world</strong>.</p>
<p><a href="/guide">Guide</a> <code>a &lt; b</code></p>
<ul><li>First</li><li>Second</li></ul>
<script>hidden-script</script><textarea>hidden-input</textarea>
</body></html>`
	got, err := convertHTMLToMarkdown(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# 文件", "Hello &", "**world**", "[Guide](/guide)", "`a < b`", "First", "Second"} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"hidden-style", "hidden-script", "hidden-input", "<h1>", "<strong>"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("markdown contains %q: %q", unwanted, got)
		}
	}
	if text := extractTextFromHTML(input); text != got {
		t.Errorf("text conversion = %q, want %q", text, got)
	}
}

func TestFetchContentFormats(t *testing.T) {
	const html = `<h1>文件</h1><p>Hello &amp; <strong>world</strong>.</p><script>hidden-script</script>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"value":"<strong>raw</strong>"}`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, html)
	}))
	defer server.Close()

	for _, tt := range []struct {
		name, path, format, want string
	}{
		{name: "default", path: "/", want: "**world**"},
		{name: "markdown", path: "/", format: "markdown", want: "**world**"},
		{name: "text", path: "/", format: "text", want: "**world**"},
		{name: "html", path: "/", format: "html", want: html},
		{name: "non_html", path: "/json", format: "markdown", want: `{"value":"<strong>raw</strong>"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input, err := json.Marshal(fetchArgs{URL: server.URL + tt.path, Format: tt.format})
			if err != nil {
				t.Fatal(err)
			}
			response, err := executeFetch(t.Context(), fantasy.ToolCall{Input: string(input)}, "")
			if err != nil {
				t.Fatal(err)
			}
			if response.IsError || !strings.Contains(response.Content, tt.want) {
				t.Fatalf("unexpected response: %+v", response)
			}
			if tt.format != "html" && strings.Contains(response.Content, "hidden-script") {
				t.Fatalf("script leaked into converted content: %q", response.Content)
			}
		})
	}
}
