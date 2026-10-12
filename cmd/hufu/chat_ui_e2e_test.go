package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// chatFixtureServer is an OpenAI-compatible provider for the chat TUI E2E
// test. Prompts carry ZQ… tokens and the newest token in a request
// transcript identifies the current turn. The server answers
// "ANSWER <token>", blocks while the current token is chatFixtureSlowToken,
// and records every request transcript.
type chatFixtureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	slow     chan struct{}
}

var chatFixtureToken = regexp.MustCompile(`ZQ[A-Z]+[0-9]`)

const chatFixtureSlowToken = "ZQSLOW1"

func newChatFixtureServer(t *testing.T) *chatFixtureServer {
	t.Helper()
	fixture := &chatFixtureServer{slow: make(chan struct{}, 1)}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"test","object":"model"}]}`)
			return
		case "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}
		var request contractChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var transcript []string
		for _, message := range request.Messages {
			transcript = append(transcript, chatFixtureText(message.Content))
		}
		joined := strings.Join(transcript, "\n")
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, joined)
		fixture.mu.Unlock()
		token := "ZQNONE0"
		if tokens := chatFixtureToken.FindAllString(joined, -1); len(tokens) > 0 {
			token = tokens[len(tokens)-1]
		}
		if token == chatFixtureSlowToken {
			select {
			case fixture.slow <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-time.After(25 * time.Second):
			}
			return
		}
		writeChatFixtureAnswer(w, request.Stream, "ANSWER "+token)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("sandbox does not permit TCP fixture listener: %v", err)
		}
		t.Fatalf("start TCP fixture listener: %v", err)
	}
	fixture.Server = httptest.NewUnstartedServer(handler)
	fixture.Listener = listener
	fixture.Start()
	t.Cleanup(fixture.Close)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		for i, request := range fixture.requests {
			if len(request) > 600 {
				request = request[:300] + " … " + request[len(request)-300:]
			}
			t.Logf("provider request %d:\n%s", i, request)
		}
	})
	return fixture
}

func chatFixtureText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.Text)
	}
	return b.String()
}

func writeChatFixtureAnswer(w http.ResponseWriter, stream bool, answer string) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", answer)
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "fixture", "object": "chat.completion", "created": 1, "model": "test",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": answer}}},
		"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

// sawRequestWith reports whether one request carried every fragment.
func (f *chatFixtureServer) sawRequestWith(fragments ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		all := true
		for _, fragment := range fragments {
			all = all && strings.Contains(request, fragment)
		}
		if all {
			return true
		}
	}
	return false
}

// TestE2E_ChatUI_TurnsCancelAndExit drives the real binary in a PTY: two
// turns keep one conversation, Ctrl+C cancels only the running turn, and
// /exit leaves cleanly.
func TestE2E_ChatUI_TurnsCancelAndExit(t *testing.T) {
	binary := buildTestBinary(t)
	server := newChatFixtureServer(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	proc := startPTY(t, binary, []string{
		"chat", "--ui", "chat", "--default",
		"--provider-url", server.URL + "/v1", "--model", "test", "--coordinator-model", "test",
		"--context-window", "131072", "--max-rounds", "2", "--timeout", "20",
		"--workspace", workspace,
	}, 24, 80)
	proc.waitForOutput(t, "Message the team", 15*time.Second)

	proc.writeInput(t, "first question ZQKIWI7\r")
	proc.waitForOutput(t, "ANSWER ZQKIWI7", 15*time.Second)

	proc.writeInput(t, "second question ZQQUOKKA9\r")
	proc.waitForOutput(t, "ANSWER ZQQUOKKA9", 15*time.Second)
	if !server.sawRequestWith("ZQKIWI7", "ZQQUOKKA9") {
		t.Fatal("the second turn did not carry the first turn's conversation")
	}

	proc.writeInput(t, "slow turn "+chatFixtureSlowToken+"\r")
	select {
	case <-server.slow:
	case <-time.After(15 * time.Second):
		t.Fatal("slow turn never reached the provider")
	}
	proc.writeInput(t, "\x03")
	proc.waitForOutput(t, "cancelled", 15*time.Second)

	proc.writeInput(t, "after cancel ZQZEBRA5\r")
	proc.waitForOutput(t, "ANSWER ZQZEBRA5", 15*time.Second)

	proc.writeInput(t, "/exit\r")
	if code := proc.waitExit(t, 15*time.Second); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}
