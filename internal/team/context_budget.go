package team

// Byte-budget shaping of conversation messages. Two callers:
//   - capStepMessages: per-request shaping in PrepareStep, so a long agent
//     stream cannot grow its own prompt without bound (observed 189KB
//     coordinator requests re-sending every old tool result verbatim).
//   - truncateOversizedMessage: cross-turn history persistence, replacing the
//     old behavior of silently dropping any message over maxMessageSize —
//     which erased fetched file contents and provoked re-fetch loops.
// Messages are only ever truncated in place, never removed, so tool_call /
// tool_result pairing stays intact.

import (
	"fmt"

	"charm.land/fantasy"
)

const (
	// stepContextBudgetChars caps the total text content sent per LLM request
	// (system prompt excluded). Roughly 30K tokens.
	stepContextBudgetChars = 120_000
	// squeezedPartCapChars is the size an old part is reduced to when the
	// budget is exceeded.
	squeezedPartCapChars = 1_500
	// recentMessagesProtected is how many trailing messages are never
	// squeezed — the model needs its most recent tool results verbatim.
	recentMessagesProtected = 10
)

// messageTextSize measures every text-bearing part of a message. The previous
// estimator counted only TextPart, so a message holding a 100KB tool result
// measured as size 0.
func messageTextSize(msg fantasy.Message) int {
	total := 0
	for _, part := range msg.Content {
		switch part.GetType() {
		case fantasy.ContentTypeText:
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				total += len(p.Text)
			}
		case fantasy.ContentTypeReasoning:
			if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
				total += len(p.Text)
			}
		case fantasy.ContentTypeToolCall:
			if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				total += len(p.Input)
			}
		case fantasy.ContentTypeToolResult:
			if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				txt, _ := toolResultOutputText(p.Output)
				total += len(txt)
			}
		}
	}
	return total
}

// squeezeText reduces s to at most capChars runes, keeping the head and tail
// around an elision marker so both the beginning and the conclusion survive.
func squeezeText(s string, capChars int) string {
	runes := []rune(s)
	if len(runes) <= capChars {
		return s
	}
	marker := fmt.Sprintf("\n…[truncated %d chars]…\n", len(runes)-capChars)
	head := capChars * 2 / 3
	tail := capChars - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

// squeezeMessage returns a copy of msg with every text-bearing part reduced
// to at most capChars. Returns msg unchanged (and false) when nothing needed
// squeezing.
func squeezeMessage(msg fantasy.Message, capChars int) (fantasy.Message, bool) {
	changed := false
	parts := make([]fantasy.MessagePart, len(msg.Content))
	copy(parts, msg.Content)
	for i, part := range parts {
		switch part.GetType() {
		case fantasy.ContentTypeText:
			if p, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && len([]rune(p.Text)) > capChars {
				p.Text = squeezeText(p.Text, capChars)
				parts[i] = p
				changed = true
			}
		case fantasy.ContentTypeReasoning:
			if p, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok && len([]rune(p.Text)) > capChars {
				p.Text = squeezeText(p.Text, capChars)
				parts[i] = p
				changed = true
			}
		case fantasy.ContentTypeToolResult:
			if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				txt, isErr := toolResultOutputText(p.Output)
				if len([]rune(txt)) > capChars && !isErr {
					p.Output = fantasy.ToolResultOutputContentText{Text: squeezeText(txt, capChars)}
					parts[i] = p
					changed = true
				}
			}
		}
	}
	if !changed {
		return msg, false
	}
	msg.Content = parts
	return msg, true
}

// headMessagesProtected covers the system prompt (index 0 when present) and
// the task/goal prompt — squeezing either cripples the whole run.
const headMessagesProtected = 2

// capStepMessages shapes the per-request message list to the byte budget.
// It walks from the oldest message forward, squeezing each one until the
// total fits, while protecting the leading system/goal messages and the most
// recent recentMessagesProtected messages. Returns nil when the input
// already fits, signalling the caller to send the original slice.
func capStepMessages(msgs []fantasy.Message) []fantasy.Message {
	total := 0
	for _, m := range msgs {
		total += messageTextSize(m)
	}
	if total <= stepContextBudgetChars {
		return nil
	}

	out := make([]fantasy.Message, len(msgs))
	copy(out, msgs)
	protectFrom := len(out) - recentMessagesProtected
	for i := headMessagesProtected; i < len(out) && total > stepContextBudgetChars; i++ {
		if i >= protectFrom {
			break
		}
		before := messageTextSize(out[i])
		if squeezed, changed := squeezeMessage(out[i], squeezedPartCapChars); changed {
			out[i] = squeezed
			total += messageTextSize(squeezed) - before
		}
	}
	return out
}

// truncateOversizedMessage shrinks a message that exceeds maxSize down to
// roughly that size instead of dropping it from history. Dropping erased
// fetched file contents from the coordinator's memory, so it re-delegated
// the same read over and over.
func truncateOversizedMessage(msg fantasy.Message, maxSize int) fantasy.Message {
	size := messageTextSize(msg)
	if size <= maxSize {
		return msg
	}
	// Distribute the budget across text-bearing parts proportionally; a
	// simple per-part cap is enough since messages rarely have many parts.
	// The 64-char headroom covers the elision marker squeezeText inserts.
	perPart := maxSize
	if n := len(msg.Content); n > 1 {
		perPart = maxSize / n
	}
	perPart -= 64
	if perPart < 200 {
		perPart = 200
	}
	squeezed, _ := squeezeMessage(msg, perPart)
	return squeezed
}
