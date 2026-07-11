package team

import (
	"fmt"
	"strings"
)

// unfinishedOutputPrefixes flag final messages that read as a narration of
// work still to come rather than a result.
var unfinishedOutputPrefixes = []string{
	"let me ",
	"now let me ",
	"i'll ",
	"i will ",
}

// unfinishedOutputMaxRunes bounds the unfinished-prefix heuristic: a long
// report may legitimately open with "Let me walk through..." while a short
// one starting that way is almost always a truncated progress update.
const unfinishedOutputMaxRunes = 400

// validateTaskOutput rejects final outputs the coordinator cannot act on.
// It checks form only (empty, unfinished narration); judging content quality
// is the job of task.Verify commands and adversarial verification, not
// hardcoded heuristics about specific task types.
func validateTaskOutput(task TaskDef, output string) error {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return fmt.Errorf("task returned empty output: the agent finished without a final message. Results must be summarized in the final message even when every command succeeded or produced no stdout")
	}

	if len([]rune(trimmed)) <= unfinishedOutputMaxRunes {
		lowerOutput := strings.ToLower(trimmed)
		for _, prefix := range unfinishedOutputPrefixes {
			if strings.HasPrefix(lowerOutput, prefix) {
				return fmt.Errorf("task returned an unfinished progress update instead of a final result")
			}
		}
	}

	return nil
}
