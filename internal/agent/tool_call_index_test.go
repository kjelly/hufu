package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"charm.land/fantasy"
)

type streamedToolCall struct {
	Index int64
	ID    string
	Name  string
	Args  string
}

func sseToolChunk(calls ...string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":null}]}` + "\n\n"
}

func sseToolCall(index int, id, name, args string) string {
	call := map[string]any{"index": index, "type": "function", "function": map[string]any{"arguments": args}}
	if id != "" {
		call["id"] = id
	}
	if name != "" {
		call["function"].(map[string]any)["name"] = name
	}
	raw, _ := json.Marshal(call)
	return string(raw)
}

const sseDone = "data: [DONE]\n\n"

// rewriteToolCallStream reads a stream one byte at a time so the body's
// buffering, not just its line rewriting, is exercised.
func rewriteToolCallStream(t *testing.T, stream string) string {
	t.Helper()
	source := iotest.OneByteReader(strings.NewReader(stream))
	body := &toolCallIndexBody{source: io.NopCloser(source), reader: bufio.NewReader(source), choices: map[int64]*toolCallIndexes{}}
	out, err := io.ReadAll(iotest.OneByteReader(body))
	if err != nil {
		t.Fatalf("read rewritten stream: %v", err)
	}
	return string(out)
}

func streamToolCalls(t *testing.T, stream string) [][]streamedToolCall {
	t.Helper()
	var lines [][]streamedToolCall
	for _, line := range strings.Split(stream, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok || !strings.Contains(payload, `"tool_calls"`) {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int64  `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		var calls []streamedToolCall
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				calls = append(calls, streamedToolCall{Index: call.Index, ID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments})
			}
		}
		lines = append(lines, calls)
	}
	return lines
}

func TestToolCallIndexBodyGivesEachCallItsOwnIndex(t *testing.T) {
	view := sseToolCall(0, "call_1", "view", `{"file_path":"a.go"}`)
	cases := []struct {
		name      string
		stream    string
		want      [][]int64
		unchanged bool
	}{
		{
			name:   "parallel calls in one chunk share index 0",
			stream: sseToolChunk(view, sseToolCall(0, "call_2", "ls", `{"path":"pkg"}`)) + sseDone,
			want:   [][]int64{{0, 1}},
		},
		{
			name:   "calls in separate chunks share index 0",
			stream: sseToolChunk(view) + sseToolChunk(sseToolCall(0, "call_2", "ls", `{"path":"pkg"}`)) + sseDone,
			want:   [][]int64{{0}, {1}},
		},
		{
			name: "incremental stream with distinct indexes is unchanged",
			stream: sseToolChunk(sseToolCall(0, "call_1", "view", "")) + sseToolChunk(sseToolCall(0, "", "", `{"file_path":"a.go"}`)) +
				sseToolChunk(sseToolCall(1, "call_2", "ls", "")) + sseToolChunk(sseToolCall(1, "", "", `{"path":"pkg"}`)) + sseDone,
			want:      [][]int64{{0}, {0}, {1}, {1}},
			unchanged: true,
		},
		{
			name:      "text, comments, and unparsable lines pass through",
			stream:    `data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n: keep-alive\n\ndata: {\"tool_calls\": [\n\n" + sseDone,
			unchanged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteToolCallStream(t, tc.stream)
			if tc.unchanged && got != tc.stream {
				t.Fatalf("stream changed:\n got %q\nwant %q", got, tc.stream)
			}
			before, after := streamToolCalls(t, tc.stream), streamToolCalls(t, got)
			if len(after) != len(tc.want) {
				t.Fatalf("tool-call lines = %d, want %d: %q", len(after), len(tc.want), got)
			}
			for i, calls := range after {
				if len(calls) != len(tc.want[i]) {
					t.Fatalf("line %d calls = %+v, want indexes %v", i, calls, tc.want[i])
				}
				for j, call := range calls {
					original := before[i][j]
					original.Index = tc.want[i][j]
					if call != original {
						t.Fatalf("line %d call %d = %+v, want %+v", i, j, call, original)
					}
				}
			}
			if !strings.HasSuffix(got, sseDone) && strings.HasSuffix(tc.stream, sseDone) {
				t.Fatalf("stream lost its terminator: %q", got)
			}
		})
	}
}

func TestToolCallIndexBodyKeepsLineFraming(t *testing.T) {
	stream := strings.ReplaceAll(sseToolChunk(sseToolCall(0, "call_1", "view", "{}"), sseToolCall(0, "call_2", "ls", "{}")), "data: ", "data:")
	stream = strings.ReplaceAll(stream, "\n", "\r\n")
	got := rewriteToolCallStream(t, stream)
	if !strings.HasPrefix(got, "data:{") || !strings.HasSuffix(got, "}\r\n\r\n") || strings.Count(got, "\r\n") != 2 {
		t.Fatalf("framing changed: %q", got)
	}
	if calls := streamToolCalls(t, got); len(calls) != 1 || len(calls[0]) != 2 || calls[0][1].Index != 1 {
		t.Fatalf("calls = %+v, want indexes 0 and 1", calls)
	}
}

// TestOpenAICompatStreamSeparatesParallelToolCallsSharingAnIndex pins the
// provider contract with the stream Ollama 0.35.0 sends for parallel tool
// calls: both must reach the caller with their own name and arguments rather
// than as one call with concatenated JSON.
func TestOpenAICompatStreamSeparatesParallelToolCallsSharingAnIndex(t *testing.T) {
	stream := sseToolChunk(sseToolCall(0, "call_1", "view", `{"file_path":"a.go"}`), sseToolCall(0, "call_2", "ls", `{"path":"pkg"}`)) +
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}` + "\n\n" + sseDone
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()

	for _, client := range []struct {
		name   string
		client *http.Client
	}{{name: "default client"}, {name: "boundary client", client: server.Client()}} {
		t.Run(client.name, func(t *testing.T) {
			provider, err := newOpenAICompatProvider(server.URL, "", "local", client.client)
			if err != nil {
				t.Fatal(err)
			}
			model, err := provider.LanguageModel(context.Background(), "m")
			if err != nil {
				t.Fatal(err)
			}
			parts, err := model.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for part := range parts {
				if part.Type == fantasy.StreamPartTypeError {
					t.Fatalf("stream error: %v", part.Error)
				}
				if part.Type == fantasy.StreamPartTypeToolCall {
					got = append(got, part.ToolCallName+" "+part.ToolCallInput)
				}
			}
			want := []string{`view {"file_path":"a.go"}`, `ls {"path":"pkg"}`}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("tool calls = %q, want %q", got, want)
			}
		})
	}
}

func TestWithToolCallIndexesLeavesNonStreamResponses(t *testing.T) {
	const body = `{"choices":[{"index":0,"message":{"tool_calls":[{"index":0,"id":"a"},{"index":0,"id":"b"}]}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	original := server.Client()
	client := withToolCallIndexes(original)
	if client == original || original.Transport == client.Transport {
		t.Fatal("withToolCallIndexes must wrap a copy and leave the caller's client unchanged")
	}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(raw, []byte(body)) {
		t.Fatalf("body = %s, want it unchanged", raw)
	}
}
