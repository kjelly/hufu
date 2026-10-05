//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/ollamaweb"
)

type fakeOllamaWebClient struct {
	searchRequest ollamaweb.SearchRequest
	fetchRequest  ollamaweb.FetchRequest
	searchResult  ollamaweb.SearchResponse
	fetchResult   ollamaweb.FetchResponse
	searchError   error
	fetchError    error
	searchCalls   int
	fetchCalls    int
}

func (f *fakeOllamaWebClient) Search(_ context.Context, request ollamaweb.SearchRequest) (ollamaweb.SearchResponse, error) {
	f.searchCalls++
	f.searchRequest = request
	return f.searchResult, f.searchError
}

func (f *fakeOllamaWebClient) Fetch(_ context.Context, request ollamaweb.FetchRequest) (ollamaweb.FetchResponse, error) {
	f.fetchCalls++
	f.fetchRequest = request
	return f.fetchResult, f.fetchError
}

func runWebHandler(t *testing.T, tool fantasy.AgentTool, ctx context.Context, input string) fantasy.ToolResponse {
	t.Helper()
	core, ok := tool.(*coreTool)
	if !ok {
		t.Fatal("tool is not a coreTool")
	}
	response, err := core.handler(ctx, fantasy.ToolCall{ID: "call-1", Name: tool.Info().Name, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(response.Content)) {
		t.Fatalf("invalid JSON response: %q", response.Content)
	}
	return response
}

func TestWebSearchArgumentsAndOutput(t *testing.T) {
	client := &fakeOllamaWebClient{searchResult: ollamaweb.SearchResponse{Results: []ollamaweb.SearchResult{
		{Title: "Example", URL: "https://example.com/a", Content: "first"},
		{Title: "Second", URL: "https://example.com/b", Content: "second"},
	}}}
	tool := NewWebSearchTool(WithOllamaWebClient(client))
	response := runWebHandler(t, tool, t.Context(), `{"query":"  current news  ","max_results":1}`)
	if response.IsError || client.searchRequest.Query != "current news" || client.searchRequest.MaxResults != 1 {
		t.Fatalf("unexpected search: response=%+v request=%+v", response, client.searchRequest)
	}
	var output webSearchOutput
	if err := json.Unmarshal([]byte(response.Content), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.Results) != 1 || output.Meta.ResultCount != 1 || !output.Meta.Truncated || output.Meta.Provider != "ollama-web" {
		t.Fatalf("unexpected output: %+v", output)
	}
	runWebHandler(t, tool, t.Context(), `{"query":"example"}`)
	if client.searchRequest.MaxResults != 3 {
		t.Fatalf("default max_results = %d, want 3", client.searchRequest.MaxResults)
	}
	for _, input := range []string{
		`{"query":" "}`, `{"query":"x","max_results":0}`, `{"query":"x","max_results":11}`,
		`{"query":"x","max_results":1.5}`, `{"query":"x","max_results":null}`,
		`{"query":"x","max_results":"2"}`, `{"query":"x","extra":true}`,
		`{"query":"x"} {}`, `{"query":123}`, `{"query":"` + strings.Repeat("x", 2049) + `"}`,
	} {
		response := runWebHandler(t, tool, t.Context(), input)
		if !response.IsError || !strings.Contains(response.Content, `"code":"invalid_arguments"`) {
			t.Errorf("input %q: unexpected response %+v", input, response)
		}
	}
	if client.searchCalls != 2 {
		t.Fatalf("invalid input reached client: %d calls", client.searchCalls)
	}
}

func TestWebFetchArgumentsAndOutput(t *testing.T) {
	client := &fakeOllamaWebClient{fetchResult: ollamaweb.FetchResponse{
		Title: "Page", Content: "Content", Links: []string{"https://example.com/next", "http://127.0.0.1/private"},
	}}
	tool := NewWebFetchTool(WithOllamaWebClient(client))
	response := runWebHandler(t, tool, t.Context(), `{"url":"https://example.com/page"}`)
	var output webFetchOutput
	if err := json.Unmarshal([]byte(response.Content), &output); err != nil {
		t.Fatal(err)
	}
	if response.IsError || output.URL != client.fetchRequest.URL || len(output.Links) != 1 || output.Links[0] != "https://example.com/next" {
		t.Fatalf("unexpected fetch output: %+v", output)
	}
	for _, input := range []string{
		`{"url":"http://localhost/a"}`, `{"url":"http://10.0.0.1/a"}`,
		`{"url":"file:///etc/passwd"}`, `{"url":"https://u:p@example.com"}`,
		`{"url":null}`, `{"url":"https://example.com","extra":1}`,
	} {
		if result := runWebHandler(t, tool, t.Context(), input); !result.IsError {
			t.Errorf("input %q unexpectedly accepted", input)
		}
	}
	if client.fetchCalls != 1 {
		t.Fatalf("invalid input reached client: %d calls", client.fetchCalls)
	}
}

func TestWebOutputBoundsAndErrors(t *testing.T) {
	client := &fakeOllamaWebClient{searchResult: ollamaweb.SearchResponse{Results: make([]ollamaweb.SearchResult, 10)}}
	for i := range client.searchResult.Results {
		client.searchResult.Results[i] = ollamaweb.SearchResult{
			Title: strings.Repeat("🍪", 500), URL: "https://example.com/a", Content: strings.Repeat("\n", 20000),
		}
	}
	response := runWebHandler(t, NewWebSearchTool(WithOllamaWebClient(client)), t.Context(), `{"query":"x","max_results":10}`)
	var search webSearchOutput
	if err := json.Unmarshal([]byte(response.Content), &search); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) > webSearchOutputLimit || !search.Meta.Truncated || !utf8.ValidString(response.Content) {
		t.Fatalf("search output bounds violated: size=%d meta=%+v", len(response.Content), search.Meta)
	}
	client.fetchResult = ollamaweb.FetchResponse{Title: strings.Repeat("🍪", 500), Content: strings.Repeat("\n", 200000)}
	for range 100 {
		client.fetchResult.Links = append(client.fetchResult.Links, "https://example.com/page")
	}
	response = runWebHandler(t, NewWebFetchTool(WithOllamaWebClient(client)), t.Context(), `{"url":"https://example.com"}`)
	var fetched webFetchOutput
	if err := json.Unmarshal([]byte(response.Content), &fetched); err != nil {
		t.Fatal(err)
	}
	if len(response.Content) > webFetchOutputLimit || !fetched.Meta.Truncated || !utf8.ValidString(response.Content) {
		t.Fatalf("fetch output bounds violated: size=%d meta=%+v", len(response.Content), fetched.Meta)
	}
	client.searchError = errors.New("secret transport details")
	response = runWebHandler(t, NewWebSearchTool(WithOllamaWebClient(client)), t.Context(), `{"query":"x"}`)
	if !response.IsError || strings.Contains(response.Content, "secret") || !strings.Contains(response.Content, `"code":"ollama_web_unavailable"`) {
		t.Fatalf("unsafe error: %+v", response)
	}
	response = runWebHandler(t, NewWebSearchTool(), t.Context(), `{"query":"x"}`)
	if !strings.Contains(response.Content, `"code":"ollama_web_api_key_missing"`) {
		t.Fatalf("missing key error: %+v", response)
	}
}

func TestWebPolicyDenialDisposition(t *testing.T) {
	client := &fakeOllamaWebClient{}
	for _, test := range []struct {
		name    string
		options []ToolOption
		ctx     context.Context
		code    string
	}{
		{"config no-net", []ToolOption{WithNetworkBlock(true)}, t.Context(), "network_blocked"},
		{"context no-net", nil, context.WithValue(t.Context(), AgentNetworkBlockKey, true), "network_blocked"},
		{"config force-mcp", []ToolOption{WithForceMCP(true)}, t.Context(), "force_mcp"},
		{"context force-mcp", nil, context.WithValue(t.Context(), AgentForceMCPKey, true), "force_mcp"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dispositions []ToolExecutionDisposition
			ctx := context.WithValue(test.ctx, ToolExecutionDispositionReporterKey, ToolExecutionDispositionReporter(func(d ToolExecutionDisposition) {
				dispositions = append(dispositions, d)
			}))
			tool := NewWebSearchTool(append(test.options, WithOllamaWebClient(client))...)
			response := runWebHandler(t, tool, ctx, `{"query":"x"}`)
			if !response.IsError || !strings.Contains(response.Content, `"code":"`+test.code+`"`) || len(dispositions) != 1 || dispositions[0].Executed || dispositions[0].ReasonCode != test.code {
				t.Fatalf("denial mismatch: response=%+v dispositions=%+v", response, dispositions)
			}
		})
	}
	if client.searchCalls != 0 {
		t.Fatalf("policy-denied request reached client: %d", client.searchCalls)
	}
}
