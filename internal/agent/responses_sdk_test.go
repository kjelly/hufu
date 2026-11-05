package agent

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/schema"
)

// sdkResponsesModel exercises the pinned wire adapter without a listener,
// credentials, or a network fallback. Every request reaches the supplied host
// transport, including the SDK's structured-output entry points.
func sdkResponsesModel(t *testing.T, modelID string, enabled, selected bool, handle func(*http.Request) (*http.Response, error)) fantasy.LanguageModel {
	t.Helper()
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL("https://sdk-fixture.invalid/v1"),
		openaicompat.WithName("fixture"),
		openaicompat.WithHTTPClient(&http.Client{Transport: roundTripFunc(handle)}),
		openaicompat.WithResponsesAPIFunc(func(string) bool { return selected }),
	}
	if enabled {
		opts = append(opts, openaicompat.WithUseResponsesAPI())
	}
	p, err := openaicompat.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.LanguageModel(t.Context(), modelID)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sdkJSONResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestResponsesSDKProtocolSelection(t *testing.T) {
	for _, tc := range []struct {
		name              string
		enabled, selected bool
		path              string
	}{
		{"disabled flag", false, true, "/v1/chat/completions"},
		{"explicit chat", true, false, "/v1/chat/completions"},
		{"explicit responses", true, true, "/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			m := sdkResponsesModel(t, "custom-alias", tc.enabled, tc.selected, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != tc.path || r.Method != http.MethodPost {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if tc.path == "/v1/responses" {
					return sdkJSONResponse(`{"id":"r","status":"completed","output":[{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, 200), nil
				}
				return sdkJSONResponse(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, 200), nil
			})
			response, err := m.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || response.Content.Text() != "ok" {
				t.Fatalf("calls=%d response=%+v", calls, response)
			}
		})
	}
}

func TestResponsesSDKReasoningOptions(t *testing.T) {
	for _, tc := range []struct {
		name, model, effort string
		correctKey, sent    bool
	}{
		{"gpt5", "gpt-5", "medium", true, true},
		{"gpt5 none", "gpt-5", "none", true, true},
		{"o3", "o3-mini", "high", true, true},
		{"o1", "o1-mini", "low", true, true},
		{"oss", "oss-local", "medium", true, true},
		{"codex", "codex-mini-latest", "medium", true, true},
		{"computer", "computer-use-preview", "medium", true, true},
		{"case normalized gpt5", "GPT-5", "medium", true, true},
		{"case sensitive o3", "O3-mini", "medium", true, false},
		{"chat exception", "gpt-5-chat-latest", "medium", true, false},
		{"gpt6 limitation", "gpt-6", "medium", true, false},
		{"custom alias limitation", "custom", "medium", true, false},
		{"wrong options key", "gpt-5", "medium", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			m := sdkResponsesModel(t, tc.model, true, true, func(r *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				if err = json.Unmarshal(raw, &body); err != nil {
					return nil, err
				}
				return sdkJSONResponse(`{"id":"r","status":"completed","output":[]}`, 200), nil
			})
			key := openai.Name
			if !tc.correctKey {
				key = "fixture"
			}
			options := fantasy.ProviderOptions{key: &openai.ResponsesProviderOptions{ReasoningEffort: new(openai.ReasoningEffort(tc.effort))}}
			_, err := m.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}, ProviderOptions: options})
			if err != nil {
				t.Fatal(err)
			}
			reasoning, _ := body["reasoning"].(map[string]any)
			effort, exists := reasoning["effort"]
			if exists != tc.sent || (exists && effort != tc.effort) {
				t.Fatalf("reasoning=%v sent=%t", reasoning, tc.sent)
			}
			if body["store"] != false || body["previous_response_id"] != nil {
				t.Fatalf("unexpected server history: %v", body)
			}
		})
	}
}

func TestResponsesSDKEntryPoints(t *testing.T) {
	for _, entry := range []string{"generate", "stream", "generate_object", "stream_object"} {
		t.Run(entry, func(t *testing.T) {
			calls := 0
			object := strings.Contains(entry, "object")
			streaming := strings.HasPrefix(entry, "stream")
			m := sdkResponsesModel(t, "custom", true, true, func(r *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				var body map[string]any
				if err = json.Unmarshal(raw, &body); err != nil {
					return nil, err
				}
				if r.URL.Path != "/v1/responses" || body["store"] != false || (body["stream"] == true) != streaming {
					t.Errorf("path=%s body=%v", r.URL.Path, body)
				}
				output := `{"type":"message","id":"m","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`
				if object {
					tools, _ := body["tools"].([]any)
					if len(tools) != 1 {
						t.Errorf("object tools=%v", tools)
					} else if tool, _ := tools[0].(map[string]any); tool["name"] != "answer" {
						t.Errorf("tool=%v", tool)
					}
					output = `{"type":"function_call","id":"fc","call_id":"call","name":"answer","arguments":"{\"value\":\"ok\"}","status":"completed"}`
				}
				completed := fmt.Sprintf(`{"id":"r","status":"completed","output":[%s],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`, output)
				if !streaming {
					return sdkJSONResponse(completed, 200), nil
				}
				event := func(kind, payload string) string { return "event: " + kind + "\ndata: " + payload + "\n\n" }
				s := event("response.created", `{"type":"response.created","response":{"id":"r","status":"in_progress","output":[]}}`)
				s += event("response.output_item.added", fmt.Sprintf(`{"type":"response.output_item.added","output_index":0,"item":%s}`, output))
				if object {
					s += event("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"value\":\"ok\"}"}`)
				} else {
					s += event("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"ok"}`)
				}
				s += event("response.output_item.done", fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, output))
				s += event("response.completed", fmt.Sprintf(`{"type":"response.completed","response":%s}`, completed))
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(s))}, nil
			})
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}}
			objectCall := fantasy.ObjectCall{Prompt: call.Prompt, SchemaName: "answer", Schema: schema.Generate(reflect.TypeFor[struct {
				Value string `json:"value"`
			}]())}
			var usage fantasy.Usage
			text := ""
			switch entry {
			case "generate":
				response, err := m.Generate(t.Context(), call)
				if err != nil {
					t.Fatal(err)
				}
				usage, text = response.Usage, response.Content.Text()
			case "stream":
				stream, err := m.Stream(t.Context(), call)
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
			case "generate_object":
				response, err := m.GenerateObject(t.Context(), objectCall)
				if err != nil {
					t.Fatal(err)
				}
				usage = response.Usage
				if !reflect.DeepEqual(response.Object, map[string]any{"value": "ok"}) {
					t.Fatalf("object=%v", response.Object)
				}
				text = "ok"
			case "stream_object":
				stream, err := m.StreamObject(t.Context(), objectCall)
				if err != nil {
					t.Fatal(err)
				}
				var got any
				for part := range stream {
					if part.Error != nil {
						t.Fatal(part.Error)
					}
					if part.Type == fantasy.ObjectStreamPartTypeObject {
						got = part.Object
					}
					if part.Type == fantasy.ObjectStreamPartTypeFinish {
						usage = part.Usage
					}
				}
				if !reflect.DeepEqual(got, map[string]any{"value": "ok"}) {
					t.Fatalf("object=%v", got)
				}
				text = "ok"
			}
			if calls != 1 || text != "ok" || usage.InputTokens != 4 || usage.OutputTokens != 3 || usage.TotalTokens != 7 {
				t.Fatalf("calls=%d text=%q usage=%+v", calls, text, usage)
			}
		})
	}
}

func TestResponsesSDKDoesNotRetryProviderErrors(t *testing.T) {
	for _, status := range []int{400, 404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			m := sdkResponsesModel(t, "gpt-5", true, true, func(*http.Request) (*http.Response, error) {
				calls++
				return sdkJSONResponse(`{"error":{"message":"fixture rejection","type":"fixture"}}`, status), nil
			})
			_, err := m.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
			if err == nil || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestResponsesSDKSkipsReasoningHistory(t *testing.T) {
	var raw []byte
	m := sdkResponsesModel(t, "gpt-5", true, true, func(r *http.Request) (*http.Response, error) {
		var err error
		raw, err = io.ReadAll(r.Body)
		return sdkJSONResponse(`{"id":"r","status":"completed","output":[]}`, 200), err
	})
	_, err := m.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewUserMessage("visible question"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ReasoningPart{Text: "reasoning fixture marker", ProviderOptions: fantasy.ProviderOptions{openai.Name: &openai.ResponsesReasoningMetadata{ItemID: "reasoning-item", EncryptedContent: new("encrypted fixture marker")}}}, fantasy.TextPart{Text: "visible answer"}}},
		fantasy.NewUserMessage("follow up"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reasoning fixture marker") || strings.Contains(string(raw), "encrypted fixture marker") || !strings.Contains(string(raw), "visible answer") {
		t.Fatalf("unexpected history: %s", raw)
	}
}
