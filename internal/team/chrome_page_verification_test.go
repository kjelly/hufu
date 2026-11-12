package team

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
)

func TestHumanVerificationObservationsPersistWithoutOperatorContent(t *testing.T) {
	c := newTestCoordinator(t)
	c.initEventStore()
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{RunID: c.executionRunID, TaskID: "worker-task", AgentName: "worker", Attempt: 2})
	ctx = c.withChromePageVerification(ctx)
	ctx = tools.WithHumanVerificationRequest(ctx, tools.HumanVerificationObservation{RequestID: "request-one", URLSHA256: strings.Repeat("a", 64), Attempt: 1, Trigger: "automatic"})
	statuses := []string{"requested", "prompt_requested", "answer_rejected", "confirmed", "page_unresolved"}
	for _, status := range statuses {
		if err := tools.ObserveHumanVerification(ctx, status); err != nil {
			t.Fatal(err)
		}
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, event := range events {
		if event.Type != string(EventHumanVerificationObserved) {
			continue
		}
		var observation tools.HumanVerificationObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			t.Fatal(err)
		}
		if event.TaskID != "worker-task" || event.Attempt != 2 || event.Actor != "worker" || observation.RequestID != "request-one" {
			t.Fatalf("event=%+v observation=%+v", event, observation)
		}
		actual = append(actual, observation.Status)
	}
	if !slices.Equal(actual, statuses) {
		t.Fatalf("statuses=%v", actual)
	}
	if _, err := ReplayTodoList(events); err != nil {
		t.Fatal(err)
	}
	if err := c.eventStore.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tools.ObserveHumanVerification(ctx, "page_verified"); err == nil {
		t.Fatal("closed durable journal allowed verification")
	}
	reopened, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	replayed, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != len(events) {
		t.Fatalf("persisted events=%d want=%d", len(replayed), len(events))
	}
	for i := range events {
		if replayed[i].Hash != events[i].Hash {
			t.Fatal("reopening changed canonical evidence")
		}
	}
}

func TestChromePageClassificationUsesDurableSystemOneDecision(t *testing.T) {
	for _, scenario := range []string{"challenge", "readable", "access_error", "uncertain", "provider_error", "invalid", "repair", "closed_sequence", "force_mcp"} {
		t.Run(scenario, func(t *testing.T) {
			state := map[string]int{"challenge": 1, "readable": 0, "access_error": 2, "uncertain": 3}[scenario]
			response, err := json.Marshal(map[string]any{"answers": map[string]any{"decision": map[string]any{
				"type": "choice", "confidence": .25, "choice": []string{"o00", "o01", "o02", "o03"}[state],
				"probabilities": map[string]float64{"o00": .25, "o01": .25, "o02": .25, "o03": .25},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			status := 200
			if scenario == "provider_error" {
				status = 503
			}
			if scenario == "invalid" {
				response = []byte(`{"answers":{}}`)
			}
			server := newControlModelServer(t, status, string(response))
			c := newTestCoordinator(t)
			c.session.GlobalDecisionRuntime = settings.Config{Backend: "systemone", SystemOne: settings.SystemOne{Model: "test-page-model", Endpoint: server.URL}}
			c.decisionPrimitiveGate = make(chan struct{}, 1)
			c.initEventStore()
			ctx := t.Context()
			if scenario == "repair" {
				ctx = withInvocationMetadata(ctx, InvocationMetadata{Purpose: "result_repair"})
			}
			if scenario == "closed_sequence" {
				ctx = context.WithValue(ctx, taskToolSequenceKey{}, newTaskToolSequence([]string{"chrome__browser_snapshot"}, nil, "", nil))
			}
			if scenario == "force_mcp" {
				ctx = context.WithValue(ctx, tools.AgentForceMCPKey, true)
			}
			page := browser.Snapshot{URL: "https://example.test/source", Title: "Just a moment...", Text: "Performing security verification\nPerformance and Security by Cloudflare", CapturedAt: time.Now().UTC()}
			got, err := c.classifyChromePage(ctx, page)
			policyDenied := scenario == "repair" || scenario == "closed_sequence" || scenario == "force_mcp"
			wantError := scenario == "provider_error" || scenario == "invalid" || policyDenied
			want := []mcp.ChromePageState{mcp.ChromePageReadable, mcp.ChromePageNeedsHuman, mcp.ChromePageAccessError, mcp.ChromePageUncertain}[state]
			if (err != nil) != wantError || (!wantError && got != want) {
				t.Fatalf("state=%s error=%v", got, err)
			}
			if policyDenied {
				if server.calls.Load() != 0 {
					t.Fatal("policy-denied classification launched inference")
				}
				return
			}
			if wantError {
				return
			}
			if _, err := c.classifyChromePage(ctx, page); err != nil {
				t.Fatal(err)
			}
			if server.calls.Load() != 1 {
				t.Fatal("settled classification did not replay")
			}
			records, err := c.DecisionPrimitiveResults(t.Context())
			if err != nil || len(records) != 2 || !records[1].Reused || records[0].Backend != "systemone" {
				t.Fatalf("records=%+v error=%v", records, err)
			}
			server.mu.Lock()
			body := strings.Join(server.body, "\n")
			server.mu.Unlock()
			if !strings.Contains(body, "Performing security verification") || !strings.Contains(body, "untrusted evidence") {
				t.Fatal("classification lost actual page evidence or trust boundary")
			}
		})
	}
}

func TestChromePageSidecarClassificationPreservesProviderResponseBoundary(t *testing.T) {
	for _, scenario := range []string{"separate_reasoning", "reasoning_in_content"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request struct {
					ReasoningEffort string `json:"reasoning_effort"`
					MaxTokens       int    `json:"max_tokens"`
				}
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
				}
				if request.ReasoningEffort != "none" || request.MaxTokens != 512 {
					t.Errorf("unexpected classification transport: %+v", request)
				}
				message := map[string]any{"role": "assistant", "content": `{"token":"A0"}`, "reasoning": "The public article is readable."}
				if scenario == "reasoning_in_content" {
					message["content"] = `The public article is readable.</think>{"token":"A0"}`
					delete(message, "reasoning")
				}
				w.Header().Set("Content-Type", "application/json")
				response := map[string]any{
					"id": "page-classification", "object": "chat.completion", "created": 1, "model": "page-model",
					"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
					"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 12, "total_tokens": 22},
				}
				if err := json.MarshalWrite(w, response); err != nil {
					t.Error(err)
				}
			}))
			t.Cleanup(server.Close)
			provider, err := agent.NewOllamaProvider(server.URL+"/v1", "", "local")
			if err != nil {
				t.Fatal(err)
			}
			generator, err := sidecar.NewSidecar(t.Context(), provider, "page-model")
			if err != nil {
				t.Fatal(err)
			}
			c := newTestCoordinator(t)
			c.sidecarInst, c.sidecarModel, c.sidecarInit = generator, "page-model", true
			c.decisionPrimitiveGate = make(chan struct{}, 1)
			c.initEventStore()
			page := browser.Snapshot{URL: "https://example.test/source", Text: "Already readable public article", CapturedAt: time.Now().UTC()}
			for range 2 {
				state, err := c.classifyChromePage(t.Context(), page)
				if scenario == "separate_reasoning" {
					if err != nil || state != mcp.ChromePageReadable {
						t.Fatalf("valid provider JSON was changed: state=%s error=%v", state, err)
					}
				} else if err == nil || state != mcp.ChromePageUncertain || !strings.Contains(err.Error(), "decision_invalid_backend_output") {
					t.Fatalf("provider prose was accepted as a decision: state=%s error=%v", state, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("replay repeated inference: calls=%d", calls.Load())
			}
		})
	}
}
