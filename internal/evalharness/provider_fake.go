package evalharness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// evalModelDriverName is the model identity reported in every scripted
// response. Its exact value is never asserted on by a case; it exists only
// so response payloads are well-formed OpenAI-compatible JSON.
const evalModelDriverName = "eval-harness-model"

// scriptedProvider is the EvalModelDriver from
// docs/tmp/now/06-workflow-regression-eval-harness.md §4: an
// OpenAI-compatible HTTP fake serving scripted chat completions, following
// internal/team's fixedAnswerProvider/fakeJudge pattern. One instance backs
// every model call -- worker, judge, and coordinator alike -- for a single
// case; a case gets its own fresh instance, so there is no cross-case reset.
type scriptedProvider struct {
	mu       sync.Mutex
	steps    []ProviderStep
	consumed []bool
	cursor   int
	calls    int
}

func newScriptedProvider(fixture *ProviderFixture) *scriptedProvider {
	return &scriptedProvider{
		steps:    fixture.Steps,
		consumed: make([]bool, len(fixture.Steps)),
	}
}

func (p *scriptedProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/chat/completions":
		p.serveChatCompletion(w, r)
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	default:
		http.NotFound(w, r)
	}
}

func (p *scriptedProvider) serveChatCompletion(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read chat completion request: %v", err), http.StatusBadRequest)
		return
	}
	step, err := p.nextStep(body)
	if err != nil {
		http.Error(w, fmt.Sprintf("scripted provider: %v", err), http.StatusInternalServerError)
		return
	}

	delta := sseDelta{Role: "assistant"}
	finishReason := "stop"
	if step.ToolCall != nil {
		finishReason = "tool_calls"
		p.mu.Lock()
		p.calls++
		callID := fmt.Sprintf("call-%d", p.calls)
		p.mu.Unlock()
		delta.ToolCalls = []sseToolCall{{
			Index: 0,
			ID:    callID,
			Type:  "function",
			Function: sseFunctionCall{
				Name:      step.ToolCall.Name,
				Arguments: step.ToolCall.Arguments,
			},
		}}
	} else {
		delta.Content = step.Content
	}

	// The worker/coordinator dispatch loop (fantasy) always streams
	// ("stream":true), but the decision engine's sidecar client ("ask" in
	// internal/team/decision_runners.go, used for options/judge/challenge/
	// revision stages) sends a plain, non-streaming request and errors
	// ("expected destination type of 'string' or '[]byte' for responses
	// with content-type 'text/event-stream'") if answered with SSE. Detect
	// which shape the caller wants from its own request instead of always
	// answering one way.
	if requestWantsStream(body) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(w, sseChunk{
			ID: "eval", Object: "chat.completion.chunk", Created: 1, Model: evalModelDriverName,
			Choices: []sseChoice{{Index: 0, Delta: delta}},
		})
		writeSSEChunk(w, sseChunk{
			ID: "eval", Object: "chat.completion.chunk", Created: 1, Model: evalModelDriverName,
			Choices: []sseChoice{{Index: 0, Delta: sseDelta{}, FinishReason: &finishReason}},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	encoded, err := json.Marshal(chatCompletion{
		ID: "eval", Object: "chat.completion", Created: 1, Model: evalModelDriverName,
		Choices: []chatCompletionChoice{{Index: 0, Message: delta, FinishReason: finishReason}},
	})
	if err != nil {
		panic(fmt.Sprintf("evalharness: marshal scripted chat completion: %v", err))
	}
	_, _ = w.Write(encoded)
}

func requestWantsStream(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return false
	}
	return request.Stream
}

type chatCompletion struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
}

type chatCompletionChoice struct {
	Index        int      `json:"index"`
	Message      sseDelta `json:"message"`
	FinishReason string   `json:"finish_reason"`
}

// nextStep picks the next scripted step for an incoming request: a matched,
// unconsumed step wins first (checked in fixture order), otherwise the next
// unconsumed unmatched step in arrival order.
func (p *scriptedProvider) nextStep(requestBody []byte) (ProviderStep, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, step := range p.steps {
		if p.consumed[i] || step.Match == nil {
			continue
		}
		if step.Match.Contains != "" && bytes.Contains(requestBody, []byte(step.Match.Contains)) {
			p.consumed[i] = true
			return step, nil
		}
	}
	for i := p.cursor; i < len(p.steps); i++ {
		if p.consumed[i] || p.steps[i].Match != nil {
			continue
		}
		p.consumed[i] = true
		p.cursor = i + 1
		return p.steps[i], nil
	}
	return ProviderStep{}, fmt.Errorf("no unconsumed step left to answer this request")
}

// unconsumed returns every scripted step the case never triggered, so a
// runner can flag an over-specified fixture as a finding rather than silently
// ignoring dead script entries.
func (p *scriptedProvider) unconsumed() []ProviderStep {
	p.mu.Lock()
	defer p.mu.Unlock()
	var left []ProviderStep
	for i, done := range p.consumed {
		if !done {
			left = append(left, p.steps[i])
		}
	}
	return left
}

type sseChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseDelta struct {
	Role      string        `json:"role,omitempty"`
	Content   string        `json:"content,omitempty"`
	ToolCalls []sseToolCall `json:"tool_calls,omitempty"`
}

type sseToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function sseFunctionCall `json:"function"`
}

type sseFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func writeSSEChunk(w http.ResponseWriter, chunk sseChunk) {
	encoded, err := json.Marshal(chunk)
	if err != nil {
		// The chunk is built entirely from this package's own fields; a
		// marshal failure here means a fixture author's string escaped past
		// its Go string boundary, which cannot happen through encoding/json.
		panic(fmt.Sprintf("evalharness: marshal scripted sse chunk: %v", err))
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
}
