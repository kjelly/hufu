package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeMediaFanoutProviderBoundaryFailurePreventsWorkers(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "worker bypassed failed boundary", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	factory := func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
		return nil, errors.New("offline boundary startup failure")
	}
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaProcessResume(session, true)
	})
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Compare the images."}, MediaTurnRequest{Images: []string{path}})
	if err == nil || calls.Load() != 0 || c.LastRunResult() == nil || c.LastRunResult().GoalSatisfied {
		t.Fatal("failed fanout provider boundary did not prevent execution", calls.Load(), err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventMediaProviderAdmitted) {
			t.Fatal("failed boundary admitted an image provider request")
		}
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}

func TestNativeMediaFanoutFailurePreservesIndependentLeafEvidence(t *testing.T) {
	for _, scenario := range []string{"text leaf", "unknown leaf", "ungranted leaf", "cancel both leaves"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			started := make(chan struct{}, 2)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					_, _ = io.WriteString(w, `{"data":[{"id":"vision"},{"id":"next"}]}`)
					return
				}
				var request struct {
					Model string `json:"model"`
				}
				if json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil {
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				calls.Add(1)
				if scenario == "cancel both leaves" {
					started <- struct{}{}
					<-r.Context().Done()
					return
				}
				if request.Model != "vision" {
					t.Error("denied leaf reached HTTP")
				}
				writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{
					"status": "success", "summary": "Original vision result.", "details": "Retain this successful leaf despite its sibling failure.",
				})
			}))
			t.Cleanup(server.Close)
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				return providerproxy.StartInProcess(ctx, cfg)
			}
			c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
				configureNativeMediaCost(t, session, "fixture/next")
				session.Config.Acceptance = "true"
				session.Config.Timeout, session.Config.MaxSteps = 20, 1
				worker := session.Agents["worker"]
				worker.Tools, worker.ResultTransport, worker.ExtraModels = "submit_result", agent.ResultTransportTool, []string{"fixture/next"}
				provider := session.Config.Providers["fixture"]
				provider.MaxConcurrent = 2
				session.Config.Providers["fixture"] = provider
				if scenario != "ungranted leaf" {
					session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets, media.Target{
						Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 8192})
				}
			})
			next := providerintrospection.CapabilityYes
			if scenario == "text leaf" {
				next = providerintrospection.CapabilityNo
			}
			if scenario == "unknown leaf" {
				next = providerintrospection.CapabilityUnknown
			}
			c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return routedImageIntrospector{next: next}
			}, modelprofile.ProfileCacheOptions{})
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			var watchers sync.WaitGroup
			if scenario == "cancel both leaves" {
				watchers.Go(func() {
					for range 2 {
						select {
						case <-started:
						case <-ctx.Done():
							return
						}
					}
					cancel()
				})
			}
			_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Inspect the original image."}, MediaTurnRequest{Images: []string{path}})
			cancel()
			watchers.Wait()
			wantCalls, wantAccepted := int32(1), 1
			if scenario == "cancel both leaves" {
				wantCalls, wantAccepted = 2, 0
			}
			items := c.TaskTracker().TodoList().Items()
			if runErr == nil || calls.Load() != wantCalls || len(items) != 1 || items[0].Status == TaskDone ||
				c.LastRunResult() == nil || c.LastRunResult().GoalSatisfied {
				events, _ := c.EventJournal().ReadEvents(t.Context())
				for _, event := range events {
					if event.TaskID == "" {
						continue
					}
					var state struct {
						Status             string `json:"status"`
						OccurrenceRevision int    `json:"occurrence_revision"`
						DispatchAttempt    int    `json:"dispatch_attempt"`
					}
					_ = json.Unmarshal(event.Payload, &state)
					t.Logf("task event %s revision=%d status=%s dispatch_attempt=%d", event.Type, state.OccurrenceRevision, state.Status, state.DispatchAttempt)
				}
				t.Fatalf("fanout failure evidence mismatch: calls=%d want=%d err=%v", calls.Load(), wantCalls, runErr)
			}
			accepted := 0
			for _, receipt := range items[0].ExecutionReceipts {
				if receipt.AcceptedResult != nil {
					accepted++
				}
			}
			if accepted != wantAccepted || len(items[0].ExecutionReceipts) != 2 || c.nativeMediaAcceptedWork(items[0]) {
				t.Fatal("failed fanout lost independent evidence or accepted an incomplete topology")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
		})
	}
}
