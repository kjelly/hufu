package agent

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
)

func TestNamedSubmissionToolReachesCompatibleProvider(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		select {
		case bodies <- raw:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"submit","type":"function","function":{"name":"submit_result","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(server.Close)
	provider, err := newOpenAICompatProvider(server.URL, "fixture-key", "local", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.LanguageModel(t.Context(), "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Generate(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("Submit existing evidence")},
		Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "submit_result", Description: "Submit existing evidence", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{},
		}}},
		ToolChoice: new(fantasy.SpecificToolChoice("submit_result")),
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Choice struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(<-bodies, &body); err != nil || body.Choice.Type != "function" || body.Choice.Function.Name != "submit_result" {
		t.Fatalf("named submission choice did not reach provider: %+v err=%v", body, err)
	}
	if len(response.Content.ToolCalls()) != 1 || response.Content.ToolCalls()[0].ToolName != "submit_result" {
		t.Fatal("provider tool submission was not preserved")
	}
}
