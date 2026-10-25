//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

const maxFetchSize = 100 * 1024

type fetchArgs struct {
	URL     string `json:"url"`
	Format  string `json:"format"`
	Timeout int    `json:"timeout,omitempty"`
}

func NewFetchTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		workspaceScope: noWorkspaceScope(),
		info: fantasy.ToolInfo{
			Name:        "fetch",
			Description: "Fetch content from a URL. Returns the content in the specified format (text, markdown, or html). Use this to retrieve web pages, API responses, or any URL content.",
			Parameters: map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to fetch content from",
				},
				"format": map[string]any{
					"type":        "string",
					"description": "The format to return the content in: 'text' (plain text extracted from HTML), 'markdown' (HTML converted to markdown), or 'html' (raw HTML). Default: markdown",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 30s, max 120s)",
				},
			},
			Required: []string{"url"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeFetch(ctx, call, executionWorkDir(ctx, cfg))
		},
	}
}

var multipleNewlinesRe = regexp.MustCompile(`\n{3,}`)

func executeFetch(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args fetchArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.URL == "" {
		return fantasy.NewTextErrorResponse("url parameter is required"), nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return fantasy.NewTextErrorResponse("url must start with http:// or https://"), nil
	}

	format := args.Format
	if format == "" {
		format = "markdown"
	}
	switch format {
	case "text", "markdown", "html":
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid format %q (valid: text, markdown, html)", format)), nil
	}

	timeout := 30 * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > 120*time.Second {
			timeout = 120 * time.Second
		}
	}

	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	page, err := fetchURL(fetchCtx, args.URL)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to fetch %s: %v", fetchOutputURL(args.URL), err)), nil
	}

	var result string
	switch format {
	case "text":
		if page.isHTML() {
			text := extractTextFromHTML(page.content, page.finalURL)
			result = text
		} else {
			result = page.content
		}
	case "markdown":
		if page.isHTML() {
			markdown, err := convertHTMLToMarkdown(page.content, page.finalURL)
			if err != nil {
				result = page.content
			} else {
				result = markdown
			}
		} else {
			result = page.content
		}
	case "html":
		result = page.content
	}

	result = cleanupContent(result)

	if len(result) > maxFetchSize {
		result = safeTruncateHeadBytes(result, maxFetchSize) + fmt.Sprintf("\n\n[Content truncated to %d bytes]", maxFetchSize)
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Fetched content from %s:\n\n%s", fetchOutputURL(args.URL), result)), nil
}

// fetchOutputURL removes request credentials from display and link-resolution
// URLs. Keep the original URL for HTTP authentication. Invalid URLs cannot
// provide a safe base and must not be echoed verbatim.
func fetchOutputURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parsed.User = nil
	return parsed.String()
}

// fetchedPage is the body of a successful GET together with what the callers
// need to interpret it.
type fetchedPage struct {
	content     string
	contentType string
	// finalURL is the URL that served the body. The client follows
	// redirects, so it can differ from the requested URL and is the base
	// relative links in the body must resolve against.
	finalURL string
}

func (p fetchedPage) isHTML() bool {
	return strings.HasPrefix(p.contentType, "text/html") || strings.Contains(p.contentType, "html")
}

// fetchError keeps URL-bearing causes available to errors.Is/As without
// rendering them into tool output. Even a nested parse or redirect error can
// contain credentials, so only fixed messages and failure classes are displayed.
type fetchError struct {
	message string
	cause   error
}

func (e *fetchError) Error() string {
	switch {
	case errors.Is(e.cause, context.Canceled):
		return e.message + ": request canceled"
	case errors.Is(e.cause, context.DeadlineExceeded):
		return e.message + ": deadline exceeded"
	}
	if err, ok := errors.AsType[net.Error](e.cause); ok && err.Timeout() {
		return e.message + ": timeout"
	}
	return e.message
}

func (e *fetchError) Unwrap() error { return e.cause }

func fetchURL(ctx context.Context, url string) (fetchedPage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return fetchedPage{}, &fetchError{message: "invalid request URL", cause: err}
	}
	req.Header.Set("User-Agent", "hufu/1.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fetchedPage{}, &fetchError{message: "HTTP request failed", cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fetchedPage{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchSize+1))
	if err != nil {
		return fetchedPage{}, &fetchError{message: "reading response body failed", cause: err}
	}

	page := fetchedPage{content: string(body), contentType: resp.Header.Get("Content-Type"), finalURL: url}
	if resp.Request != nil && resp.Request.URL != nil {
		page.finalURL = resp.Request.URL.String()
	}
	return page, nil
}

// htmlToMarkdown serves every fetch call: the converter locks internally, so
// one instance is safe for the tool's parallel calls. The table plugin keeps
// <table> content as a Markdown table instead of flattening the cells into
// prose.
var htmlToMarkdown = converter.NewConverter(
	converter.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
	),
)

func extractTextFromHTML(htmlContent, pageURL string) string {
	markdown, err := convertHTMLToMarkdown(htmlContent, pageURL)
	if err != nil {
		return htmlContent
	}
	return markdown
}

// convertHTMLToMarkdown converts fetched HTML to Markdown. A non-empty pageURL
// resolves relative links and image sources against the page they came from,
// so the model can fetch them without reconstructing the origin itself.
func convertHTMLToMarkdown(htmlContent, pageURL string) (string, error) {
	markdown, err := htmlToMarkdown.ConvertString(htmlContent, converter.WithDomain(fetchOutputURL(pageURL)))
	if err != nil {
		return "", fmt.Errorf("converting HTML to Markdown: %w", err)
	}
	return markdown, nil
}

func cleanupContent(content string) string {
	content = strings.TrimSpace(content)
	content = multipleNewlinesRe.ReplaceAllString(content, "\n\n")
	return content
}
