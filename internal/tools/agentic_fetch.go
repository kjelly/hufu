package tools

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

type agenticFetchArgs struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

func NewAgenticFetchTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "agentic_fetch",
			Description: "Fetch content from a URL and return it along with a prompt for analysis. Use this when you need to retrieve web content and then analyze, summarize, or extract information from it.",
			Parameters: map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to fetch content from",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The instruction for what to do with the fetched content (e.g., 'summarize this article', 'extract the main arguments')",
				},
			},
			Required: []string{"prompt"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeAgenticFetch(ctx, call, cfg.WorkDir)
		},
	}
}

func executeAgenticFetch(ctx context.Context, call fantasy.ToolCall, workDir string) (fantasy.ToolResponse, error) {
	var args agenticFetchArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Prompt == "" {
		return fantasy.NewTextErrorResponse("prompt parameter is required"), nil
	}

	if args.URL == "" {
		return fantasy.NewTextErrorResponse("url parameter is required"), nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return fantasy.NewTextErrorResponse("url must start with http:// or https://"), nil
	}

	fetchResult, err := executeFetchByURL(ctx, args.URL)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to fetch %s: %v", args.URL, err)), nil
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Fetched content from %s:\n\n", args.URL))
	b.WriteString(fetchResult)
	b.WriteString(fmt.Sprintf("\n\n---\n\nInstruction: %s", args.Prompt))

	return fantasy.NewTextResponse(b.String()), nil
}

func executeFetchByURL(ctx context.Context, url string) (string, error) {
	content, contentType, err := fetchURL(ctx, url)
	if err != nil {
		return "", err
	}

	isHTML := strings.HasPrefix(contentType, "text/html") || strings.Contains(contentType, "html")

	if isHTML {
		markdown, err := convertHTMLToMarkdown(content)
		if err != nil {
			return cleanupContent(content), nil
		}
		return cleanupContent(markdown), nil
	}

	return cleanupContent(content), nil
}
