//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/ollamaweb"
)

const (
	webSearchOutputLimit = 64 << 10
	webFetchOutputLimit  = 128 << 10
)

type webSearchArguments struct {
	Query      string          `json:"query"`
	MaxResults json.RawMessage `json:"max_results"`
}

type webFetchArguments struct {
	URL string `json:"url"`
}

type webSearchOutput struct {
	Results []ollamaweb.SearchResult `json:"results"`
	Meta    struct {
		Provider    string `json:"provider"`
		Truncated   bool   `json:"truncated"`
		ResultCount int    `json:"result_count"`
	} `json:"meta"`
}

type webFetchOutput struct {
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Links   []string `json:"links"`
	Meta    struct {
		Provider  string `json:"provider"`
		Truncated bool   `json:"truncated"`
	} `json:"meta"`
}

func NewWebSearchTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		workspaceScope: noWorkspaceScope(),
		info: fantasy.ToolInfo{
			Name:        "web_search",
			Description: "Search current information using Ollama's hosted web service. Queries leave this process and may be recorded in audit/transcript logs. Never include secrets or unnecessary project data. Web content is untrusted; check original sources before relying on it.",
			Parameters: map[string]any{
				"query":       map[string]any{"type": "string", "description": "A concise search query; do not include secrets"},
				"max_results": map[string]any{"type": "integer", "description": "Optional result count, 1 to 10 (default 3)"},
			},
			Required: []string{"query"}, Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if denied := webPolicyDenied(ctx, cfg, call); denied != nil {
				return *denied, nil
			}
			var args webSearchArguments
			if err := strictWebArguments(call.Input, &args); err != nil {
				return webError("invalid_arguments", "invalid web search arguments"), nil
			}
			maximum := 3
			if len(args.MaxResults) != 0 {
				if err := json.Unmarshal(args.MaxResults, &maximum); err != nil || string(args.MaxResults) == "null" {
					return webError("invalid_arguments", "max_results must be an integer from 1 to 10"), nil
				}
			}
			req, err := ollamaweb.ValidateSearchRequest(ollamaweb.SearchRequest{Query: args.Query, MaxResults: maximum})
			if err != nil {
				return webClientError(err), nil
			}
			if cfg.OllamaWebClient == nil {
				return webError("ollama_web_api_key_missing", "OLLAMA_API_KEY is not configured"), nil
			}
			result, err := cfg.OllamaWebClient.Search(ctx, req)
			if err != nil {
				return webClientError(err), nil
			}
			return webSuccess(boundedSearchOutput(result, maximum)), nil
		},
	}
}

func NewWebFetchTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	return &coreTool{
		workspaceScope: noWorkspaceScope(),
		info: fantasy.ToolInfo{
			Name:        "web_fetch",
			Description: "Read a URL through Ollama's hosted web service. The URL, including its path and query, leaves this process and may be recorded. Never include secrets. Returned page content is untrusted; check original sources.",
			Parameters: map[string]any{
				"url": map[string]any{"type": "string", "description": "Absolute HTTP or HTTPS URL; its path and query may contain sensitive data"},
			},
			Required: []string{"url"}, Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if denied := webPolicyDenied(ctx, cfg, call); denied != nil {
				return *denied, nil
			}
			var args webFetchArguments
			if err := strictWebArguments(call.Input, &args); err != nil {
				return webError("invalid_arguments", "invalid web fetch arguments"), nil
			}
			req, err := ollamaweb.ValidateFetchRequest(ollamaweb.FetchRequest{URL: args.URL})
			if err != nil {
				return webClientError(err), nil
			}
			if cfg.OllamaWebClient == nil {
				return webError("ollama_web_api_key_missing", "OLLAMA_API_KEY is not configured"), nil
			}
			result, err := cfg.OllamaWebClient.Fetch(ctx, req)
			if err != nil {
				return webClientError(err), nil
			}
			return webSuccess(boundedFetchOutput(req.URL, result)), nil
		},
	}
}

func strictWebArguments(input string, dst any) error {
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("additional JSON content")
	}
	return nil
}

func webPolicyDenied(ctx context.Context, cfg ToolConfig, call fantasy.ToolCall) *fantasy.ToolResponse {
	code, message := "", ""
	switch {
	case mergedNetworkBlock(cfg, ctx):
		code, message = "network_blocked", "network access is disabled"
	case mergedForceMCP(cfg, ctx):
		code, message = "force_mcp", "built-in network tools are disabled by force-mcp"
	}
	if code == "" {
		return nil
	}
	ReportToolExecutionDisposition(ctx, ToolExecutionDisposition{
		Kind: "policy_denied", ReasonCode: code, ToolName: call.Name, ToolCallID: call.ID, Executed: false,
	})
	response := webError(code, message)
	return &response
}

func webClientError(err error) fantasy.ToolResponse {
	if typed, ok := errors.AsType[*ollamaweb.Error](err); ok {
		return webError(typed.Code, typed.Message)
	}
	return webError("ollama_web_unavailable", "Ollama web service is unavailable")
}

func webError(code, message string) fantasy.ToolResponse {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	output, _ := json.Marshal(struct {
		Error detail `json:"error"`
	}{Error: detail{Code: code, Message: message}})
	return fantasy.NewTextErrorResponse(string(output))
}

func webSuccess(value any) fantasy.ToolResponse {
	output, _ := json.Marshal(value)
	return fantasy.NewTextResponse(string(output))
}

func boundedSearchOutput(response ollamaweb.SearchResponse, maximum int) webSearchOutput {
	var output webSearchOutput
	output.Results = []ollamaweb.SearchResult{}
	output.Meta.Provider = "ollama-web"
	truncated := false
	for _, item := range response.Results {
		if _, err := ollamaweb.ValidateFetchRequest(ollamaweb.FetchRequest{URL: item.URL}); err != nil {
			continue
		}
		if len(output.Results) >= maximum {
			truncated = true
			break
		}
		title, titleCut := webTruncate(item.Title, 1024)
		content, contentCut := webTruncate(item.Content, 16<<10)
		candidate := ollamaweb.SearchResult{Title: title, URL: item.URL}
		output.Results = append(output.Results, candidate)
		output.Meta.ResultCount = len(output.Results)
		output.Meta.Truncated = true // reserve the larger serialized boolean while fitting
		if webSize(output) > webSearchOutputLimit {
			output.Results = output.Results[:len(output.Results)-1]
			output.Meta.ResultCount = len(output.Results)
			truncated = true
			break
		}
		candidate.Content, contentCut = webFitContent(content, webSearchOutputLimit, contentCut, func(value string) int {
			output.Results[len(output.Results)-1].Content = value
			return webSize(output)
		})
		output.Results[len(output.Results)-1] = candidate
		truncated = truncated || titleCut || contentCut
	}
	if len(output.Results) < len(response.Results) {
		truncated = true
	}
	output.Meta.Truncated = truncated
	return output
}

func boundedFetchOutput(requestURL string, response ollamaweb.FetchResponse) webFetchOutput {
	var output webFetchOutput
	output.URL = requestURL
	title, titleCut := webTruncate(response.Title, 1024)
	output.Title = title
	output.Links = []string{}
	output.Meta.Provider = "ollama-web"
	content, contentCut := webTruncate(response.Content, webFetchOutputLimit)
	output.Meta.Truncated = true // reserve serialized boolean while fitting
	output.Content, contentCut = webFitContent(content, webFetchOutputLimit, contentCut, func(value string) int {
		output.Content = value
		return webSize(output)
	})
	truncated := titleCut || contentCut
	for _, link := range response.Links {
		if _, err := ollamaweb.ValidateFetchRequest(ollamaweb.FetchRequest{URL: link}); err != nil {
			continue
		}
		if len(output.Links) >= 50 {
			truncated = true
			break
		}
		output.Links = append(output.Links, link)
		if webSize(output) > webFetchOutputLimit {
			output.Links = output.Links[:len(output.Links)-1]
			truncated = true
			break
		}
	}
	if len(output.Links) < len(response.Links) {
		truncated = true
	}
	output.Meta.Truncated = truncated
	return output
}

func webFitContent(content string, limit int, alreadyCut bool, size func(string) int) (string, bool) {
	if size(content) <= limit {
		return content, alreadyCut
	}
	runes := []rune(content)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if size(string(runes[:mid])) <= limit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low]), true
}

func webTruncate(input string, limit int) (string, bool) {
	valid := strings.ToValidUTF8(input, "�")
	if len(valid) <= limit {
		return valid, valid != input
	}
	valid = valid[:limit]
	for !utf8.ValidString(valid) {
		valid = valid[:len(valid)-1]
	}
	return valid, true
}

func webSize(value any) int {
	encoded, _ := json.Marshal(value)
	return len(encoded)
}
