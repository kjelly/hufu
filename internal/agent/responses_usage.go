package agent

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
)

// responsesUsageBody observes bounded accounting and terminal metadata. It never
// rewrites bytes, builds generation output, dispatches tools, or replaces the
// SDK's JSON/SSE parser. Oversized or incomplete metadata stays unknown.
type responsesUsageBody struct {
	source        io.ReadCloser
	ctx           context.Context
	stream        bool
	mu            sync.Mutex
	buffer        []byte
	eventData     []byte
	eventOverflow bool
	overflow      bool
	terminal      bool
	closed        bool
	closeOnce     sync.Once
	closeErr      error
}

func observeResponsesBody(req *http.Request, resp *http.Response) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses") || resp == nil || resp.Body == nil {
		return
	}
	_, usage := req.Context().Value(providerUsageKey{}).(*providerUsageObservation)
	_, terminal := req.Context().Value(responsesTerminalKey{}).(*responsesTerminalObservation)
	if !usage && !terminal {
		return
	}
	resp.Body = &responsesUsageBody{source: resp.Body, ctx: req.Context(), stream: isEventStream(resp.Header)}
}

func (b *responsesUsageBody) Read(p []byte) (int, error) {
	n, err := b.source.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return n, err
	}
	if b.stream {
		chunk := p[:n]
		for len(chunk) > 0 {
			line, rest, complete := bytes.Cut(chunk, []byte{'\n'})
			if !b.overflow {
				if len(b.buffer)+len(b.eventData)+len(line) > maxObservedProviderBody {
					b.buffer = nil
					b.eventData = nil
					b.overflow = true
					b.eventOverflow = true
				} else {
					b.buffer = append(b.buffer, line...)
				}
			}
			if complete {
				if !b.overflow {
					b.observeLine(b.buffer)
				}
				b.buffer = b.buffer[:0]
				b.overflow = false
				chunk = rest
			} else {
				break
			}
		}
	} else if !b.overflow {
		if len(b.buffer)+n > maxObservedProviderBody {
			b.buffer = nil
			b.overflow = true
		} else {
			b.buffer = append(b.buffer, p[:n]...)
		}
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *responsesUsageBody) Close() error {
	b.mu.Lock()
	if !b.closed {
		b.finish()
		b.closed = true
	}
	b.buffer = nil
	b.eventData = nil
	b.mu.Unlock()
	b.closeOnce.Do(func() { b.closeErr = b.source.Close() })
	return b.closeErr
}

func (b *responsesUsageBody) observeLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		if !b.eventOverflow && len(b.eventData) > 0 {
			b.observeEvent(b.eventData)
		}
		b.eventData = b.eventData[:0]
		b.eventOverflow = false
		return
	}
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok || b.eventOverflow {
		return
	}
	if len(payload) > 0 && payload[0] == ' ' {
		payload = payload[1:]
	}
	if len(b.eventData)+len(payload)+1 > maxObservedProviderBody {
		b.eventData = nil
		b.eventOverflow = true
		return
	}
	b.eventData = append(b.eventData, payload...)
	b.eventData = append(b.eventData, '\n')
}

func (b *responsesUsageBody) observeEvent(payload []byte) {
	var event struct {
		Type     string                 `json:"type"`
		Response responsesUsageEnvelope `json:"response"`
	}
	if json.Unmarshal(bytes.TrimSpace(payload), &event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		b.terminal = true
		status := strings.TrimPrefix(event.Type, "response.")
		if event.Response.Status != "" && event.Response.Status != status {
			status = "invalid"
		}
		recordResponsesTerminal(b.ctx, status)
		recordResponsesWireUsage(b.ctx, normalizeResponsesWireUsage(event.Response.Usage), true)
	}
}

func (b *responsesUsageBody) finish() {
	if b.stream {
		if len(b.buffer) > 0 && !b.overflow {
			b.observeLine(b.buffer)
		}
		if len(b.eventData) > 0 && !b.eventOverflow {
			b.observeEvent(b.eventData)
			b.eventData = b.eventData[:0]
		}
		if !b.terminal {
			recordResponsesWireUsage(b.ctx, nil, false)
		}
	} else {
		var response responsesUsageEnvelope
		valid := !b.overflow && json.Unmarshal(b.buffer, &response) == nil && (response.Status == "completed" || response.Status == "incomplete" || response.Status == "failed")
		if !b.terminal {
			b.terminal = true
			if valid {
				recordResponsesTerminal(b.ctx, response.Status)
				recordResponsesWireUsage(b.ctx, normalizeResponsesWireUsage(response.Usage), true)
			} else {
				recordResponsesTerminal(b.ctx, "invalid")
				recordResponsesWireUsage(b.ctx, nil, false)
			}
		}
	}
	// Prevent repeated EOF/Close from fabricating additional observations.
	b.closed = true
}

type responsesUsageEnvelope struct {
	Status string              `json:"status"`
	Usage  *responsesWireUsage `json:"usage"`
}

type responsesWireUsage struct {
	Input        *int64 `json:"input_tokens"`
	Output       *int64 `json:"output_tokens"`
	Total        *int64 `json:"total_tokens"`
	InputDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		Reasoning int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func normalizeResponsesWireUsage(raw *responsesWireUsage) *ProviderInvocationUsage {
	if raw == nil || raw.Input == nil || raw.Output == nil || raw.Total == nil {
		return nil
	}
	input, output, cached, reasoning := *raw.Input, *raw.Output, raw.InputDetails.Cached, raw.OutputDetails.Reasoning
	if input < 0 || output < 0 || cached < 0 || cached > input || reasoning < 0 || reasoning > output || input > math.MaxInt64-output || *raw.Total != input+output {
		return nil
	}
	return &ProviderInvocationUsage{InputTokens: input - cached, CacheReadTokens: cached, OutputTokens: output, TotalTokens: *raw.Total}
}

// A trusted fixture can already have recorded usage on this same invocation.
// Reconcile it against the actual wire metadata; two wire terminal events or
// disagreement fail closed. The public observer's duplicate rule is unchanged.
func recordResponsesWireUsage(ctx context.Context, usage *ProviderInvocationUsage, observed bool) {
	observation, ok := ctx.Value(providerUsageKey{}).(*providerUsageObservation)
	if !ok {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.responseObserved = observation.responseObserved || observed
	if observation.wireReported {
		observation.conflict = true
		return
	}
	observation.wireReported = true
	if observation.reported {
		if (observation.usage == nil) != (usage == nil) || usage != nil && observation.usage != nil && *observation.usage != *usage {
			observation.conflict = true
		}
		return
	}
	observation.reported = true
	if usage != nil {
		owned := *usage
		observation.usage = &owned
	}
}

func responsesWireObserved(ctx context.Context) bool {
	observation, ok := ctx.Value(providerUsageKey{}).(*providerUsageObservation)
	if !ok {
		return false
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.responseObserved
}

type responsesTerminalKey struct{}
type responsesTerminalObservation struct {
	mu     sync.Mutex
	status string
}

func withResponsesTerminal(ctx context.Context) context.Context {
	return context.WithValue(ctx, responsesTerminalKey{}, &responsesTerminalObservation{})
}

func recordResponsesTerminal(ctx context.Context, status string) {
	observation, ok := ctx.Value(responsesTerminalKey{}).(*responsesTerminalObservation)
	if !ok {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.status != "" {
		observation.status = "invalid"
	} else {
		observation.status = status
	}
}

func responsesTerminalStatus(ctx context.Context) string {
	observation, ok := ctx.Value(responsesTerminalKey{}).(*responsesTerminalObservation)
	if !ok {
		return ""
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.status
}
