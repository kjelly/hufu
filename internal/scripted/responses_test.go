package scripted

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesBoundaryFixtures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			for _, known := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream_%t/tool_%t/usage_%t", stream, tool, known), func(t *testing.T) {
					step := Step{Content: "answer"}
					if tool {
						step.ToolCall = &ToolCall{Name: "lookup", Arguments: `{"value":"marker"}`}
					}
					if known {
						step.Usage = &Usage{InputTokens: 5, OutputTokens: 2}
					}
					var observed *Usage
					observations := 0
					p := NewObservedProvider("fixture", []Step{step}, func(_ context.Context, usage *Usage) { observations++; observed = usage })
					b, err := NewBoundary(t.Context(), p)
					if err != nil {
						t.Fatal(err)
					}
					defer b.Stop()
					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.URL()+"/responses", strings.NewReader(fmt.Sprintf(`{"stream":%t}`, stream)))
					if err != nil {
						t.Fatal(err)
					}
					response, err := b.HTTPClient().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					raw, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != 200 || observations != 1 || (observed != nil) != known || len(p.Issues()) != 0 {
						t.Fatalf("status=%d body=%s observations=%d usage=%+v issues=%v", response.StatusCode, raw, observations, observed, p.Issues())
					}
					payload := raw
					if stream {
						marker := "event: response.completed\ndata: "
						_, tail, found := strings.Cut(string(raw), marker)
						if !found || !strings.HasPrefix(string(raw), "event: response.created\n") {
							t.Fatalf("events=%s", raw)
						}
						line, _, _ := strings.Cut(tail, "\n")
						var event struct {
							Response jsontext.Value `json:"response"`
						}
						if err = json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatal(err)
						}
						payload = event.Response
					}
					var result scriptedResponse
					if err = json.Unmarshal(payload, &result); err != nil {
						t.Fatal(err)
					}
					if result.Status != "completed" || len(result.Output) != 1 || (result.Usage != nil) != known {
						t.Fatalf("response=%+v", result)
					}
					if known && result.Usage.Total != 7 {
						t.Fatalf("usage=%+v", result.Usage)
					}
					item := result.Output[0]
					if tool {
						if item.Type != "function_call" || item.CallID == "" || item.Name != "lookup" || item.Arguments != step.ToolCall.Arguments {
							t.Fatalf("item=%+v", item)
						}
					} else if len(item.Content) != 1 || item.Content[0].Text != "answer" {
						t.Fatalf("item=%+v", item)
					}
				})
			}
		}
	}
}

func TestResponsesBoundaryRejectsMalformedRequest(t *testing.T) {
	for _, tc := range []struct {
		name, method, body string
		status             int
	}{
		{"method", http.MethodGet, "{}", http.StatusMethodNotAllowed},
		{"json", http.MethodPost, "{", http.StatusBadRequest},
		{"oversized", http.MethodPost, strings.Repeat(" ", (4<<20)+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProvider("fixture", []Step{{Content: "unused"}})
			b, err := NewBoundary(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Stop()
			request, err := http.NewRequestWithContext(t.Context(), tc.method, b.URL()+"/responses", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := b.HTTPClient().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status || len(p.Issues()) != 1 {
				t.Fatalf("status=%d issues=%v", response.StatusCode, p.Issues())
			}
		})
	}
}
