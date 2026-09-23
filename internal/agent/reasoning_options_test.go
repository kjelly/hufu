package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

type namedLanguageModel struct {
	fantasy.LanguageModel
	name string
}

func (m namedLanguageModel) Provider() string { return m.name }

func TestReasoningEffortProviderOptionsKeysByProviderName(t *testing.T) {
	cases := []struct {
		name  string
		model fantasy.LanguageModel
		want  string
	}{
		{name: "configured provider name", model: namedLanguageModel{name: "local"}, want: "local"},
		{name: "nil model falls back", model: nil, want: openaicompat.Name},
		{name: "empty provider falls back", model: namedLanguageModel{}, want: openaicompat.Name},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := ReasoningEffortProviderOptions(tc.model, "none")
			value, ok := opts[tc.want].(*openaicompat.ProviderOptions)
			if !ok || len(opts) != 1 || value.ReasoningEffort == nil || *value.ReasoningEffort != openai.ReasoningEffortNone {
				t.Fatalf("options = %#v, want one entry under %q with effort none", opts, tc.want)
			}
		})
	}
}

// TestReasoningEffortReachesOpenAICompatibleRequest pins the end-to-end
// contract: with a provider named "local" the effort must appear in the
// request body, which openaicompat.NewProviderOptions alone never achieved.
func TestReasoningEffortReachesOpenAICompatibleRequest(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(raw, &body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	provider, err := newOpenAICompatProvider(server.URL, "key", "local", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.LanguageModel(context.Background(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}, ProviderOptions: ReasoningEffortProviderOptions(model, "none")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body["reasoning_effort"] != "none" {
		t.Fatalf("request body = %v, want reasoning_effort none", body)
	}
}
