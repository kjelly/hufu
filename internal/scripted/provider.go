package scripted

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
)

// Provider is a deterministic OpenAI-compatible transport. Each instance owns
// its script and cursor. It has no runtime, evaluator, or live-model fallback.
type Provider struct {
	mu           sync.Mutex
	model        string
	steps        []Step
	consumed     []bool
	cursor       int
	calls        int
	observeUsage func(context.Context, *Usage)
}

// NewObservedProvider preserves explicit response-usage presence for a trusted
// transport adapter. The observer is immutable and scoped by each request's
// context; it is never invoked for metadata or malformed requests.
func NewObservedProvider(model string, steps []Step, observer func(context.Context, *Usage)) *Provider {
	provider := NewProvider(model, steps)
	provider.observeUsage = observer
	return provider
}

func NewProvider(model string, steps []Step) *Provider {
	owned := make([]Step, len(steps))
	for i, step := range steps {
		owned[i] = cloneStep(step)
	}
	return &Provider{model: model, steps: owned, consumed: make([]bool, len(steps))}
}

func cloneStep(step Step) Step {
	if step.Match != nil {
		value := *step.Match
		step.Match = &value
	}
	if step.ToolCall != nil {
		value := *step.ToolCall
		step.ToolCall = &value
	}
	if step.Usage != nil {
		value := *step.Usage
		step.Usage = &value
	}
	return step
}

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.Context().Err(); err != nil {
		http.Error(w, "scripted request cancelled", http.StatusRequestTimeout)
		return
	}
	switch r.URL.Path {
	case "/v1/chat/completions":
		p.serveCompletion(w, r)
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	default:
		http.NotFound(w, r)
	}
}

// NextStep also supports offline script-contract tests. Matching is literal
// request matching supplied by the fixture, never a task-intent interpreter.
func (p *Provider) NextStep(body []byte) (Step, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, step := range p.steps {
		if !p.consumed[i] && step.Match != nil && step.Match.Contains != "" && bytes.Contains(body, []byte(step.Match.Contains)) {
			p.consumed[i] = true
			return cloneStep(step), nil
		}
	}
	for i := p.cursor; i < len(p.steps); i++ {
		if p.consumed[i] || p.steps[i].Match != nil {
			continue
		}
		p.consumed[i], p.cursor = true, i+1
		return cloneStep(p.steps[i]), nil
	}
	return Step{}, errors.New("no unconsumed step left to answer this request")
}

type Issue struct {
	Index  int
	Reason string
	Step   Step
}

func (p *Provider) Issues() []Issue {
	p.mu.Lock()
	defer p.mu.Unlock()
	var issues []Issue
	for i, done := range p.consumed {
		step := p.steps[i]
		switch {
		case step.Forbidden && done:
			issues = append(issues, Issue{i, "forbidden_consumed", cloneStep(step)})
		case !step.Forbidden && !done:
			issues = append(issues, Issue{i, "unused_step", cloneStep(step)})
		}
	}
	return issues
}

// ObservedUsage retains explicit presence, including nil. This detects SDKs
// that synthesize zero usage when no usage was sent.
func (p *Provider) ObservedUsage() []*Usage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var usages []*Usage
	for i, consumed := range p.consumed {
		if consumed {
			usages = append(usages, cloneStep(p.steps[i]).Usage)
		}
	}
	return usages
}

func (p *Provider) serveCompletion(w http.ResponseWriter, r *http.Request) {
	const requestLimit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	if err != nil {
		http.Error(w, "read scripted request failed", http.StatusBadRequest)
		return
	}
	if len(body) > requestLimit {
		http.Error(w, "scripted request exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	if r.Context().Err() != nil {
		http.Error(w, "scripted request cancelled", http.StatusRequestTimeout)
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid scripted request", http.StatusBadRequest)
		return
	}
	step, err := p.NextStep(body)
	if err != nil {
		http.Error(w, "scripted provider: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var usage *wireUsage
	if step.Usage != nil {
		if step.Usage.InputTokens < 0 || step.Usage.OutputTokens < 0 || step.Usage.OutputTokens > math.MaxInt64-step.Usage.InputTokens {
			http.Error(w, "invalid scripted usage", http.StatusBadRequest)
			return
		}
		usage = &wireUsage{step.Usage.InputTokens, step.Usage.OutputTokens, step.Usage.InputTokens + step.Usage.OutputTokens}
	}
	if p.observeUsage != nil {
		p.observeUsage(r.Context(), cloneStep(step).Usage)
	}
	delta := completionDelta{Role: "assistant", Content: step.Content}
	finish := "stop"
	if step.ToolCall != nil {
		finish = "tool_calls"
		p.mu.Lock()
		p.calls++
		callID := fmt.Sprintf("call-%d", p.calls)
		p.mu.Unlock()
		delta.ToolCalls = []completionToolCall{{Index: 0, ID: callID, Type: "function", Function: *step.ToolCall}}
	}
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChunk(w, completionChunk{ID: "eval", Object: "chat.completion.chunk", Created: 1, Model: p.model, Choices: []chunkChoice{{Index: 0, Delta: delta}}})
		writeChunk(w, completionChunk{ID: "eval", Object: "chat.completion.chunk", Created: 1, Model: p.model, Choices: []chunkChoice{{Index: 0, FinishReason: &finish}}})
		if usage != nil {
			writeChunk(w, completionChunk{ID: "eval", Object: "chat.completion.chunk", Created: 1, Model: p.model, Choices: []chunkChoice{}, Usage: usage})
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(completion{ID: "eval", Object: "chat.completion", Created: 1, Model: p.model,
		Choices: []completionChoice{{Index: 0, Message: delta, FinishReason: finish}}, Usage: usage})
	if err != nil {
		http.Error(w, "encode scripted response failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

type wireUsage struct {
	Input  int64 `json:"prompt_tokens"`
	Output int64 `json:"completion_tokens"`
	Total  int64 `json:"total_tokens"`
}

type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *wireUsage         `json:"usage,omitzero"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      completionDelta `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type completionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *wireUsage    `json:"usage,omitzero"`
}

type chunkChoice struct {
	Index        int             `json:"index"`
	Delta        completionDelta `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
}

type completionDelta struct {
	Role      string               `json:"role,omitempty"`
	Content   string               `json:"content,omitempty"`
	ToolCalls []completionToolCall `json:"tool_calls,omitempty"`
}

type completionToolCall struct {
	Index    int      `json:"index"`
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function ToolCall `json:"function"`
}

func writeChunk(w http.ResponseWriter, chunk completionChunk) {
	raw, err := json.Marshal(chunk)
	if err != nil {
		return // owned string/integer fields cannot fail JSON encoding
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
}
