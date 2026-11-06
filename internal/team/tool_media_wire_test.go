package team

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type toolMediaWireRequest struct {
	Model string `json:"model"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []struct {
		Role       string         `json:"role"`
		Content    jsontext.Value `json:"content"`
		ToolCallID string         `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
}

func TestMCPImagesReachNativeWorkerThroughActualSDK(t *testing.T) {
	for _, scenario := range []string{"gateway", "direct", "image only", "multiple calls", "recycled IDs", "result repair", "fanout", "fanout branch", "invalid base64", "wrong MIME", "error image", "no vision", "egress denied", "count limit"} {
		t.Run(scenario, func(t *testing.T) { testMCPImageWire(t, scenario) })
	}
}

func testMCPImageWire(t *testing.T, scenario string) {
	fanout := scenario == "fanout" || scenario == "fanout branch"
	image := mediaStorePNG(t)
	encoded := base64.StdEncoding.EncodeToString(image)
	fake := newMCPActionFake(collectDebugTool("Capture two ordered images."))
	fake.setHandler(func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		data := encoded
		if scenario == "invalid base64" {
			data = "invalid"
		}
		mime := "image/png"
		if scenario == "wrong MIME" {
			mime = "image/jpeg"
		}
		content := []mcpgo.Content{mcpgo.NewTextContent("before"), mcpgo.NewImageContent(data, mime), mcpgo.NewTextContent("after"), mcpgo.NewImageContent(data, mime)}
		result := &mcpgo.CallToolResult{IsError: scenario == "error image", StructuredContent: map[string]any{"complete": true}, Content: content}
		if scenario == "image only" {
			result.Content, result.StructuredContent = []mcpgo.Content{content[1], content[3]}, nil
		}
		return result, nil
	})
	var mcpConfig internalmcp.MCPServerConfig
	manager := newHTTPMCPActionManager(t, fake, func(config *internalmcp.MCPServerConfig) {
		config.ToolPolicies = map[string]internalmcp.WorkerToolPolicy{"collect_debug": workerObservationPolicy()}
		mcpConfig = *config
	})
	var calls atomic.Int32
	var modelCalls sync.Map
	requests := make(chan toolMediaWireRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request toolMediaWireRequest
		if r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			http.Error(w, "unexpected extra request", http.StatusBadRequest)
			return
		}
		name, arguments, id := "submit_result", `{"status":"success","summary":"Compared the published images.","details":"Two ordered tool images were analyzed."}`, "submit"
		callNumber := calls.Add(1)
		first := callNumber == 1
		if fanout {
			counter, _ := modelCalls.LoadOrStore(request.Model, new(atomic.Int32))
			first = counter.(*atomic.Int32).Add(1) == 1
		}
		if first || scenario == "recycled IDs" && callNumber == 2 {
			name, arguments, id = dynamicToolGatewayName, `{"action":"call","target":"diagnostics__collect_debug","arguments":{"service":"public"}}`, "capture"
			if scenario == "direct" {
				name, arguments = "diagnostics__collect_debug", `{"service":"public"}`
			}
		}
		if scenario == "result repair" && callNumber == 2 {
			arguments = `{"status":"success"}`
		}
		toolCalls := []any{map[string]any{
			"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments},
		}}
		if first && scenario == "multiple calls" {
			toolCalls = append(toolCalls, map[string]any{"index": 1, "id": "capture-2", "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
		}
		writeNativeRepairStream(t, w, map[string]any{"role": "assistant", "tool_calls": toolCalls}, "tool_calls")
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	capability := providerintrospection.CapabilityYes
	if scenario == "no vision" {
		capability = providerintrospection.CapabilityNo
	}
	c := newMediaPolicyCoordinatorWithMCP(t, manager, server.URL+"/v1", factory, capability, func(session *TeamSession) {
		session.MCPServers = map[string]internalmcp.MCPServerConfig{"diagnostics": mcpConfig}
		session.Agents["worker"].Tools = "diagnostics__collect_debug,submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		session.Agents["worker"].MaxSteps = 4
		session.Agents["worker"].MaxRetries = 2
		session.Config.MaxRetries = 2
		// A fixed offline estimate leaves room for both complete tool results.
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
		if scenario == "egress denied" {
			session.MediaPolicy.AllowedTargets = nil
		}
		if scenario == "count limit" {
			session.MediaPolicy.Image.MaxCount = 1
		}
		if fanout {
			configureNativeMediaCost(t, session, "fixture/next")
			session.Agents["worker"].ExtraModels = []string{"fixture/next"}
			provider := session.Config.Providers["fixture"]
			provider.MaxConcurrent = 2
			session.Config.Providers["fixture"] = provider
			session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets,
				media.Target{Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 4096})
		}
	})
	if fanout {
		c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return mediaFanoutIntrospector{}
		}, modelprofile.ProfileCacheOptions{})
	}
	if scenario == "fanout branch" {
		tree := NewSessionTree()
		branch, err := tree.CreateRootBranch("tool-images")
		if err != nil {
			t.Fatal(err)
		}
		tree.ActiveBranch = branch.ID
		if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
			t.Fatal(err)
		}
	}
	task := TaskDef{Agent: "worker", Goal: "Capture the tool images and compare them.", Execution: ExecutionContract{RequiresResult: true}}
	if scenario == "direct" {
		task.Execution.ToolSequence = []string{"diagnostics__collect_debug", "submit_result"}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	_, runErr := c.RunTask(ctx, task)
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 {
		t.Fatal("fixture lost its canonical task", runErr)
	}
	if fake.calls.Load() == 0 {
		for len(requests) > 0 {
			request := <-requests
			for _, tool := range request.Tools {
				t.Log("provider tool:", tool.Function.Name)
			}
			for _, message := range request.Messages {
				if message.Role == "tool" {
					t.Logf("fixture tool response %.512s", message.Content)
				}
			}
		}
	}
	if scenario == "invalid base64" || scenario == "wrong MIME" || scenario == "error image" || scenario == "no vision" || scenario == "egress denied" || scenario == "count limit" {
		if calls.Load() != 1 || fake.calls.Load() != 1 || items[0].Status != TaskError || len(items[0].ExecutionReceipts) != 1 {
			t.Fatalf("invalid media retried a completed tool or reached another model call: HTTP=%d MCP=%d status=%s err=%v", calls.Load(), fake.calls.Load(), items[0].Status, runErr)
		}
		return
	}
	if fanout {
		if items[0].Status != TaskDone || calls.Load() != 4 || fake.calls.Load() != 2 || len(items[0].ExecutionReceipts) != 2 {
			t.Fatalf("fanout lost independent MCP continuations: HTTP=%d MCP=%d status=%s detail=%s err=%v", calls.Load(), fake.calls.Load(), items[0].Status, items[0].Detail, runErr)
		}
		for _, receipt := range items[0].ExecutionReceipts {
			if receipt.AcceptedResult == nil || receipt.AcceptedResult.RuntimeMedia == nil || receipt.RuntimeMediaHash != receipt.AcceptedResult.RuntimeMedia.Hash {
				t.Fatal("fanout leaf lost its receipt-bound image evidence")
			}
		}
		events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
		if err == nil {
			err = VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events)
		}
		if err != nil {
			t.Fatal("fanout media lineage failed independent replay", err)
		}
		return
	}
	expectedCalls := int64(1)
	if scenario == "multiple calls" || scenario == "recycled IDs" {
		expectedCalls = 2
	}
	expectedHTTP := int32(2)
	if scenario == "recycled IDs" || scenario == "result repair" {
		expectedHTTP = 3
	}
	if items[0].Status != TaskDone || calls.Load() != expectedHTTP || fake.calls.Load() != expectedCalls {
		t.Fatalf("native MCP continuation failed: HTTP=%d MCP=%d status=%s err=%v", calls.Load(), fake.calls.Load(), items[0].Status, runErr)
	}
	if items[0].TypedResult == nil || items[0].TypedResult.RuntimeMedia == nil || items[0].ExecutionReceipt == nil || items[0].ExecutionReceipt.RuntimeMediaHash != items[0].TypedResult.RuntimeMedia.Hash {
		t.Fatal("worker image results were not finalized into their canonical receipt")
	}
	c.TaskCache().Store(TaskCacheStoreRequest{AgentKey: "worker", Task: task.Goal, Output: "cached prose"})
	if _, hit := c.TaskCache().Lookup(t.Context(), TaskCacheLookupRequest{Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: task.Goal}); hit {
		t.Fatal("text-starting MCP worker reused a prose-only cache entry")
	}
	first, continuation := <-requests, <-requests
	for range expectedHTTP - 2 {
		continuation = <-requests
	}
	for _, message := range first.Messages {
		if bytes.Contains(message.Content, []byte("image_url")) {
			t.Fatal("plaintext initial request acquired an image")
		}
	}
	var parts []mediaWorkerWirePart
	var images int
	toolResult := -1
	firstImageMessage := len(continuation.Messages)
	var resultCount int64
	pendingCalls := make(map[string]bool)
	for index, message := range continuation.Messages {
		for _, call := range message.ToolCalls {
			if pendingCalls[call.ID] {
				t.Fatal("wire duplicated a still-pending tool call ID")
			}
			pendingCalls[call.ID] = true
		}
		if message.Role == "tool" {
			if !pendingCalls[message.ToolCallID] {
				t.Fatal("wire lost the matching assistant call")
			}
			delete(pendingCalls, message.ToolCallID)
			toolResult = index
			resultCount++
		}
		if message.Role != "user" || json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type != "image_url" {
				continue
			}
			_, data, ok := strings.Cut(part.ImageURL.URL, ",")
			decoded, err := base64.StdEncoding.DecodeString(data)
			if !ok || err != nil || !bytes.Equal(decoded, image) || toolResult < 0 || index <= toolResult || len(pendingCalls) != 0 {
				t.Fatal("image wire changed bytes or preceded its paired tool result")
			}
			images++
			firstImageMessage = min(firstImageMessage, index)
		}
	}
	expectedResults := expectedCalls
	if scenario == "result repair" {
		expectedResults++ // The rejected submit_result has its own paired result.
	}
	if images != int(expectedCalls)*2 || resultCount != expectedResults || scenario != "recycled IDs" && scenario != "result repair" && firstImageMessage <= toolResult || len(pendingCalls) != 0 {
		t.Fatal("SDK dropped or duplicated an ordered MCP image occurrence", images)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal("actual MCP wire lacks independently verifiable evidence", err)
	}
	invocations := make(map[string]bool)
	for _, event := range events {
		if event.Type == string(EventToolMediaPublished) {
			producer, err := decodeToolMediaPublication(event)
			if err != nil || producer.ToolInvocationID == "" || invocations[producer.ToolInvocationID] {
				t.Fatal("separate completed MCP calls shared an invocation identity", err)
			}
			invocations[producer.ToolInvocationID] = true
		}
	}
	if len(invocations) != int(expectedCalls) {
		t.Fatal("MCP calls lost their separate canonical publication identities")
	}
	stored, err := json.Marshal(events)
	if err != nil || bytes.Contains(stored, []byte(encoded)) || bytes.Contains(stored, []byte("data:image/")) || c.DualWriteFailures() != 0 {
		t.Fatalf("MCP execution persisted raw image transport or lost canonical projections: %v", err)
	}
}
