package scripted

import (
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
)

// serveResponses projects the same deterministic steps as Chat Completions.
// The model chooses no new actions: call names and arguments come exclusively
// from the fixture. Unknown usage stays absent on the wire.
func (p *Provider) serveResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "scripted Responses requires POST", http.StatusMethodNotAllowed)
		return
	}
	const requestLimit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	if err != nil {
		http.Error(w, "read scripted request failed", http.StatusBadRequest)
		return
	}
	if len(raw) > requestLimit {
		http.Error(w, "scripted request exceeds size limit", http.StatusRequestEntityTooLarge)
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		http.Error(w, "invalid scripted request", http.StatusBadRequest)
		return
	}
	if r.Context().Err() != nil {
		http.Error(w, "scripted request cancelled", http.StatusRequestTimeout)
		return
	}
	step, err := p.NextStep(raw)
	if err != nil {
		http.Error(w, "scripted provider: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if step.Usage != nil && (step.Usage.InputTokens < 0 || step.Usage.OutputTokens < 0 || step.Usage.OutputTokens > math.MaxInt64-step.Usage.InputTokens) {
		http.Error(w, "invalid scripted usage", http.StatusBadRequest)
		return
	}
	if p.observeUsage != nil {
		p.observeUsage(r.Context(), cloneStep(step).Usage)
	}
	p.mu.Lock()
	p.calls++
	id := fmt.Sprintf("response-%d", p.calls)
	p.mu.Unlock()
	output := scriptedResponseItem{Type: "message", ID: id + "-message", Role: "assistant", Status: "completed", Content: []scriptedResponseText{{Type: "output_text", Text: step.Content}}}
	if step.ToolCall != nil {
		output = scriptedResponseItem{Type: "function_call", ID: id + "-function", Status: "completed", CallID: id + "-call", Name: step.ToolCall.Name, Arguments: step.ToolCall.Arguments}
	}
	response := scriptedResponse{ID: id, Object: "response", Model: p.model, Status: "completed", Output: []scriptedResponseItem{output}}
	if step.Usage != nil {
		response.Usage = &scriptedResponseUsage{Input: step.Usage.InputTokens, Output: step.Usage.OutputTokens, Total: step.Usage.InputTokens + step.Usage.OutputTokens}
	}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		data, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			http.Error(w, "encode scripted response failed", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	writeResponseEvent(w, "response.created", map[string]any{"response": scriptedResponse{ID: id, Object: "response", Model: p.model, Status: "in_progress", Output: []scriptedResponseItem{}}})
	initial := output
	initial.Status = "in_progress"
	initial.Arguments = ""
	if step.ToolCall == nil {
		initial.Content = []scriptedResponseText{}
	}
	writeResponseEvent(w, "response.output_item.added", map[string]any{"output_index": 0, "item": initial})
	if step.ToolCall == nil {
		writeResponseEvent(w, "response.output_text.delta", map[string]any{"output_index": 0, "content_index": 0, "item_id": output.ID, "delta": step.Content})
	} else {
		writeResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": 0, "item_id": output.ID, "delta": step.ToolCall.Arguments})
	}
	writeResponseEvent(w, "response.output_item.done", map[string]any{"output_index": 0, "item": output})
	writeResponseEvent(w, "response.completed", map[string]any{"response": response})
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
}

func writeResponseEvent(w io.Writer, kind string, payload map[string]any) {
	payload["type"] = kind
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, data)
}

type scriptedResponse struct {
	ID     string                 `json:"id"`
	Object string                 `json:"object"`
	Model  string                 `json:"model"`
	Status string                 `json:"status"`
	Output []scriptedResponseItem `json:"output"`
	Usage  *scriptedResponseUsage `json:"usage,omitzero"`
}

type scriptedResponseItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id"`
	Status    string                 `json:"status"`
	Role      string                 `json:"role,omitempty"`
	Content   []scriptedResponseText `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
}

type scriptedResponseText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type scriptedResponseUsage struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}
