package team

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

type nativeSynthesisIntrospector struct{}

func (nativeSynthesisIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, model string) (providerintrospection.RuntimeModelInfo, error) {
	imageInput := providerintrospection.CapabilityNo
	if model == "vision" || model == "fixture/vision" {
		imageInput = providerintrospection.CapabilityYes
	}
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768, ImageInput: imageInput, Capabilities: []string{"tools"}}, nil
}

func TestNativeDirectSynthesisConsumesTextWithinOriginalRun(t *testing.T) {
	for _, scenario := range []string{"success", "provider failure", "tool attempt", "cancelled", "capacity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			type wireRequest struct {
				Model    string                   `json:"model"`
				Messages []mediaWorkerWireMessage `json:"messages"`
				Tools    []jsontext.Value         `json:"tools"`
				History  []fantasy.Message        `json:"-"`
			}
			requests := make(chan wireRequest, 4)
			var c *Coordinator
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request wireRequest
				if json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				if request.Model == "text" {
					c.conversationHistoryMu.Lock()
					request.History = cloneMessages(c.conversationHistory)
					c.conversationHistoryMu.Unlock()
				}
				select {
				case requests <- request:
				default:
					http.Error(w, "unexpected extra request", http.StatusBadRequest)
					return
				}
				if request.Model == "text" {
					switch scenario {
					case "provider failure":
						http.Error(w, "fixture rejects presentation", http.StatusBadRequest)
						return
					case "cancelled":
						cancel()
						<-r.Context().Done()
						return
					case "tool attempt":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"id\":\"forbidden-delegation\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"delegate-again\",\"type\":\"function\",\"function\":{\"name\":\"agent\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"forbidden-delegation\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
						return
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if request.Model == "vision" {
					arguments := `{"status":"success","summary":"Worker comparison.","details":"Canonical worker observations."}`
					fmt.Fprintf(w, "data: {\"id\":\"native-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-native\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", arguments)
					_, _ = io.WriteString(w, "data: {\"id\":\"native-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, "data: {\"id\":\"native-synthesis\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Formatted comparison.\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"native-synthesis\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"text\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				return providerproxy.StartInProcess(ctx, cfg)
			}
			c = newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
				configureNativeMediaCost(t, session)
				session.Agents["worker"].Tools = "submit_result"
				session.Agents["worker"].ResultTransport = agent.ResultTransportTool
				coordinator := cloneAgentDef(session.Agents["worker"])
				coordinator.Name, coordinator.Role, coordinator.Tools = "coordinator", "coordinator", ""
				coordinator.Generation.Model = "fixture/text"
				if scenario == "capacity" {
					coordinator.Generation.ContextWindow = 128
				}
				session.Agents["coordinator"] = coordinator
				session.Config.CoordinatorModel = "fixture/text"
				session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
			})
			c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return nativeSynthesisIntrospector{}
			}, modelprofile.ProfileCacheOptions{})
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := c.RunDirectAgentWithMediaAndSynthesis(ctx, "worker", "Compare the images.", MediaTurnRequest{Images: []string{path, path}})
			wantCalls := 2
			if scenario == "capacity" {
				wantCalls = 1
			}
			if result == nil || len(requests) != wantCalls || (scenario == "success" && (err != nil || result.Output != "Formatted comparison.")) || (scenario != "success" && err == nil) {
				t.Fatalf("native presentation outcome: scenario=%s result=%+v calls=%d err=%v", scenario, result, len(requests), err)
			}
			items := c.TaskTracker().TodoList().Items()
			if len(items) != 1 || items[0].Status != TaskDone || len(items[0].ExecutionReceipts) != 1 || items[0].MediaBinding.InvocationMode != media.InvocationDirectAgent {
				t.Fatal("presentation changed the worker lifecycle")
			}
			worker := <-requests
			assertMediaWorkerWire(t, worker.Messages, items[0].MediaBinding)
			if wantCalls == 2 {
				formatter := <-requests
				encoded, err := json.Marshal(formatter.Messages)
				if err != nil || formatter.Model != "text" || len(formatter.Tools) != 0 || bytes.Contains(encoded, []byte("image_url")) || bytes.Contains(encoded, []byte("base64,")) || bytes.Contains(encoded, []byte("media-v1-")) || !bytes.Contains(encoded, []byte("Canonical worker observations.")) {
					t.Fatal("presentation did not use its explicit text result view", err)
				}
				c.conversationHistoryMu.Lock()
				history := cloneMessages(c.conversationHistory)
				c.conversationHistoryMu.Unlock()
				if !reflect.DeepEqual(formatter.History, history) {
					t.Fatal("text presentation rewrote durable conversation history")
				}
			}
			if accepted := c.GetTaskResult(items[0].ID); accepted == nil || accepted.Details != "Canonical worker observations." {
				t.Fatal("presentation rewrote the accepted worker result")
			}
			if outcome := c.LastRunResult(); outcome == nil || outcome.GoalSatisfied || IsRunOutcomeSuccess(outcome.Outcome) || (scenario == "success" && (outcome.Outcome != RunOutcomeUnverified || outcome.StopReason != StopReasonAcceptanceNotSet)) {
				t.Fatal("text presentation invented acceptance")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			runs, imageGrants, textProfiles := 0, 0, 0
			for _, event := range events {
				if event.Type == string(EventRunStarted) {
					runs++
				}
				if event.Type == string(EventMediaProviderAdmitted) {
					imageGrants++
				}
				if event.Type == string(EventModelProfileResolved) {
					var profile modelprofile.TelemetryProjection
					if err := json.Unmarshal(event.Payload, &profile); err != nil {
						t.Fatal(err)
					}
					if profile.ModelID == "fixture/text" && profile.Capabilities["image_input"].State == modelprofile.CapabilityNo {
						textProfiles++
					}
				}
			}
			if runs != 1 || imageGrants != 1 {
				t.Fatal("presentation created a new run or image grant")
			}
			if wantCalls == 2 && textProfiles != 1 {
				t.Fatal("presentation did not retain its model's explicit lack of image capability")
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
			assertNativeMediaCost(t, events, wantCalls)
		})
	}
}
