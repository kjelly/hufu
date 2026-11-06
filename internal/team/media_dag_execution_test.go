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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestCoordinatorMediaDAGKeepsDifferentInputsOutOfAutomaticReuse(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		cancel bool
	}{{"completed", false}, {"cancelled after first worker", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			type wireRequest struct {
				Model    string                   `json:"model"`
				Messages []mediaWorkerWireMessage `json:"messages"`
			}
			requests := make(chan wireRequest, 5)
			paths := make(chan []string, 1)
			var coordinators, workers atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[{"id":"vision"},{"id":"text"}]}`)
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
					http.Error(w, "unexpected repeated inference", http.StatusBadRequest)
					return
				}
				if request.Model == "text" {
					if coordinators.Add(1) == 1 {
						select {
						case sources := <-paths:
							for _, path := range sources {
								if err := os.Remove(path); err != nil {
									t.Error(err)
								}
							}
						default:
							http.Error(w, "missing source fixture", http.StatusBadRequest)
							return
						}
						writeCoordinatorMediaToolStream(t, w, request.Model, "agent", map[string]any{"tasks": []any{
							map[string]any{"agent": "worker", "goal": "Inspect selected input.", "image_inputs": []any{map[string]any{"turn_id": "dag-input", "part_index": 1}}},
							map[string]any{"agent": "worker", "goal": "Inspect selected input.", "depends_on": []int{0}, "image_inputs": []any{map[string]any{"turn_id": "dag-input", "part_index": 2}}},
						}})
						return
					}
					if workers.Load() != 2 {
						http.Error(w, "DAG worker evidence is incomplete", http.StatusBadRequest)
						return
					}
					writeCoordinatorMediaToolStream(t, w, request.Model, "finish", map[string]any{"response": "DAG native inputs inspected."})
					return
				}
				if request.Model != "vision" {
					http.Error(w, "unexpected model", http.StatusBadRequest)
					return
				}
				workers.Add(1)
				if scenario.cancel {
					cancel()
					return
				}
				writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{"status": "success", "summary": "Selected image inspected.", "details": "Only this occurrence's native input."})
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
				coordinator.Generation.Model, coordinator.MaxSteps = "fixture/text", 3
				session.Agents["coordinator"] = coordinator
				session.Config.CoordinatorModel = "fixture/text"
			})
			c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return nativeSynthesisIntrospector{}
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
			output, err := c.RunWithMedia(ctx, "Inspect both inputs in order.", MediaTurnRequest{TurnID: "dag-input", Images: sourcePaths})
			wantCalls := 4
			if scenario.cancel {
				wantCalls = 2
			}
			if (err != nil) != scenario.cancel || !scenario.cancel && output != "DAG native inputs inspected." || len(requests) != wantCalls {
				t.Fatal("DAG input execution did not finish exactly once per occurrence", output, err, len(requests))
			}
			items := c.TaskTracker().TodoList().Items()
			if len(items) != 2 || items[0].MediaBinding == nil || items[1].MediaBinding == nil || items[0].MediaInputDigest == items[1].MediaInputDigest ||
				len(items[1].DependsOn) != 1 || items[1].DependsOn[0] != items[0].ID || c.LastRunResult() == nil || c.LastRunResult().GoalSatisfied == scenario.cancel {
				t.Fatal("DAG lost its dependency, distinct media input or acceptance")
			}
			if scenario.cancel && (workers.Load() != 1 || items[0].Status == TaskDone || items[1].Status == TaskDone || len(items[1].ExecutionReceipts) != 0 || c.GetTaskResult(items[1].ID) != nil) {
				t.Fatal("cancelled DAG launched a dependent image worker or invented completion")
			}
			for index := range wantCalls {
				request := <-requests
				if index == 0 || index == 3 {
					wire, err := json.Marshal(request.Messages)
					if err != nil || request.Model != "text" || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64,")) {
						t.Fatal("DAG coordinator received pixels", err)
					}
					continue
				}
				item := items[index-1]
				if request.Model != "vision" || !scenario.cancel && (item.Status != TaskDone || c.GetTaskResult(item.ID) == nil || len(item.ExecutionReceipts) != 1 ||
					item.ExecutionReceipt == nil || item.ExecutionReceipt.MediaInputDigest != item.MediaInputDigest) {
					t.Fatal("DAG reused another image occurrence's completion")
				}
				assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, request.Messages), item.MediaBinding, "Inspect selected input.")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
			assertNativeMediaCost(t, events, wantCalls)
		})
	}
}
