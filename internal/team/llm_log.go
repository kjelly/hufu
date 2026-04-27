package team

import (
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

func formatMessagePart(part fantasy.MessagePart) string {
	switch part.GetType() {
	case fantasy.ContentTypeText:
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			return tp.Text
		}
	case fantasy.ContentTypeReasoning:
		if rp, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
			return "<reasoning>" + rp.Text + "</reasoning>"
		}
	case fantasy.ContentTypeToolCall:
		if tcp, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
			start := "<tool_call name=" + fmt.Sprintf("%q", tcp.ToolName) + " id=" + fmt.Sprintf("%q", tcp.ToolCallID) + ">"
			end := "</tool_call>"
			return start + tcp.Input + end
		}
	case fantasy.ContentTypeToolResult:
		if trp, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
			var out string
			if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](trp.Output); ok {
				out = txt.Text
			}
			start := "<tool_result id=" + fmt.Sprintf("%q", trp.ToolCallID) + ">"
			end := "</tool_result>"
			return start + out + end
		}
	default:
		return "<" + string(part.GetType()) + "/>"
	}
	return ""
}

func llmLogRequest(logWrite func(string), opts fantasy.PrepareStepFunctionOptions) {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[%s] === REQUEST step=%d model=%s ===\n",
		time.Now().Format(time.RFC3339), opts.StepNumber, opts.Model.Model()))
	for _, msg := range opts.Messages {
		b.WriteString(fmt.Sprintf("[%s] ", msg.Role))
		for _, part := range msg.Content {
			b.WriteString(formatMessagePart(part))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	logWrite(b.String())
}

func llmLogStreamEvent(logWrite func(string), eventType, content string) {
	logWrite(fmt.Sprintf("[%s] <%s>%s</%s>\n",
		time.Now().Format(time.RFC3339), eventType, content, eventType))
}

func llmLogStreamFinish(logWrite func(string), finishReason fantasy.FinishReason, usage fantasy.Usage) {
	logWrite(fmt.Sprintf("[%s] === RESPONSE finish_reason=%s tokens_in=%d tokens_out=%d ===\n\n",
		time.Now().Format(time.RFC3339), finishReason, usage.InputTokens, usage.OutputTokens))
}

func formatToolCallContent(tc fantasy.ToolCallContent) string {
	return "<tool_call name=" + fmt.Sprintf("%q", tc.ToolName) + " id=" + fmt.Sprintf("%q", tc.ToolCallID) + ">" + tc.Input + "</tool_call>"
}

func formatToolResultContent(tr fantasy.ToolResultContent) string {
	var out string
	if tr.Result != nil {
		if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
			out = txt.Text
		}
	}
	return "<tool_result name=" + fmt.Sprintf("%q", tr.ToolName) + " id=" + fmt.Sprintf("%q", tr.ToolCallID) + ">" + out + "</tool_result>"
}
