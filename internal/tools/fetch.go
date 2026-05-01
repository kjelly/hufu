package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"charm.land/fantasy"
	md "github.com/JohannesKaufmann/html-to-markdown"
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
			return executeFetch(ctx, call, cfg.WorkDir)
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

	content, contentType, err := fetchURL(fetchCtx, args.URL)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to fetch %s: %v", args.URL, err)), nil
	}

	isHTML := strings.HasPrefix(contentType, "text/html") || strings.Contains(contentType, "html")

	var result string
	switch format {
	case "text":
		if isHTML {
			text := extractTextFromHTML(content)
			result = text
		} else {
			result = content
		}
	case "markdown":
		if isHTML {
			markdown, err := convertHTMLToMarkdown(content)
			if err != nil {
				result = content
			} else {
				result = markdown
			}
		} else {
			result = content
		}
	case "html":
		result = content
	}

	result = cleanupContent(result)

	if len(result) > maxFetchSize {
		result = safeTruncateHeadBytes(result, maxFetchSize) + fmt.Sprintf("\n\n[Content truncated to %d bytes]", maxFetchSize)
	}

	return fantasy.NewTextResponse(fmt.Sprintf("Fetched content from %s:\n\n%s", args.URL, result)), nil
}

func fetchURL(ctx context.Context, url string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "hufu/1.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchSize+1))
	if err != nil {
		return "", "", err
	}

	contentType := resp.Header.Get("Content-Type")
	return string(body), contentType, nil
}

func extractTextFromHTML(htmlContent string) string {
	converter := md.NewConverter("", true, nil)
	markdown, err := converter.ConvertString(htmlContent)
	if err != nil {
		return htmlContent
	}
	return markdown
}

func convertHTMLToMarkdown(htmlContent string) (string, error) {
	converter := md.NewConverter("", true, nil)
	return converter.ConvertString(htmlContent)
}

func cleanupContent(content string) string {
	content = strings.TrimSpace(content)
	content = multipleNewlinesRe.ReplaceAllString(content, "\n\n")
	return content
}
