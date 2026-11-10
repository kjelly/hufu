package team

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
)

func TestToolMediaConsumerPreservesInitialInputAndAdmitsCompleteContinuation(t *testing.T) {
	for _, scenario := range []string{"initial images", "tool only"} {
		t.Run(scenario, func(t *testing.T) {
			testToolMediaConsumerContinuation(t, scenario == "tool only")
		})
	}
}

func testToolMediaConsumerContinuation(t *testing.T, withoutInput bool) {
	c, ctx, tool := toolMediaPublicationFixture(t, withoutInput)
	_, item, err := c.nativeProviderOccurrence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initial := item.MediaBinding.Clone()
	input := internalmcp.RuntimeToolResult{InvocationID: "mcp-test-call", Content: []internalmcp.RuntimeToolContent{
		{Type: "text", Text: "before"}, {Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"}, {Type: "text", Text: "after"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"},
	}}
	response, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, input)
	if err != nil {
		t.Fatal(err)
	}
	history := []fantasy.Message{fantasy.NewUserMessage(item.Goal)}
	if item.MediaBinding != nil {
		history, err = c.nativeWorkerMessages(ctx, item.ID, item.Goal, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	history = append(history, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "capture", ToolName: tool.Name, Input: `{}`}}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "capture", Output: fantasy.ToolResultOutputContentText{Text: response.Content}, ClientMetadata: response.Metadata}}})
	before, err := agent.EncodeMediaHistory(history, "tool-continuation")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMediaHistorySources(ctx, c.session.Workspace, c.activeBranchID(), history); err != nil {
		t.Fatal("canonical tool carrier failed source validation", err)
	}
	messages, err := agent.ExpandToolMediaMessages(history)
	if err != nil {
		t.Fatal(err)
	}
	request := nativeMediaProviderRequest(t, c, messages, 65536)
	request.InvocationID = "tool-consumer"
	admission := providerRequestAdmission{c: c}
	if err := admission.AdmitProviderRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := admission.CommitProviderInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	wire, release, err := admission.CompileProviderRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rawRequest := request
	rawCall := *request.Call
	rawCall.Prompt = history
	rawRequest.Call = &rawCall
	rawWire, rawRelease, err := admission.CompileProviderRequest(ctx, rawRequest)
	if err != nil {
		t.Fatal("compiler could not normalize the same admitted carrier projection", err)
	}
	if len(rawWire.Call.Prompt) != len(wire.Call.Prompt) {
		rawRelease()
		t.Fatal("raw carrier and expanded request compiled into different shapes")
	}
	rawRelease()
	count := 0
	for _, message := range wire.Call.Prompt {
		for _, part := range message.Content {
			if file, ok := part.(fantasy.FilePart); ok {
				count++
				if !bytes.Equal(file.Data, mediaStorePNG(t)) {
					t.Fatal("consumer compiler substituted published image bytes")
				}
			}
		}
	}
	expected := 4
	if withoutInput {
		expected = 2
	}
	if count != expected || !reflect.DeepEqual(initial, c.todoItemByID(item.ID).MediaBinding) {
		t.Fatal("consumer lost an occurrence or rewrote the immutable initial binding")
	}
	if err := admission.AuthorizeProviderTransport(ctx, request); err != nil {
		t.Fatal(err)
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, view.events); err != nil {
		t.Fatal("independent consumer audit failed", err)
	}
	if view.events[len(view.events)-2].Type != string(EventMediaConsumerBound) || view.events[len(view.events)-1].Type != string(EventMediaProviderAdmitted) {
		t.Fatal("consumer and provider permission were not atomically committed")
	}
	after, err := agent.EncodeMediaHistory(history, "tool-continuation")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("provider projection changed canonical tool history", err)
	}
	for _, scenario := range []string{"reordered", "foreign task", "unbound hash", "missing result"} {
		t.Run(scenario, func(t *testing.T) {
			changed := request
			call := *request.Call
			call.Prompt = cloneMessages(request.Call.Prompt)
			changed.Call = &call
			locations := historyReferences(call.Prompt)
			last := locations[len(locations)-1]
			ref := last.Reference
			switch scenario {
			case "reordered":
				first := locations[len(locations)-2]
				call.Prompt[last.Message].Content[last.Part], call.Prompt[first.Message].Content[first.Part] = call.Prompt[first.Message].Content[first.Part], call.Prompt[last.Message].Content[last.Part]
			case "foreign task":
				ref.TaskID = "other-task"
				call.Prompt[last.Message].Content[last.Part] = agent.MediaReferencePart{Reference: ref}
			case "unbound hash":
				ref.BindingHash = ref.Source.SnapshotHash
				call.Prompt[last.Message].Content[last.Part] = agent.MediaReferencePart{Reference: ref}
			case "missing result":
				call.Prompt = append(call.Prompt[:2], call.Prompt[3:]...)
			}
			if err := admission.AdmitProviderRequest(ctx, changed); err == nil {
				t.Fatal("invalid tool continuation reached provider admission")
			}
		})
	}
	if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "revoked", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := admission.AuthorizeProviderTransport(ctx, request); err == nil {
		t.Fatal("revoked consumer lease reached HTTP transport")
	}
}
