package team

import (
	"fmt"
	"strings"
)

// validateTaskOutput checks transport form only. Completion is established by
// submitted results and verification, never by the language of the final text.
func validateTaskOutput(task TaskDef, output string) error {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return withFailureClassOverride(fmt.Errorf("task returned empty output: the agent finished without a final message. Results must be summarized in the final message even when every command succeeded or produced no stdout"), FailureProtocol)
	}

	return nil
}
