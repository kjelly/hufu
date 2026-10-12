package conversation

import "context"

// work is one admitted command waiting for, or holding, the actor.
type work struct {
	cmd    Command
	turnID string
	ctx    context.Context
	cancel context.CancelFunc
}

func (w *work) isPrompt() bool { return w.cmd.Kind == CommandSubmitPrompt }

// ackWindow remembers the Ack of the most recent command IDs so a resubmitted
// command is answered idempotently instead of being executed twice.
type ackWindow struct {
	limit int
	order []string
	acks  map[string]Ack
}

func newAckWindow(limit int) *ackWindow {
	return &ackWindow{limit: limit, acks: make(map[string]Ack, limit)}
}

func (w *ackWindow) get(id string) (Ack, bool) {
	ack, ok := w.acks[id]
	return ack, ok
}

func (w *ackWindow) put(id string, ack Ack) {
	if _, exists := w.acks[id]; exists {
		w.acks[id] = ack
		return
	}
	if len(w.order) >= w.limit {
		oldest := w.order[0]
		w.order = w.order[1:]
		delete(w.acks, oldest)
	}
	w.order = append(w.order, id)
	w.acks[id] = ack
}
