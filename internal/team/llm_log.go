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
			out, isErr := toolResultOutputText(trp.Output)
			attrs := " id=" + fmt.Sprintf("%q", trp.ToolCallID)
			if isErr {
				attrs += ` error="true"`
			}
			return "<tool_result" + attrs + ">" + out + "</tool_result>"
		}
	default:
		return "<" + string(part.GetType()) + "/>"
	}
	return ""
}

// llmLogRequest logs one REQUEST entry, printing only the messages not yet
// logged for this stream. Full per-step dumps made the log O(N²): a 244-step
// task repeated the identical system prompt 244 times (27% of a 6.5MB file).
// msgs is the message slice actually sent (post-compaction); logged is how
// many messages this stream has already logged. Returns the new
// logged-message count and the approximate request size in bytes.
func llmLogRequest(logWrite func(string), opts fantasy.PrepareStepFunctionOptions, msgs []fantasy.Message, logged int) (int, int) {
	reqBytes := 0
	for _, msg := range msgs {
		reqBytes += messageTextSize(msg)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] === REQUEST step=%d model=%s messages=%d bytes≈%d ===\n",
		time.Now().Format(time.RFC3339), opts.StepNumber, opts.Model.Model(), len(msgs), reqBytes)
	start := logged
	if len(msgs) < logged {
		// History was compacted/rewritten; earlier offsets no longer line up.
		start = 0
		b.WriteString("[history rewritten by compaction; full dump follows]\n")
	} else if logged > 0 {
		fmt.Fprintf(&b, "[... %d earlier message(s) unchanged — see previous REQUEST entries ...]\n", logged)
	}
	for _, msg := range msgs[start:] {
		fmt.Fprintf(&b, "[%s] ", msg.Role)
		for _, part := range msg.Content {
			b.WriteString(formatMessagePart(part))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	logWrite(b.String())
	return len(msgs), reqBytes
}

func llmLogStreamEvent(logWrite func(string), eventType, content string) {
	logWrite(fmt.Sprintf("[%s] <%s>%s</%s>\n",
		time.Now().Format(time.RFC3339), eventType, content, eventType))
}

func llmLogStreamFinish(logWrite func(string), finishReason fantasy.FinishReason, usage fantasy.Usage, reqBytes int) {
	line := fmt.Sprintf("[%s] === RESPONSE finish_reason=%s tokens_in=%d tokens_out=%d",
		time.Now().Format(time.RFC3339), finishReason, usage.InputTokens, usage.OutputTokens)
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && reqBytes > 0 {
		// Some providers report no usage at all (observed with minimax via
		// ollama); log the same bytes/4 estimate the token budget uses so the
		// log is not blind for cost analysis.
		line += fmt.Sprintf(" est_tokens_in≈%d (provider reported no usage; bytes/4 estimate)", reqBytes/4)
	}
	logWrite(line + " ===\n\n")
}

func formatToolCallContent(tc fantasy.ToolCallContent) string {
	return "<tool_call name=" + fmt.Sprintf("%q", tc.ToolName) + " id=" + fmt.Sprintf("%q", tc.ToolCallID) + ">" + tc.Input + "</tool_call>"
}

func formatToolResultContent(tr fantasy.ToolResultContent) string {
	var out string
	var isErr bool
	if tr.Result != nil {
		out, isErr = toolResultOutputText(tr.Result)
	}
	attrs := " name=" + fmt.Sprintf("%q", tr.ToolName) + " id=" + fmt.Sprintf("%q", tr.ToolCallID)
	if isErr {
		attrs += ` error="true"`
	}
	return "<tool_result" + attrs + ">" + out + "</tool_result>"
}

// toolResultOutputText extracts the human-readable text of a tool result
// output, covering both text and error outputs. Error outputs previously
// rendered as empty strings in every log view (LLM request dump, stream
// events, audit), hiding denial and failure messages from all diagnostics.
func toolResultOutputText(output fantasy.ToolResultOutputContent) (string, bool) {
	if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](output); ok {
		return txt.Text, false
	}
	if errOut, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](output); ok {
		if errOut.Error != nil {
			return errOut.Error.Error(), true
		}
		return "", true
	}
	return "", false
}
