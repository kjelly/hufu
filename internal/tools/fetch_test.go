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
<p><a href="/guide">Guide</a> <a href="sub.html">Sub</a> <a href="https://other.example/x">Other</a> <code>a &lt; b</code></p>
<img src="../img/logo.png" alt="Logo">
<ul><li>First</li><li>Second</li></ul>
<table><tr><th>Name</th><th>Qty</th></tr><tr><td>Apple</td><td>3</td></tr></table>
<script>hidden-script</script><textarea>hidden-input</textarea>
</body></html>`
	got, err := convertHTMLToMarkdown(input, "https://example.com/docs/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# 文件", "Hello &", "**world**", "`a < b`", "First", "Second",
		"[Guide](https://example.com/guide)",
		"[Sub](https://example.com/docs/sub.html)",
		"[Other](https://other.example/x)",
		"![Logo](https://example.com/img/logo.png)",
		"| Name  | Qty |\n|-------|-----|\n| Apple | 3   |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("markdown missing %q: %q", want, got)
		}
	}
	for _, unwanted := range []string{"hidden-style", "hidden-script", "hidden-input", "<h1>", "<strong>", "<table>", "(/guide)"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("markdown contains %q: %q", unwanted, got)
		}
	}
	if text := extractTextFromHTML(input, "https://example.com/docs/index.html"); text != got {
		t.Errorf("text conversion = %q, want %q", text, got)
	}
}

func TestConvertHTMLToMarkdownRelativeLinks(t *testing.T) {
	const input = `<p><a href="/guide">Guide</a> <a href="sub.html">Sub</a> <a href="#top">Top</a></p>`
	for _, tt := range []struct {
		name, pageURL string
		want          []string
	}{
		{
			name:    "no_page_url_keeps_links_as_written",
			pageURL: "",
			want:    []string{"[Guide](/guide)", "[Sub](sub.html)", "[Top](#top)"},
		},
		{
			name:    "page_url_resolves_against_the_page",
			pageURL: "http://example.com/docs/index.html?x=1",
			want:    []string{"[Guide](http://example.com/guide)", "[Sub](http://example.com/docs/sub.html)", "[Top](http://example.com/docs/index.html?x=1#top)"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convertHTMLToMarkdown(input, tt.pageURL)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("markdown missing %q: %q", want, got)
				}
			}
		})
	}
}

func TestExecuteFetchByURLResolvesLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<p><a href="next.html">Next</a></p><table><tr><th>K</th></tr><tr><td>V</td></tr></table>`)
	}))
	defer server.Close()

	got, err := executeFetchByURL(t.Context(), server.URL+"/docs/page.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Next](" + server.URL + "/docs/next.html)", "| K |", "| V |"} {
		if !strings.Contains(got, want) {
			t.Errorf("agentic fetch missing %q: %q", want, got)
		}
	}
}

func TestFetchContentFormats(t *testing.T) {
	const html = `<h1>文件</h1><p>Hello &amp; <strong>world</strong>. <a href="/guide">Guide</a></p>` +
		`<table><tr><th>Name</th><th>Qty</th></tr><tr><td>Apple</td><td>3</td></tr></table>` +
		`<script>hidden-script</script>`
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

	converted := []string{"**world**", "[Guide](" + server.URL + "/guide)", "| Name  | Qty |\n|-------|-----|\n| Apple | 3   |"}
	for _, tt := range []struct {
		name, path, format string
		want               []string
	}{
		{name: "default", path: "/docs/", want: converted},
		{name: "markdown", path: "/docs/", format: "markdown", want: converted},
		{name: "text", path: "/docs/", format: "text", want: converted},
		{name: "html", path: "/docs/", format: "html", want: []string{html}},
		{name: "non_html", path: "/json", format: "markdown", want: []string{`{"value":"<strong>raw</strong>"}`}},
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
			if response.IsError {
				t.Fatalf("unexpected error response: %+v", response)
			}
			for _, want := range tt.want {
				if !strings.Contains(response.Content, want) {
					t.Errorf("response missing %q: %q", want, response.Content)
				}
			}
			if tt.format != "html" && strings.Contains(response.Content, "hidden-script") {
				t.Fatalf("script leaked into converted content: %q", response.Content)
			}
		})
	}
}
