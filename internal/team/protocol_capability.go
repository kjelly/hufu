package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// protocolAttemptWasReadOnly is deliberately conservative. It is used only
// after a worker omitted submit_result to decide whether a single clean model
// retry could replay the task without repeating a side effect. Unknown tools,
// missing calls, and malformed bash input are all unsafe.
func protocolAttemptWasReadOnly(steps []fantasy.StepResult) bool {
	sawCall := false
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			sawCall = true
			switch strings.TrimSpace(call.ToolName) {
			case "view", "grep", "glob", "ls", "math", "random":
				continue
			case "bash":
				var input struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal([]byte(call.Input), &input); err != nil || !tools.IsReadOnlyBashCommand(input.Command) {
					return false
				}
			default:
				return false
			}
		}
	}
	return sawCall
}

// protocolRepairEvidenceSummary avoids carrying old fantasy tool messages into
// a result-only repair turn. Those messages can bias a tool-calling model
// toward tools that are intentionally absent from the repair agent.
func protocolRepairEvidenceSummary(steps []fantasy.StepResult, transcript *ArtifactRef) string {
	var names []string
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			if name := strings.TrimSpace(call.ToolName); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 && transcript == nil {
		return "\n\n## Execution Evidence\nNo worker tool calls were recorded. Do not infer that work completed; submit a truthful blocked or partial result.\n"
	}
	var b strings.Builder
	b.WriteString("\n\n## Execution Evidence\n")
	if len(names) > 0 {
		b.WriteString("The prior worker used these tools: ")
		b.WriteString(strings.Join(names, ", "))
		b.WriteString(". Their raw calls are sealed evidence, not tools available in this repair turn.\n")
	}
	if transcript != nil {
		fmt.Fprintf(&b, "Transcript evidence reference: %s.\n", transcript.ID)
	}
	b.WriteString("Only submit_result is available now.\n")
	return b.String()
}
