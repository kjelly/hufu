//go:build linux || darwin

package tools

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	const input = `<p><a href="/guide">Guide</a> <a href="sub.html">Sub</a> <a href="#top">Top</a></p><img src="../img.png" alt="Image">`
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
		{
			name:    "page_url_credentials_do_not_propagate",
			pageURL: "http://fetch-user:p%40ssword@example.com/docs/index.html?x=1",
			want:    []string{"[Guide](http://example.com/guide)", "[Sub](http://example.com/docs/sub.html)", "[Top](http://example.com/docs/index.html?x=1#top)", "![Image](http://example.com/img.png)"},
		},
		{
			name:    "page_url_username_does_not_propagate",
			pageURL: "http://fetch-token@example.com/docs/index.html?x=1",
			want:    []string{"[Guide](http://example.com/guide)", "[Sub](http://example.com/docs/sub.html)", "[Top](http://example.com/docs/index.html?x=1#top)", "![Image](http://example.com/img.png)"},
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

func TestFetchOutputURL(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
	}{
		{name: "empty"},
		{name: "unchanged", input: "https://example.com/docs?q=1#top", want: "https://example.com/docs?q=1#top"},
		{name: "password", input: "https://user:secret@example.com/docs?q=1#top", want: "https://example.com/docs?q=1#top"},
		{name: "username_only", input: "https://token@example.com/docs", want: "https://example.com/docs"},
		{name: "encoded_credentials", input: "https://user%40name:p%40ss@example.com/docs", want: "https://example.com/docs"},
		{name: "invalid", input: "https://user:secret@example.com/%zz"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := fetchOutputURL(tt.input); got != tt.want {
				t.Fatalf("fetchOutputURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetchCredentialsStayOutOfOutput(t *testing.T) {
	const username, password = "fetch-user", "fetch-secret"
	const body = `<a href="next.html">Next</a><img src="pic.png" alt="Pic">`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			t.Error("fetch lost HTTP Basic Auth credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/error" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, body)
	}))
	defer target.Close()
	credentialURL, err := url.Parse(target.URL + "/docs/page.html")
	if err != nil {
		t.Fatal(err)
	}
	credentialURL.User = url.UserPassword(username, password)
	errorURL := credentialURL.Clone()
	errorURL.Path = "/error"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, credentialURL.String(), http.StatusFound)
	}))
	defer origin.Close()

	for _, tt := range []struct {
		name, requestURL, displayURL string
		wantError                    bool
	}{
		{name: "request_credentials", requestURL: credentialURL.String(), displayURL: target.URL + "/docs/page.html"},
		{name: "redirect_credentials", requestURL: origin.URL + "/entry", displayURL: origin.URL + "/entry"},
		{name: "http_error", requestURL: errorURL.String(), displayURL: target.URL + "/error", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, response fantasy.ToolResponse) {
				t.Helper()
				if response.IsError != tt.wantError {
					t.Fatalf("IsError = %t, want %t: %s", response.IsError, tt.wantError, response.Content)
				}
				for _, credential := range []string{username, password} {
					if strings.Contains(response.Content, credential) {
						t.Error("URL credentials appeared in tool output")
					}
				}
				if !strings.Contains(response.Content, tt.displayURL) {
					t.Errorf("output omitted display URL %q", tt.displayURL)
				}
			}
			for _, format := range []string{"markdown", "text", "html", "agentic"} {
				t.Run(format, func(t *testing.T) {
					var args any = fetchArgs{URL: tt.requestURL, Format: format}
					if format == "agentic" {
						args = agenticFetchArgs{URL: tt.requestURL, Prompt: "summarize"}
					}
					input, err := jsonv2.Marshal(args)
					if err != nil {
						t.Fatal(err)
					}
					call := fantasy.ToolCall{Input: string(input)}
					var response fantasy.ToolResponse
					if format == "agentic" {
						response, err = executeAgenticFetch(t.Context(), call, "")
					} else {
						response, err = executeFetch(t.Context(), call, "")
					}
					if err != nil {
						t.Fatal(err)
					}
					check(t, response)
					if !tt.wantError && format != "html" {
						for _, want := range []string{"[Next](" + target.URL + "/docs/next.html)", "![Pic](" + target.URL + "/docs/pic.png)"} {
							if !strings.Contains(response.Content, want) {
								t.Errorf("output missing %q", want)
							}
						}
					}
				})
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

// TestFetchResolvesLinksAgainstRedirectTarget covers the client following a
// redirect: links must resolve against the URL that served the body, not the
// one that was requested.
func TestFetchResolvesLinksAgainstRedirectTarget(t *testing.T) {
	const body = `<p><a href="next.html">Next</a> <a href="#top">Top</a> <a href="/root.html">Root</a></p><img src="pic.png" alt="Pic">`
	serve := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, body)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { serve(w) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/docs":
			http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
		case "/old":
			http.Redirect(w, r, "/new/page.html", http.StatusFound)
		case "/away":
			http.Redirect(w, r, target.URL+"/b/index.html", http.StatusTemporaryRedirect)
		default:
			serve(w)
		}
	}))
	defer origin.Close()

	for _, tt := range []struct {
		name, path, finalURL, dir string
	}{
		{name: "trailing_slash", path: "/docs", finalURL: origin.URL + "/docs/", dir: origin.URL + "/docs/"},
		{name: "path_change", path: "/old", finalURL: origin.URL + "/new/page.html", dir: origin.URL + "/new/"},
		{name: "host_change", path: "/away", finalURL: target.URL + "/b/index.html", dir: target.URL + "/b/"},
		{name: "no_redirect", path: "/plain/page.html", finalURL: origin.URL + "/plain/page.html", dir: origin.URL + "/plain/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := []string{
				"[Next](" + tt.dir + "next.html)",
				"[Top](" + tt.finalURL + "#top)",
				"![Pic](" + tt.dir + "pic.png)",
			}
			if tt.name == "host_change" {
				want = append(want, "[Root]("+target.URL+"/root.html)")
			} else {
				want = append(want, "[Root]("+origin.URL+"/root.html)")
			}

			page, err := fetchURL(t.Context(), origin.URL+tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if page.finalURL != tt.finalURL {
				t.Fatalf("finalURL = %q, want %q", page.finalURL, tt.finalURL)
			}

			for _, format := range []string{"markdown", "text"} {
				input, err := json.Marshal(fetchArgs{URL: origin.URL + tt.path, Format: format})
				if err != nil {
					t.Fatal(err)
				}
				response, err := executeFetch(t.Context(), fantasy.ToolCall{Input: string(input)}, "")
				if err != nil {
					t.Fatal(err)
				}
				if response.IsError {
					t.Fatalf("%s: unexpected error response: %+v", format, response)
				}
				for _, w := range want {
					if !strings.Contains(response.Content, w) {
						t.Errorf("%s response missing %q: %q", format, w, response.Content)
					}
				}
			}

			agentic, err := executeFetchByURL(t.Context(), origin.URL+tt.path)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range want {
				if !strings.Contains(agentic, w) {
					t.Errorf("agentic fetch missing %q: %q", w, agentic)
				}
			}
		})
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
