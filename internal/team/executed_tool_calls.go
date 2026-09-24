package team

import (
	"context"
	"sync"
)

// executedToolCall is one tool call the policy gate let through to its tool.
type executedToolCall struct {
	name  string
	input string
}

// executedToolCallRecorder records, for one attempt, every tool call that
// passed authorization and reached its tool. It is recorded before the tool
// runs, so a call that fails half-way still counts. Fantasy drops a failed
// step's messages, so an attempt ended by a provider error has no steps to
// inspect; this recorder is the only evidence of what it already did. It is
// in-memory only: the fallback decision is made in the same process.
type executedToolCallRecorder struct {
	mu    sync.Mutex
	calls []executedToolCall
}

type executedToolCallRecorderKey struct{}

func withExecutedToolCallRecorder(ctx context.Context, recorder *executedToolCallRecorder) context.Context {
	return context.WithValue(ctx, executedToolCallRecorderKey{}, recorder)
}

func recordExecutedToolCall(ctx context.Context, name, input string) {
	recorder, _ := ctx.Value(executedToolCallRecorderKey{}).(*executedToolCallRecorder)
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	recorder.calls = append(recorder.calls, executedToolCall{name: name, input: input})
	recorder.mu.Unlock()
}

func (r *executedToolCallRecorder) snapshot() []executedToolCall {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]executedToolCall(nil), r.calls...)
}
