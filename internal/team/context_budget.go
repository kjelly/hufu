package team

// Token-budget and byte-budget shaping of conversation messages. Two callers:
//   - capStepMessages / CapStepMessagesWithCounter: per-request shaping in PrepareStep,
//     so a long agent stream cannot grow its prompt beyond the model's token context limit.
//   - truncateOversizedMessage: cross-turn history persistence, replacing the
//     old behavior of silently dropping any message over maxMessageSize.
// Messages are only ever truncated in place, never removed, so tool_call /
// tool_result pairing stays intact.

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"charm.land/fantasy"
)

const (
	// defaultStepContextBudgetTokens is the fallback message token budget when no model spec is supplied.
	defaultStepContextBudgetTokens = 30_000
	// squeezedPartCapChars is the default size an old part is reduced to when the budget is exceeded.
	squeezedPartCapChars = 1_500
	// recentMessagesProtected is how many trailing messages are never squeezed.
	recentMessagesProtected = 10
	// headMessagesProtected covers system and goal prompts (index 0 and 1).
	headMessagesProtected = 2
	// verifiedHistoryPrefix is a durable marker on a message produced only by
	// compactMessages after ValidatedCompactor has accepted its evidence.
	verifiedHistoryPrefix = "[Verified Compacted History]\n"
)

// isVerifiedHistoryMessage identifies evidence already checked for required
// IDs, anchors and verification polarity. It must never pass through generic
// head/tail squeezing, which would invalidate those checks.
func isVerifiedHistoryMessage(msg fantasy.Message) bool {
	for _, part := range msg.Content {
		if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && strings.HasPrefix(text.Text, verifiedHistoryPrefix) {
			return true
		}
	}
	return false
}

// messageTextSize measures every text-bearing part of a message in characters.
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

// squeezeText reduces s to at most capChars runes, keeping head and tail around an elision marker.
func squeezeText(s string, capChars int) string {
	runes := []rune(s)
	if len(runes) <= capChars {
		return s
	}
	marker := fmt.Sprintf("\n…[truncated %d chars]…\n", len(runes)-capChars)
	head := capChars * 2 / 3
	tail := capChars - head
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

// compactToolResultText is intentionally different from generic text
// squeezing: command output often has its only actionable error in the
// middle. Preserve every diagnostic-looking line before adding a small head
// and tail excerpt, so a budget boundary cannot turn a failed command into an
// apparently clean one.
func compactToolResultText(s string, capChars int) string {
	if len([]rune(s)) <= capChars {
		return s
	}
	lines := strings.Split(s, "\n")
	critical := make([]string, 0)
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") || strings.Contains(lower, "exit status") || strings.Contains(lower, "exit code") || strings.Contains(lower, "warning") {
			critical = append(critical, line)
		}
	}
	// A pathological output can contain thousands of matching lines. Keep a
	// deterministic subset while making the omitted count explicit.
	criticalText := strings.Join(critical, "\n")
	if len([]rune(criticalText)) >= capChars/2 {
		criticalText = squeezeText(criticalText, capChars/2)
	}
	remaining := capChars - len([]rune(criticalText)) - 64
	if remaining < 100 {
		remaining = 100
	}
	excerpt := squeezeText(s, remaining)
	if criticalText == "" {
		return excerpt
	}
	return "[preserved diagnostics]\n" + criticalText + "\n[output excerpt]\n" + excerpt
}

// squeezeMessage returns a copy of msg with every text-bearing part reduced to at most capChars.
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
					p.Output = fantasy.ToolResultOutputContentText{Text: compactToolResultText(txt, capChars)}
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

// StepMessageCapResult makes the shaping outcome explicit. In particular,
// callers must not mistake a non-nil shaped slice for proof that the request
// fits: protected recent messages can keep the result over budget.
type StepMessageCapResult struct {
	Messages        []fantasy.Message
	Changed         bool
	StillOverBudget bool
	Tokens          int
}

// CapStepMessagesWithCounterResult shapes messages and performs a final full
// recount. It never claims success when the final count is still over budget.
func CapStepMessagesWithCounterResult(ctx context.Context, counter TokenCounter, modelID string, msgs []fantasy.Message, maxTokens int) StepMessageCapResult {
	if len(msgs) == 0 {
		return StepMessageCapResult{}
	}
	if counter == nil {
		counter = defaultCounter
	}
	if maxTokens <= 0 {
		spec := globalRegistry.GetSpec(modelID)
		budget := CalculateContextBudget(spec, 0, 0)
		maxTokens = budget.Available
	}
	if maxTokens <= 0 {
		maxTokens = defaultStepContextBudgetTokens
	}

	totalTokens, err := counter.CountMessages(ctx, modelID, msgs)
	if err != nil {
		return StepMessageCapResult{StillOverBudget: true}
	}
	if totalTokens <= maxTokens {
		return StepMessageCapResult{Tokens: totalTokens}
	}

	out := make([]fantasy.Message, len(msgs))
	copy(out, msgs)
	protectFrom := len(out) - recentMessagesProtected

	// First pass: squeeze oversized parts to squeezedPartCapChars.
	for i := headMessagesProtected; i < len(out) && totalTokens > maxTokens; i++ {
		if i >= protectFrom {
			break
		}
		if isVerifiedHistoryMessage(out[i]) {
			continue
		}
		before, beforeErr := counter.CountMessages(ctx, modelID, []fantasy.Message{out[i]})
		if beforeErr != nil {
			return StepMessageCapResult{Messages: out, Changed: !reflect.DeepEqual(out, msgs), StillOverBudget: true}
		}
		if squeezed, changed := squeezeMessage(out[i], squeezedPartCapChars); changed {
			out[i] = squeezed
			after, afterErr := counter.CountMessages(ctx, modelID, []fantasy.Message{out[i]})
			if afterErr != nil {
				return StepMessageCapResult{Messages: out, Changed: true, StillOverBudget: true}
			}
			totalTokens += after - before
		}
	}

	// Second pass: aggressively shrink older non-protected messages.
	capChars := squeezedPartCapChars / 2
	for i := headMessagesProtected; i < len(out) && totalTokens > maxTokens && capChars >= 100; i++ {
		if i >= protectFrom {
			break
		}
		if isVerifiedHistoryMessage(out[i]) {
			continue
		}
		before, beforeErr := counter.CountMessages(ctx, modelID, []fantasy.Message{out[i]})
		if beforeErr != nil {
			return StepMessageCapResult{Messages: out, Changed: !reflect.DeepEqual(out, msgs), StillOverBudget: true}
		}
		if squeezed, changed := squeezeMessage(out[i], capChars); changed {
			out[i] = squeezed
			after, afterErr := counter.CountMessages(ctx, modelID, []fantasy.Message{out[i]})
			if afterErr != nil {
				return StepMessageCapResult{Messages: out, Changed: true, StillOverBudget: true}
			}
			totalTokens += after - before
		}
	}

	finalTokens, finalErr := counter.CountMessages(ctx, modelID, out)
	if finalErr != nil {
		return StepMessageCapResult{Messages: out, Changed: true, StillOverBudget: true}
	}
	return StepMessageCapResult{
		Messages:        out,
		Changed:         !reflect.DeepEqual(out, msgs),
		StillOverBudget: finalTokens > maxTokens,
		Tokens:          finalTokens,
	}
}

// CapStepMessagesWithCounter shapes messages using a TokenCounter and model context token budget.
func CapStepMessagesWithCounter(ctx context.Context, counter TokenCounter, modelID string, msgs []fantasy.Message, maxTokens int) []fantasy.Message {
	result := CapStepMessagesWithCounterResult(ctx, counter, modelID, msgs, maxTokens)
	if result.Messages == nil || (!result.Changed && !result.StillOverBudget) {
		return nil
	}
	return result.Messages
}

// capStepMessages is a legacy wrapper for CapStepMessagesWithCounter using default token budget.
func capStepMessages(msgs []fantasy.Message) []fantasy.Message {
	return CapStepMessagesWithCounter(context.Background(), defaultCounter, "default", msgs, defaultStepContextBudgetTokens)
}

// truncateOversizedMessage shrinks a message exceeding maxSize.
func truncateOversizedMessage(msg fantasy.Message, maxSize int) fantasy.Message {
	if isVerifiedHistoryMessage(msg) {
		return msg
	}
	size := messageTextSize(msg)
	if size <= maxSize {
		return msg
	}
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
