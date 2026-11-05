package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestCoordinatorMediaDelegationKeepsPixelsWithSelectedWorker(t *testing.T) {
	type wireRequest struct {
		Model    string                   `json:"model"`
		Messages []mediaWorkerWireMessage `json:"messages"`
	}
	requests := make(chan wireRequest, 4)
	var coordinatorCalls atomic.Int32
	sourcePaths := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"vision","object":"model"},{"id":"text","object":"model"}]}`)
			return
		}
		var request wireRequest
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			http.Error(w, "extra model invocation", http.StatusBadRequest)
			return
		}
		tool := "submit_result"
		args := map[string]any{"status": "success", "summary": "Worker comparison.", "details": "Native selected-image observations."}
		if request.Model == "text" {
			round := coordinatorCalls.Add(1)
			if round == 1 || round == 3 {
				for _, path := range <-sourcePaths {
					if err := os.Remove(path); err != nil {
						t.Error(err)
					}
				}
				tool = "agent"
				args = map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "Compare selected images.", "image_inputs": []any{
					map[string]any{"turn_id": "coordinator-turn", "part_index": 2}, map[string]any{"turn_id": "coordinator-turn", "part_index": 1}, map[string]any{"turn_id": "coordinator-turn", "part_index": 2},
				}}}}
				if round == 3 {
					args = map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "Inspect the new image only.", "image_inputs": []any{
						map[string]any{"turn_id": "followup-turn", "part_index": 1},
					}}}}
				}
			} else {
				tool = "finish"
				args = map[string]any{"response": "Worker comparison."}
			}
		} else if request.Model != "vision" {
			http.Error(w, "unexpected target", http.StatusBadRequest)
			return
		}
		writeCoordinatorMediaToolStream(t, w, request.Model, tool, args)
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
		session.Config.Timeout, session.Config.MaxRounds, session.Config.MaxSteps = 30, 3, 4
		session.Config.Acceptance = "true"
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		session.Agents["worker"].MaxSteps = 1
		coordinator := cloneAgentDef(session.Agents["worker"])
		coordinator.Name, coordinator.Role, coordinator.Tools = "coordinator", "coordinator", ""
		coordinator.Generation.Model = "fixture/text"
		coordinator.MaxSteps = 4
		session.Agents["coordinator"] = coordinator
		session.Config.CoordinatorModel = "fixture/text"
	})
	c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return nativeSynthesisIntrospector{}
	}, modelprofile.ProfileCacheOptions{})
	first := filepath.Join(c.projectDir, "first.png")
	second := filepath.Join(c.projectDir, "second.png")
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string][]byte{first: mediaStorePNG(t), second: data.Bytes()} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{first, second}
	sourcePaths <- paths
	output, err := c.RunWithMedia(t.Context(), "Compare the original images.", MediaTurnRequest{TurnID: "coordinator-turn", Images: paths})
	if err != nil || output != "Worker comparison." || len(requests) != 3 {
		callCount := len(requests)
		for _, item := range c.TaskTracker().TodoList().Items() {
			t.Logf("task %s status=%s detail=%s", item.ID, item.Status, item.Detail)
		}
		for len(requests) > 0 {
			request := <-requests
			for _, message := range request.Messages {
				if message.Role == "tool" {
					t.Logf("model=%s tool result=%s", request.Model, message.Content)
				}
			}
		}
		t.Fatalf("coordinator image loop: output=%q calls=%d err=%v", output, callCount, err)
	}
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || items[0].MediaBinding == nil || items[0].MediaBinding.InvocationMode != media.InvocationDelegatedTask {
		t.Fatal("coordinator did not complete a canonical delegated image task")
	}
	for index := range 3 {
		request := <-requests
		if index == 1 {
			if request.Model != "vision" {
				t.Fatal("selected worker used the wrong target")
			}
			assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, request.Messages), items[0].MediaBinding, "Compare selected images.")
			continue
		}
		wire, err := json.Marshal(request.Messages)
		if err != nil || request.Model != "text" || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64")) || strings.Count(string(wire), "Image input metadata") != 2 {
			t.Fatal("coordinator received pixels or lost its ordered metadata")
		}
	}
	if c.LastRunResult() == nil || !c.LastRunResult().GoalSatisfied {
		t.Fatal("image delegation lost its configured whole-run acceptance")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	assertNativeMediaCost(t, events, 3)

	third := filepath.Join(c.projectDir, "followup.png")
	if err := os.WriteFile(third, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sourcePaths <- []string{third}
	const followup = "Inspect this newly attached image only."
	output, err = c.ContinueWithMedia(t.Context(), followup, MediaTurnRequest{TurnID: "followup-turn", Images: []string{third}})
	if err != nil || output != "Worker comparison." || len(requests) != 3 {
		t.Fatalf("image continuation: output=%q calls=%d err=%v", output, len(requests), err)
	}
	items = c.TaskTracker().TodoList().Items()
	if len(items) != 2 || items[1].Status != TaskDone || len(items[1].MediaBinding.Selections) != 1 || items[1].MediaBinding.Selections[0].TurnID != "followup-turn" {
		t.Fatal("image continuation inherited old worker inputs or lost its new task")
	}
	for index := range 3 {
		request := <-requests
		if index == 1 {
			assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, request.Messages), items[1].MediaBinding, "Inspect the new image only.")
			continue
		}
		wire, marshalErr := json.Marshal(request.Messages)
		userCopies := 0
		for _, message := range request.Messages {
			if message.Role == "user" {
				userCopies += strings.Count(string(message.Content), followup)
			}
		}
		if marshalErr != nil || request.Model != "text" || bytes.Contains(wire, []byte("image_url")) || strings.Count(string(wire), "Image input metadata") != 3 || userCopies != 1 {
			t.Fatalf("image continuation: model=%s metadata=%d user copies=%d err=%v", request.Model, strings.Count(string(wire), "Image input metadata"), userCopies, marshalErr)
		}
	}
	const plainFollowup = "Summarize the accepted observations briefly."
	output, err = c.ContinueWithPrompt(t.Context(), plainFollowup)
	if err != nil || output != "Worker comparison." || len(requests) != 1 {
		t.Fatalf("plain continuation: output=%q calls=%d err=%v", output, len(requests), err)
	}
	plain := <-requests
	wire, err := json.Marshal(plain.Messages)
	if err != nil || plain.Model != "text" || !bytes.Contains(wire, []byte(plainFollowup)) || bytes.Contains(wire, []byte("image_url")) {
		t.Fatal("plain follow-up was dropped or automatically resent images", err)
	}
	events, err = c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal("multi-turn image lineage failed independent audit", err)
	}
	assertNativeMediaCost(t, events, 7)
}

func writeCoordinatorMediaToolStream(t *testing.T, w http.ResponseWriter, model, tool string, args map[string]any) {
	t.Helper()
	arguments, err := json.Marshal(args)
	if err != nil {
		t.Error(err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for index, delta := range []map[string]any{{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call-" + tool, "type": "function", "function": map[string]any{"name": tool, "arguments": string(arguments)}}}}, {}} {
		var reason any
		if index == 1 {
			reason = "tool_calls"
		}
		data, err := json.Marshal(map[string]any{"id": "coordinator-media", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestCoordinatorImageFinishRejectsMetadataWithoutNativeWork(t *testing.T) {
	c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := historyReferences(request.Call.Prompt)[0].Reference.Source
	snapshot := view.snapshots[source.SnapshotID]
	ctx = context.WithValue(ctx, coordinatorCommittedMediaKey{}, snapshot)
	requireMediaCode(t, c.requireCoordinatorImageWork(ctx), media.DurabilityRequired)
	response, err := (&finishTool{coordinator: c}).Run(ctx, fantasy.ToolCall{Input: `{"response":"I inspected the images."}`})
	if err != nil || !response.IsError || c.finishCalled.Load() || c.LastRunResult() != nil {
		t.Fatal("metadata-only coordinator manufactured image completion", err)
	}
}

func TestCoordinatorMediaPromptProtectionPreservesPlainFollowup(t *testing.T) {
	c, ctx, request := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := historyReferences(request.Call.Prompt)[0].Reference.Source
	snapshot := view.snapshots[source.SnapshotID]
	ctx = context.WithValue(ctx, coordinatorCommittedMediaKey{}, snapshot)
	if !coordinatorMediaPromptCommitted(ctx, snapshot.Input.Parts[0].Text, request.Call.Prompt) ||
		coordinatorMediaPromptCommitted(ctx, "new plain follow-up", request.Call.Prompt) ||
		coordinatorMediaPromptCommitted(ctx, snapshot.Input.Parts[0].Text, nil) {
		t.Fatal("mixed-turn prompt protection dropped a new prompt or duplicated a committed one")
	}
}

func TestCoordinatorMediaDownshiftRebindsMetadataGrant(t *testing.T) {
	c, ctx, original := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	c.modelList = []config.ModelEntry{{ID: "fixture/vision"}, {ID: "fixture/text"}}
	preflight := newCoordinatorRequestPreflightWithAdmission(original.ModelID, "", "system", nil, original.AdmissionContext)
	continuation, err := c.admitCoordinatorEarlierModel(ctx, preflight, original.Call.Prompt, "", 0, 1024, original.ModelID)
	if err != nil || continuation.Model == nil || continuation.Invocation.ModelID != "fixture/vision" {
		t.Fatal("metadata coordinator could not admit its earlier model", err)
	}
	request := agent.ProviderRequest{ModelID: continuation.Invocation.ModelID, AdmissionContext: continuation.Invocation.AdmissionContext, Messages: continuation.Messages}
	if permitted, err := c.resolveProviderReferenceMetadata(continuation.Context, request); err != nil || !permitted {
		t.Fatal("downshift did not own its candidate-specific metadata grant", err)
	}
	if _, err := c.resolveProviderReferenceMetadata(ctx, request); err == nil {
		t.Fatal("candidate reused the previous model's metadata grant")
	}
	wire, release, err := (providerRequestAdmission{c: c}).CompileProviderRequest(continuation.Context, request)
	if err != nil || release != nil || agent.ProviderRequestHasMedia(wire) {
		t.Fatal("downshift metadata compilation acquired image bytes", err)
	}
}
