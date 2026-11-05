package agent

import (
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/providerproxy"
	"github.com/kjelly/hufu/internal/scripted"
)

func TestProviderProtocolAcrossBoundaries(t *testing.T) {
	for _, protocol := range []string{"", config.APIProtocolChatCompletions, config.APIProtocolResponses} {
		for _, mode := range []string{"direct", "in_process", "subprocess"} {
			for _, streaming := range []bool{false, true} {
				name := protocol + "/" + mode + "/generate"
				if streaming {
					name = protocol + "/" + mode + "/stream"
				}
				t.Run(name, func(t *testing.T) {
					script := scripted.NewProvider("custom-alias", []scripted.Step{{Content: "answer", Usage: &scripted.Usage{InputTokens: 5, OutputTokens: 2}}})
					var mu sync.Mutex
					var path string
					var body map[string]any
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, err.Error(), 500)
							return
						}
						mu.Lock()
						path = r.URL.Path
						err = json.Unmarshal(raw, &body)
						mu.Unlock()
						if err != nil {
							http.Error(w, err.Error(), 500)
							return
						}
						r.Body = io.NopCloser(strings.NewReader(string(raw)))
						http.StripPrefix("/prefix", script).ServeHTTP(w, r)
					}))
					defer upstream.Close()
					p, err := newConfiguredOpenAICompatibleProvider(upstream.URL+"/prefix/v1", "", "remote", protocol)
					if err != nil {
						t.Fatal(err)
					}
					if mode != "direct" {
						var boundary providerproxy.Boundary
						cfg := providerproxy.Config{UpstreamURL: p.baseURL}
						if mode == "in_process" {
							boundary, err = providerproxy.StartInProcess(t.Context(), cfg)
						} else {
							boundary, err = providerproxy.StartWithCommand(t.Context(), os.Args[0], []string{"-test.run=^TestOllamaProviderListModelNamesProxyChild$"}, []string{"HUFU_OLLAMA_PROVIDER_PROXY_TEST_CHILD=1"}, cfg)
						}
						if err != nil {
							t.Fatal(err)
						}
						defer func() {
							if err := boundary.Stop(); err != nil {
								t.Error(err)
							}
						}()
						p.setBoundary(boundary.URL(), boundary.HTTPClient())
					}
					lm, err := p.LanguageModel(t.Context(), "remote/custom-alias")
					if err != nil {
						t.Fatal(err)
					}
					call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}}
					text := ""
					var usage fantasy.Usage
					if streaming {
						stream, err := lm.Stream(t.Context(), call)
						if err != nil {
							t.Fatal(err)
						}
						for part := range stream {
							if part.Error != nil {
								t.Fatal(part.Error)
							}
							if part.Type == fantasy.StreamPartTypeTextDelta {
								text += part.Delta
							}
							if part.Type == fantasy.StreamPartTypeFinish {
								usage = part.Usage
							}
						}
					} else {
						response, err := lm.Generate(t.Context(), call)
						if err != nil {
							t.Fatal(err)
						}
						text, usage = response.Content.Text(), response.Usage
					}
					wantPath := "/prefix/v1/chat/completions"
					if protocol == config.APIProtocolResponses {
						wantPath = "/prefix/v1/responses"
					}
					mu.Lock()
					defer mu.Unlock()
					if path != wantPath || body["model"] != "custom-alias" || text != "answer" || usage.TotalTokens != 7 || len(script.Issues()) != 0 {
						t.Fatalf("path=%s body=%v text=%q usage=%+v issues=%v", path, body, text, usage, script.Issues())
					}
					if protocol == config.APIProtocolResponses && body["store"] != false {
						t.Fatalf("store=%v", body["store"])
					}
				})
			}
		}
	}
}

func TestResponsesScriptedToolRoundTrip(t *testing.T) {
	script := scripted.NewProvider("custom", []scripted.Step{
		{ToolCall: &scripted.ToolCall{Name: "lookup", Arguments: `{"value":"question"}`}},
		{Match: &scripted.Match{Contains: "tool-result-marker"}, Content: "answer"},
	})
	boundary, err := scripted.NewBoundary(t.Context(), script)
	if err != nil {
		t.Fatal(err)
	}
	defer boundary.Stop()
	p, err := newConfiguredOpenAICompatibleProvider(scripted.Endpoint, "", "remote", config.APIProtocolResponses)
	if err != nil {
		t.Fatal(err)
	}
	p.setBoundary(boundary.URL(), boundary.HTTPClient())
	lm, err := p.LanguageModel(t.Context(), "remote/custom")
	if err != nil {
		t.Fatal(err)
	}
	prompt := fantasy.Prompt{fantasy.NewUserMessage("question")}
	call := fantasy.Call{Prompt: prompt, Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "lookup", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}}}
	response, err := lm.Generate(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	calls := response.Content.ToolCalls()
	if len(calls) != 1 || calls[0].ToolName != "lookup" || calls[0].Input != `{"value":"question"}` {
		t.Fatalf("calls=%+v", calls)
	}
	prompt = append(prompt,
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: calls[0].ToolCallID, ToolName: calls[0].ToolName, Input: calls[0].Input}}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: calls[0].ToolCallID, Output: fantasy.ToolResultOutputContentText{Text: "tool-result-marker"}}}},
	)
	call.Prompt = prompt
	response, err = lm.Generate(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	if response.Content.Text() != "answer" || len(script.Issues()) != 0 {
		t.Fatalf("response=%+v issues=%v", response, script.Issues())
	}
}
