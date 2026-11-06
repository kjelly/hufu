package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

type mediaFanoutIntrospector struct{}

func (mediaFanoutIntrospector) InspectModel(ctx context.Context, ref providerintrospection.ProviderRef, model string) (providerintrospection.RuntimeModelInfo, error) {
	if strings.TrimPrefix(model, "fixture/") == "text" {
		return providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768, ImageInput: providerintrospection.CapabilityNo, Capabilities: []string{"tools"}}, nil
	}
	return (routedImageIntrospector{next: providerintrospection.CapabilityYes}).InspectModel(ctx, ref, model)
}

func TestNativeMediaModelFanoutKeepsIndependentGrantsAndOriginalCAS(t *testing.T) {
	for _, mode := range []string{"single_task", "coordinator", "judge_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			type wireRequest struct {
				Model    string                   `json:"model"`
				Messages []mediaWorkerWireMessage `json:"messages"`
			}
			requests := make(chan wireRequest, 6)
			var coordinatorCalls atomic.Int32
			paths := make(chan []string, 1)
			removeSources := sync.OnceFunc(func() {
				select {
				case sources := <-paths:
					for _, path := range sources {
						if err := os.Remove(path); err != nil {
							t.Error(err)
						}
					}
				default:
					t.Error("missing original source fixture")
				}
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[{"id":"vision"},{"id":"next"},{"id":"text"}]}`)
					return
				}
				var request wireRequest
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || (request.Model != "vision" && request.Model != "next" && request.Model != "text") {
					http.Error(w, "invalid fanout fixture request", http.StatusBadRequest)
					return
				}
				select {
				case requests <- request:
				default:
					http.Error(w, "unexpected repeated model invocation", http.StatusBadRequest)
					return
				}
				removeSources()
				if request.Model == "text" {
					if mode == "judge_failure" {
						http.Error(w, "offline judge unavailable", http.StatusUnauthorized)
						return
					}
					if coordinatorCalls.Add(1) == 1 {
						writeCoordinatorMediaToolStream(t, w, request.Model, "agent", map[string]any{"tasks": []any{map[string]any{
							"agent": "worker", "goal": "Inspect the original images independently.", "image_inputs": []any{
								map[string]any{"turn_id": "fanout-input", "part_index": 1}, map[string]any{"turn_id": "fanout-input", "part_index": 2}, map[string]any{"turn_id": "fanout-input", "part_index": 3},
							},
						}}})
					} else {
						writeCoordinatorMediaToolStream(t, w, request.Model, "finish", map[string]any{"response": "Fanout models inspected."})
					}
					return
				}
				writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{"status": "success", "summary": request.Model + " image observations.", "details": "Independent native CAS input."})
			}))
			t.Cleanup(server.Close)
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				return providerproxy.StartInProcess(ctx, cfg)
			}
			c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
				configureNativeMediaCost(t, session, "fixture/next")
				session.Config.Acceptance = "true"
				session.Config.Timeout, session.Config.MaxRounds, session.Config.MaxSteps = 30, 3, 4
				if mode == "judge_failure" {
					session.Config.JudgeModel = "fixture/text"
				}
				worker := session.Agents["worker"]
				worker.Tools, worker.ResultTransport, worker.MaxSteps = "submit_result", agent.ResultTransportTool, 1
				worker.ExtraModels = []string{"fixture/next"}
				provider := session.Config.Providers["fixture"]
				provider.MaxConcurrent = 2
				session.Config.Providers["fixture"] = provider
				session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
				session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets, media.Target{Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 4096})
				if mode == "coordinator" {
					coordinator := cloneAgentDef(worker)
					coordinator.Name, coordinator.Role, coordinator.Tools = "coordinator", "coordinator", ""
					coordinator.Generation.Model, coordinator.MaxSteps, coordinator.ExtraModels = "fixture/text", 3, nil
					session.Agents["coordinator"] = coordinator
					session.Config.CoordinatorModel = "fixture/text"
				}
			})
			c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return mediaFanoutIntrospector{}
			}, modelprofile.ProfileCacheOptions{})
			var second bytes.Buffer
			if err := png.Encode(&second, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
				t.Fatal(err)
			}
			var sourcePaths []string
			for index, data := range [][]byte{mediaStorePNG(t), second.Bytes()} {
				path := filepath.Join(c.projectDir, []string{"first.png", "second.png"}[index])
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				sourcePaths = append(sourcePaths, path)
			}
			paths <- sourcePaths
			const goal = "Inspect the original images independently."
			input := MediaTurnRequest{
				TurnID: "fanout-input", Images: []string{sourcePaths[1], sourcePaths[0], sourcePaths[1]},
			}
			wantCalls := 2
			if mode == "judge_failure" {
				wantCalls = 3
			}
			var runErr error
			if mode == "coordinator" {
				wantCalls = 4
				_, runErr = c.RunWithMedia(ctx, goal, input)
			} else {
				_, runErr = c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: goal}, input)
			}
			items := c.TaskTracker().TodoList().Items()
			if len(requests) != wantCalls || len(items) != 1 || items[0].Status != TaskDone || items[0].MediaBinding == nil ||
				len(items[0].ExecutionReceipts) != 2 || c.LastRunResult() == nil || !c.LastRunResult().GoalSatisfied {
				for _, item := range items {
					t.Logf("fanout task status=%s detail=%s receipts=%d", item.Status, item.Detail, len(item.ExecutionReceipts))
				}
				t.Fatalf("native image fanout did not finish with independent receipts: calls=%d want=%d tasks=%d err=%v", len(requests), wantCalls, len(items), runErr)
			}
			models := make(map[string]bool)
			for range wantCalls {
				request := <-requests
				if request.Model == "text" {
					wire, err := json.Marshal(request.Messages)
					if err != nil || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64,")) {
						t.Fatal("fanout coordinator received image pixels", err)
					}
					continue
				}
				if models[request.Model] {
					t.Fatal("fanout repeated one model instead of using its admitted slots")
				}
				models[request.Model] = true
				assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, request.Messages), items[0].MediaBinding, goal)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal("independent fanout media audit", err)
			}
			if mode == "judge_failure" {
				// The judge's typed choice resolver is billed as a sidecar call.
				assertNativeMediaCost(t, events, wantCalls, cost.PurposeSidecar)
			} else {
				assertNativeMediaCost(t, events, wantCalls)
			}
			grants := make(map[string]MediaProviderFanoutGrant)
			for _, event := range events {
				if event.Type != string(EventMediaProviderAdmitted) {
					continue
				}
				payload, err := decodeMediaProviderEvent(event)
				if err != nil || !payload.ModelFanout.present() || grants[payload.ModelFanout.ModelExecutionID].present() {
					t.Fatal("fanout has missing or repeated leaf permission", err)
				}
				grants[payload.ModelFanout.ModelExecutionID] = payload.ModelFanout
			}
			if len(grants) != 2 {
				t.Fatal("fanout did not preserve both model execution grants")
			}
			for _, receipt := range items[0].ExecutionReceipts {
				if !grants[receipt.ModelExecutionID].present() || receipt.MediaInputDigest != items[0].MediaInputDigest {
					t.Fatal("fanout completion lost its own admitted media identity")
				}
				if receipt.AcceptedResult == nil || receipt.AcceptedResult.Details != "Independent native CAS input." {
					t.Fatal("fanout completion lost its accepted semantic result")
				}
			}
			replayed := todoItemByID(ReduceToTodoList(events), items[0].ID)
			if replayed == nil || !c.nativeMediaAcceptedWork(replayed) {
				t.Fatal("canonical replay cannot verify accepted fanout completion")
			}
		})
	}
}
