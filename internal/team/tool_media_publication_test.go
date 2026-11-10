package team

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func toolMediaPublicationFixture(t *testing.T, withoutInput ...bool) (*Coordinator, context.Context, internalmcp.MCPTool) {
	t.Helper()
	c := newMediaPolicyCoordinatorWithTransport(t, "http://fixture.invalid/v1", nil, providerintrospection.CapabilityUnknown, nil)
	ctx := openMediaInvocation(t, c)
	_, descriptors, authorization := gatewayProjectionFixture(t, 1)
	task := TaskDef{Agent: "worker", Goal: "capture tool image"}
	if len(withoutInput) == 0 || !withoutInput[0] {
		task = nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
	}
	spec, id := prepareMediaOccurrence(t, c, ctx, task, func(spec *TodoSpec) { spec.DynamicToolAuthorization = authorization })
	if _, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(ctx, id, TaskPending, TaskInProgress, "running", "", nil); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(id, 1)
	identity, found := c.activeTaskResultOccurrence(id)
	if !found {
		t.Fatal("missing publisher lease")
	}
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: id, AgentName: "worker", AgentRole: "worker", Attempt: 1, Purpose: "task_execution"})
	return c, ctx, descriptors[0]
}

func TestToolMediaPublicationPreservesOrderedSourcesAndCanonicalEvidence(t *testing.T) {
	c, ctx, tool := toolMediaPublicationFixture(t)
	encoded := base64.StdEncoding.EncodeToString(mediaStorePNG(t))
	input := internalmcp.RuntimeToolResult{InvocationID: "mcp-test-call", StructuredContent: []byte(`{"complete":true}`), Content: []internalmcp.RuntimeToolContent{
		{Type: "text", Text: "before"}, {Type: "image", Data: encoded, MIMEType: "image/png"}, {Type: "text", Text: "after"}, {Type: "image", Data: encoded, MIMEType: "image/png"},
	}}
	response, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, input)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := agent.DecodeToolMediaMetadata(response.Metadata)
	if err != nil || metadata == nil || len(metadata.Parts) != 4 || metadata.Parts[0].Text != "before" || metadata.Parts[2].Text != "after" || metadata.Parts[1].Image.Ref != metadata.Parts[3].Image.Ref {
		t.Fatal("publication lost ordered text or repeated image identity", err)
	}
	if metadata.Parts[1].Image.Source.PartIndex != 1 || metadata.Parts[3].Image.Source.PartIndex != 3 || metadata.Parts[1].Image.TaskID == "" || response.Content != "before\nafter" || response.Data != nil {
		t.Fatal("publication lost occurrence scope or exposed raw bytes")
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := view.events[len(view.events)-1]
	if last.Type != string(EventToolMediaPublished) || view.events[len(view.events)-2].Type != string(EventMediaInputAdmitted) {
		t.Fatal("publisher did not atomically bind source and owner")
	}
	published, err := decodeToolMediaPublication(last)
	if err != nil || len(published.Snapshot.Refs) != 1 || len(published.Snapshot.Occurrences) != 2 {
		t.Fatal("producer publication lost CAS deduplication or source occurrences", err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, view.events); err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, view.events[:len(view.events)-1]); err == nil {
		t.Fatal("independent audit accepted an orphan tool snapshot")
	}
	if _, err := c.admitMediaInput(ctx, published.Snapshot); err == nil {
		t.Fatal("input-only API bypassed atomic tool provenance publication")
	}
	carrier := *metadata
	carrier.Parts = append([]agent.ToolMediaPart(nil), metadata.Parts...)
	carrier.Parts[0].Text = "forged caption"
	forged, err := agent.EncodeToolMediaMetadata(carrier)
	if err != nil {
		t.Fatal(err)
	}
	history := []fantasy.Message{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "capture", Output: fantasy.ToolResultOutputContentText{Text: "before\nafter"}, ClientMetadata: forged}}}}
	if err := validateMediaHistorySources(ctx, c.session.Workspace, c.activeBranchID(), history); err == nil {
		t.Fatal("history accepted forged text on an otherwise valid image producer")
	}
	for _, scenario := range []string{"missing snapshot", "wrong descriptor", "wrong owner", "wrong hash"} {
		t.Run(scenario, func(t *testing.T) {
			p, event, prior := published, last, view.events[:len(view.events)-1]
			switch scenario {
			case "missing snapshot":
				prior = prior[:len(prior)-1]
			case "wrong descriptor":
				p.DescriptorSHA256 = string(make([]byte, 64))
			case "wrong owner":
				event.TaskID = "other-task"
			case "wrong hash":
				p.Hash = "invalid"
			}
			event.Payload, err = json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			decoded, decodeErr := decodeToolMediaPublication(event)
			if decodeErr == nil && verifyToolMediaPublication(event, decoded, prior) == nil {
				t.Fatal("invalid producer evidence was accepted")
			}
		})
	}
	if string(input.StructuredContent) != `{"complete":true}` || input.Content[1].Data != encoded {
		t.Fatal("publisher rewrote raw tool evidence")
	}
	before := view.inputs
	if _, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, input); err != nil {
		t.Fatal("identical completed publication was not idempotent", err)
	}
	view, err = c.mediaLineage(ctx)
	if err != nil || !reflect.DeepEqual(before, view.inputs) {
		t.Fatal("idempotent publication consumed branch quota again", err)
	}
}

func TestToolMediaPublicationPreservesEmptyTextBlocks(t *testing.T) {
	c, ctx, tool := toolMediaPublicationFixture(t, true)
	response, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, internalmcp.RuntimeToolResult{InvocationID: "mcp-empty-text", Content: []internalmcp.RuntimeToolContent{
		{Type: "text"}, {Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"}, {Type: "text"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := agent.DecodeToolMediaMetadata(response.Metadata)
	if err != nil || carrier == nil || len(carrier.Parts) != 3 || carrier.Parts[0].Type != "text" || carrier.Parts[2].Type != "text" || carrier.Parts[1].Image.Source.PartIndex != 1 {
		t.Fatal("empty text changed the ordered MCP result", err)
	}
	view, err := c.mediaLineage(ctx)
	if err != nil || VerifyNativeMediaLineage(ctx, c.session.Workspace, view.events) != nil {
		t.Fatal("empty tool text failed canonical replay", err)
	}
}

func TestToolMediaPublicationRejectsUnusableContentBeforeAdmission(t *testing.T) {
	for _, scenario := range []string{"error", "invalid base64", "wrong MIME", "unbound", "unknown block", "empty call"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, tool := toolMediaPublicationFixture(t)
			input := internalmcp.RuntimeToolResult{InvocationID: "mcp-test-call", Content: []internalmcp.RuntimeToolContent{{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"}}}
			call := fantasy.ToolCall{ID: "capture"}
			switch scenario {
			case "error":
				input.IsError = true
			case "invalid base64":
				input.Content[0].Data = "invalid"
			case "wrong MIME":
				input.Content[0].MIMEType = "image/jpeg"
			case "unbound":
				ctx = t.Context()
			case "unknown block":
				input.Content[0].Type = "resource"
			case "empty call":
				call.ID = ""
			}
			if _, err := c.publishToolMediaResult(ctx, tool, call, input); err == nil {
				t.Fatal("invalid content became an admitted image")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == string(EventToolMediaPublished) {
					t.Fatal("invalid publication left usable producer evidence")
				}
			}
		})
	}
}
