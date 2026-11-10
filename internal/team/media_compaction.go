package team

import (
	"context"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

// Compact one contiguous text interval. Media-bearing messages remain at their
// original relative positions, keeping both image order and monotonic source
// ranges. A later request can compact another interval if it still cannot fit.
func (c *Coordinator) buildMediaCompactionProjection(ctx context.Context, messages []fantasy.Message, sourceOffset int, counts []int, predecessor *StructuredSummary, compacter SidecarCompacter) compactionProjection {
	if _, err := agent.ValidatePersistentMessages(messages); err != nil {
		return compactionProjection{messages: messages, err: err}
	}
	start, end, ok := mediaTextCompactionRange(messages)
	if !ok {
		return compactionProjection{messages: messages, err: media.Reject(media.LimitExceeded, "media references cannot be removed to fit context")}
	}
	counts = normalizeSourceCounts(len(messages), counts)
	offset := sourceOffset + sumSourceCounts(counts[:start])
	goal := c.initialPrompt
	if goal == "" && predecessor != nil {
		goal = predecessor.Goal
	}
	if goal == "" {
		goal = extractFirstUserMessageText(messages)
	}
	projection := c.buildTextCompactionProjection(ctx, messages[start:end], offset, counts[start:end], predecessor, compacter, goal)
	if projection.summary == nil {
		projection.messages = messages
		return projection
	}
	projection.messages = replaceMediaTextInterval(messages, start, end, projection.messages)
	projection.compactedStart, projection.compactedEnd = start, end
	projection.tokensBefore = countTokensInMessages(c.coordinatorModelID(), messages)
	projection.tokensAfter = countTokensInMessages(c.coordinatorModelID(), projection.messages)
	return projection
}

func mediaTextCompactionRange(messages []fantasy.Message) (int, int, bool) {
	bestStart, bestEnd, bestSize := 0, 0, -1
	for start := 0; start < len(messages); {
		if agent.MessagesHaveMedia(messages[start : start+1]) {
			start++
			continue
		}
		end, size := start, 0
		for end < len(messages) && !agent.MessagesHaveMedia(messages[end:end+1]) {
			size += messageTextSize(messages[end])
			end++
		}
		if size > bestSize && isToolPairBoundaryClean(messages, start) && isToolPairBoundaryClean(messages, end) {
			bestStart, bestEnd, bestSize = start, end, size
		}
		start = end
	}
	return bestStart, bestEnd, bestSize >= 0
}

func replaceMediaTextInterval(messages []fantasy.Message, start, end int, replacement []fantasy.Message) []fantasy.Message {
	result := make([]fantasy.Message, 0, len(messages)-(end-start)+len(replacement))
	result = append(result, messages[:start]...)
	result = append(result, replacement...)
	return append(result, messages[end:]...)
}

func projectionCompactedRanges(projection compactionProjection, original [][]CompactionRange) []CompactionRange {
	if projection.compactedEnd > 0 {
		return flattenSourceRanges(original[projection.compactedStart:projection.compactedEnd])
	}
	return flattenSourceRanges(original)
}

func projectionHistoryRanges(projection compactionProjection, original [][]CompactionRange) [][]CompactionRange {
	if projection.compactedEnd == 0 {
		return [][]CompactionRange{flattenSourceRanges(original)}
	}
	result := cloneSourceRanges(original[:projection.compactedStart])
	result = append(result, flattenSourceRanges(original[projection.compactedStart:projection.compactedEnd]))
	return append(result, cloneSourceRanges(original[projection.compactedEnd:])...)
}

func (c *Coordinator) buildMediaWorkerCompaction(ctx context.Context, messages []fantasy.Message, predecessor *StructuredSummary, goal string) ([]fantasy.Message, *StructuredSummary, error) {
	if _, err := agent.ValidatePersistentMessages(messages); err != nil {
		return nil, nil, err
	}
	start, end, ok := mediaTextCompactionRange(messages)
	if !ok {
		return nil, nil, media.Reject(media.LimitExceeded, "media references cannot be removed to fit worker context")
	}
	replacement, summary, err := c.buildTextWorkerCompactionProjection(ctx, messages[start:end], predecessor, goal)
	if err != nil {
		return nil, nil, err
	}
	return replaceMediaTextInterval(messages, start, end, replacement), summary, nil
}
