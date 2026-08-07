package team

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// taskToolSequenceKey carries one attempt-local closed tool sequence. It is
// deliberately task-scoped rather than agent-scoped: the same worker may run
// different atomic checkpoints in one team.
type taskToolSequenceKey struct{}

// taskToolSequence reserves each admitted call before its tool runs. Counting
// the invocation, rather than only successful tool responses, prevents a
// failed command from silently creating extra budget for retries or probes.
type taskToolSequence struct {
	mu       sync.Mutex
	sequence []string
	next     int
}

func newTaskToolSequence(sequence []string) *taskToolSequence {
	if len(sequence) == 0 {
		return nil
	}
	copyOf := make([]string, len(sequence))
	for i, name := range sequence {
		copyOf[i] = strings.TrimSpace(name)
	}
	return &taskToolSequence{sequence: copyOf}
}

// reserve admits exactly the next configured tool. A mismatch never reaches
// the underlying tool, so a worker cannot spend post-checkpoint budget on an
// exploratory tool or delegate after the terminal action has been satisfied.
func (s *taskToolSequence) reserve(tool string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.sequence) {
		return "closed tool sequence is complete; do not call another tool"
	}
	expected := s.sequence[s.next]
	if tool != expected {
		return fmt.Sprintf("closed tool sequence violation: expected tool %q at position %d of %d, got %q; do not call it", expected, s.next+1, len(s.sequence), tool)
	}
	s.next++
	return ""
}

func taskToolSequenceFromContext(ctx context.Context) *taskToolSequence {
	sequence, _ := ctx.Value(taskToolSequenceKey{}).(*taskToolSequence)
	return sequence
}

// filterToolsForSequence keeps only tools that can appear in the closed
// sequence. The gate remains the authoritative enforcement layer, while this
// removes unrelated convenience tools from the model's visible choices.
func filterToolsForSequence(agentTools []fantasy.AgentTool, sequence []string) []fantasy.AgentTool {
	if len(sequence) == 0 {
		return agentTools
	}
	allowed := make(map[string]bool, len(sequence))
	for _, name := range sequence {
		allowed[strings.TrimSpace(name)] = true
	}
	filtered := make([]fantasy.AgentTool, 0, len(agentTools))
	for _, tool := range agentTools {
		if tool != nil && allowed[strings.TrimSpace(tool.Info().Name)] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}
