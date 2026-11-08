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
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

type routedImageIntrospector struct {
	next providerintrospection.CapabilityState
}

func (i routedImageIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, model string) (providerintrospection.RuntimeModelInfo, error) {
	state := providerintrospection.CapabilityYes
	if strings.TrimPrefix(model, "fixture/") == "next" {
		state = i.next
	}
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768, ImageInput: state, Capabilities: []string{"tools"}}, nil
}

func TestNativeMediaRouteFallbackPreservesCASAndChecksEachCandidate(t *testing.T) {
	for _, test := range []struct {
		name        string
		firstStatus int
		next        providerintrospection.CapabilityState
		allowNext   bool
		wantCalls   int
		wantFailure bool
	}{
		{"authorized vision fallback", http.StatusServiceUnavailable, providerintrospection.CapabilityYes, true, agent.ProviderTransientRetryLimit + 2, false},
		{"text fallback", http.StatusServiceUnavailable, providerintrospection.CapabilityNo, true, agent.ProviderTransientRetryLimit + 1, true},
		{"unknown fallback", http.StatusServiceUnavailable, providerintrospection.CapabilityUnknown, true, agent.ProviderTransientRetryLimit + 1, true},
		{"ungranted fallback", http.StatusServiceUnavailable, providerintrospection.CapabilityYes, false, agent.ProviderTransientRetryLimit + 1, true},
		{"auth failure does not fall back", http.StatusUnauthorized, providerintrospection.CapabilityYes, true, 1, true},
		{"successful primary", http.StatusOK, providerintrospection.CapabilityYes, true, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			type wireRequest struct {
				Model    string                   `json:"model"`
				Messages []mediaWorkerWireMessage `json:"messages"`
			}
			requests := make(chan wireRequest, 4)
			paths := make(chan []string, 1)
			var removedSources atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[{"id":"vision"},{"id":"next"}]}`)
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
				if request.Model == "vision" {
					if removedSources.CompareAndSwap(false, true) {
						var sourcePaths []string
						select {
						case sourcePaths = <-paths:
						default:
							http.Error(w, "source paths were not supplied", http.StatusBadRequest)
							return
						}
						for _, path := range sourcePaths {
							if err := os.Remove(path); err != nil {
								t.Error(err)
							}
						}
					}
					if test.firstStatus != http.StatusOK {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(test.firstStatus)
						_, _ = io.WriteString(w, `{"error":{"message":"fixture provider failure"}}`)
						return
					}
				} else if request.Model != "next" {
					http.Error(w, "unexpected model", http.StatusBadRequest)
					return
				}
				writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{"status": "success", "summary": "Route image observations.", "details": "Original CAS bytes preserved."})
			}))
			t.Cleanup(server.Close)
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				return providerproxy.StartInProcess(ctx, cfg)
			}
			c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
				configureNativeMediaCost(t, session, "fixture/next")
				session.Config.Acceptance = "true"
				worker := session.Agents["worker"]
				worker.Tools, worker.ResultTransport = "submit_result", agent.ResultTransportTool
				worker.Generation.Model, worker.ExecutionRoute = "", "vision-route"
				session.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"vision-route": {
					Candidates: []string{"fixture/vision", "fixture/next"}, FallbackOn: []string{string(ProviderUnavailable)},
				}}
				session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
				if test.allowNext {
					session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets, media.Target{Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 4096})
				}
			})
			c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return routedImageIntrospector{next: test.next}
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
			const goal = "Inspect routed image inputs."
			_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: goal}, MediaTurnRequest{
				TurnID: "routed-input", Images: []string{sourcePaths[1], sourcePaths[0], sourcePaths[1]},
			})
			if test.wantFailure && runErr == nil || len(requests) != test.wantCalls {
				t.Fatalf("route outcome: calls=%d failure=%v err=%v", len(requests), test.wantFailure, runErr)
			}
			items := c.TaskTracker().TodoList().Items()
			if len(items) != 1 || items[0].MediaBinding == nil || c.LastRunResult() == nil || c.LastRunResult().GoalSatisfied == test.wantFailure || IsRunOutcomeSuccess(c.LastRunResult().Outcome) == test.wantFailure {
				t.Fatal("route lost its canonical binding or truthful acceptance")
			}
			for index := range test.wantCalls {
				request := <-requests
				wantModel := "vision"
				if index == agent.ProviderTransientRetryLimit+1 {
					wantModel = "next"
				}
				if request.Model != wantModel {
					t.Fatal("route used a different candidate", request.Model)
				}
				assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, request.Messages), items[0].MediaBinding, goal)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal("route media replay", err)
			}
			if test.allowNext {
				for index, event := range events {
					if event.Type != string(EventMediaProviderAdmitted) {
						continue
					}
					payload, err := decodeMediaProviderEvent(event)
					if err != nil {
						t.Fatal(err)
					}
					payload.Target.Model = "next"
					if payload.Attempt > 1 {
						payload.Target.Model = "vision"
					}
					event.Payload, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					requireMediaCode(t, verifyMediaProviderAdmission(event, payload, events[:index]), media.BindingConflict)
				}
			}
			assertNativeMediaCost(t, events, test.wantCalls)
			if !test.wantFailure && (items[0].Status != TaskDone || items[0].ExecutionReceipt == nil || items[0].ExecutionReceipt.MediaInputDigest != items[0].MediaInputDigest) {
				t.Fatal("successful route lost its native completion receipt")
			}
		})
	}
}
