package team

import (
	"encoding/base64"
	"reflect"
	"testing"

	"charm.land/fantasy"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
)

func TestToolMediaCompactionUsesExplicitFormatAndKeepsClosedPair(t *testing.T) {
	c, ctx, tool := toolMediaPublicationFixture(t, true)
	response, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, internalmcp.RuntimeToolResult{InvocationID: "mcp-test-call", Content: []internalmcp.RuntimeToolContent{
		{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	history := []fantasy.Message{historyToolCall("capture", `{}`), {Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "capture", Output: fantasy.ToolResultOutputContentText{Text: "captured"}, ClientMetadata: response.Metadata},
	}}}
	state := newCompactionState()
	state.Branches["main"] = ConversationCompactionCheckpoint{History: history}
	redacted, err := redactedCompactionState(state)
	if err != nil || redacted.Version != toolMediaCompactionStateVersion {
		t.Fatal("tool carrier did not select a new compaction format", err)
	}
	if err := validateCompactionMediaVersion(redacted); err != nil {
		t.Fatal(err)
	}
	redacted.Version = mediaCompactionStateVersion
	if err := validateCompactionMediaVersion(redacted); err == nil {
		t.Fatal("tool carrier was accepted by the previous media checkpoint format")
	}
	if _, _, ok := mediaTextCompactionRange(history); ok {
		t.Fatal("compaction detached a tool call from its image result")
	}
	messages := append(cloneMessages(history), fantasy.NewUserMessage("follow-up"), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "text observation"}}})
	start, end, ok := mediaTextCompactionRange(messages)
	if !ok || start != 2 || end != len(messages) {
		t.Fatal("text compaction did not preserve the complete media exchange")
	}
	projection := replaceMediaTextInterval(messages, start, end, []fantasy.Message{fantasy.NewUserMessage("text summary")})
	if validateToolPairs(projection) != nil || !reflect.DeepEqual(historyReferences(projection), historyReferences(history)) {
		t.Fatal("compaction lost pairing, order or media provenance")
	}
}
