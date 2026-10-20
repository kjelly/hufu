package scripted

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderUsageStreamingAndNonStreaming(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			name := "plain"
			if stream {
				name = "stream"
			}
			if known {
				name += "_known"
			} else {
				name += "_unknown"
			}
			t.Run(name, func(t *testing.T) {
				step := Step{ToolCall: &ToolCall{Name: "submit_result", Arguments: `{"status":"success"}`}}
				if known {
					step.Usage = &Usage{InputTokens: 100, OutputTokens: 20}
				}
				provider := NewProvider("scripted", []Step{step})
				raw, err := json.Marshal(map[string]any{"stream": stream, "messages": []any{}})
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://fixture.invalid/v1/chat/completions", bytes.NewReader(raw))
				response := httptest.NewRecorder()
				provider.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Fatalf("%d: %s", response.Code, response.Body.String())
				}
				if stream {
					if response.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
						t.Fatal("invalid stream")
					}
					var usages int
					for line := range strings.SplitSeq(response.Body.String(), "\n") {
						if !strings.HasPrefix(line, "data: {") {
							continue
						}
						var chunk completionChunk
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
							t.Fatal(err)
						}
						if chunk.Usage != nil {
							usages++
							if chunk.Usage.Total != 120 || chunk.Usage.Input != 100 || chunk.Usage.Output != 20 || len(chunk.Choices) != 0 {
								t.Fatalf("%+v", chunk)
							}
						}
					}
					if (usages == 1) != known || usages > 1 {
						t.Fatalf("usage chunks=%d known=%v", usages, known)
					}
				} else {
					var value completion
					if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
						t.Fatal(err)
					}
					if (value.Usage != nil) != known {
						t.Fatalf("usage=%+v", value.Usage)
					}
					if known && value.Usage.Total != 120 {
						t.Fatalf("%+v", value.Usage)
					}
				}
				observed := provider.ObservedUsage()
				if len(observed) != 1 || (observed[0] != nil) != known {
					t.Fatalf("observed=%+v", observed)
				}
				if len(provider.Issues()) != 0 {
					t.Fatal("script not consumed")
				}
			})
		}
	}
}

func TestProviderOwnsIndependentFrozenScripts(t *testing.T) {
	steps := []Step{{Content: "original", Usage: &Usage{InputTokens: 3, OutputTokens: 4}}}
	a, b := NewProvider("a", steps), NewProvider("b", steps)
	steps[0].Content = "changed"
	steps[0].Usage.InputTokens = 999
	first, err := a.NextStep(nil)
	if err != nil || first.Content != "original" || first.Usage.InputTokens != 3 {
		t.Fatalf("%+v %v", first, err)
	}
	first.Usage.InputTokens = 888
	second, err := b.NextStep(nil)
	if err != nil || second.Content != "original" || second.Usage.InputTokens != 3 {
		t.Fatalf("%+v %v", second, err)
	}
	if got := a.ObservedUsage(); got[0].InputTokens != 3 {
		t.Fatal("returned step mutated provider")
	}
	if _, err := a.NextStep(nil); err == nil {
		t.Fatal("exhausted script fell back")
	}
}

func TestProviderCancelledRequestConsumesNothing(t *testing.T) {
	provider := NewProvider("scripted", []Step{{Content: "unused"}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/chat/completions", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	provider.ServeHTTP(response, request)
	if response.Code != http.StatusRequestTimeout || len(provider.ObservedUsage()) != 0 || len(provider.Issues()) != 1 {
		t.Fatal("cancelled request consumed script")
	}
}
