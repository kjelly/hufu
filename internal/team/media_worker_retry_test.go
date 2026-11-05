package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
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
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeWorkerAutomaticRetryUsesCanonicalImagesAfterSourceRemoval(t *testing.T) {
	for _, mode := range []string{TaskOutputModeSummary, TaskOutputModeVerbatim} {
		t.Run(mode, func(t *testing.T) { testNativeWorkerAutomaticRetry(t, mode) })
	}
}

func testNativeWorkerAutomaticRetry(t *testing.T, outputMode string) {
	const failureEvidence = "OFFLINE_NATIVE_RATE_LIMIT_EVIDENCE"
	type observedRequest struct {
		Attempt  int
		Messages []mediaWorkerWireMessage
	}
	requests := make(chan observedRequest, 16)
	var c *Coordinator
	var removed atomic.Bool
	var source string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                   `json:"model"`
			Messages []mediaWorkerWireMessage `json:"messages"`
		}
		if json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || request.Model != "vision" {
			http.Error(w, "invalid native retry request", http.StatusBadRequest)
			return
		}
		events, err := c.EventJournal().ReadEvents(t.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		attempt := 0
		for _, event := range events {
			if event.Type == string(EventMediaProviderAdmitted) {
				admission, err := decodeMediaProviderEvent(event)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				attempt = admission.Attempt
			}
		}
		select {
		case requests <- observedRequest{Attempt: attempt, Messages: request.Messages}:
		default:
			http.Error(w, "retry count exceeded fixture budget", http.StatusBadRequest)
			return
		}
		if removed.CompareAndSwap(false, true) {
			if err := os.Remove(source); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.001")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, failureEvidence)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		arguments := `{"status":"success","summary":"Retry comparison.","details":"Images recovered from canonical CAS."}`
		fmt.Fprintf(w, "data: {\"id\":\"native-retry\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-retry\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"native-retry\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c = newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		session.Agents["worker"].MaxRetries = 1
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
	})
	source = filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(source, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Compare the images.", OutputMode: outputMode, Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{source, source}})
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || !removed.Load() {
		for _, item := range items {
			t.Logf("retry task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
		}
		events, _ := c.EventJournal().ReadEvents(t.Context())
		for index, event := range events {
			if event.Type != string(EventTaskStarted) {
				continue
			}
			var payload struct {
				Status   TaskStatus `json:"status"`
				Revision int        `json:"occurrence_revision"`
				Attempt  int        `json:"attempt"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			projected, _ := ReplayTodoList(events[:index+1])
			if current := todoItemByID(projected, event.TaskID); current != nil {
				t.Logf("task start payload=%+v replayed status=%s revision=%d dispatch=%s", payload, current.Status, current.OccurrenceRevision, current.DispatchID)
			}
		}
		t.Logf("observed provider requests=%d", len(requests))
		t.Fatalf("native automatic retry did not complete: tasks=%+v err=%v", items, runErr)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("retry fixture source was not removed")
	}
	seen := map[int]int{}
	for len(requests) > 0 {
		request := <-requests
		seen[request.Attempt]++
		if request.Attempt == 1 {
			assertMediaWorkerWire(t, request.Messages, items[0].MediaBinding)
			continue
		}
		if request.Attempt != 2 {
			t.Fatal("native retry escaped its attempt budget")
		}
		encoded, err := json.Marshal(request.Messages)
		if err != nil || !bytes.Contains(encoded, []byte(failureEvidence)) {
			t.Fatal("retry lost its canonical failure evidence", err)
		}
		// The retry prompt repeats the goal as part of its new instruction;
		// image bytes and their original user turn must still appear once.
		assertMediaWorkerWireForPrompt(t, request.Messages, items[0].MediaBinding, failureEvidence)
	}
	if seen[1] == 0 || seen[2] != 1 {
		t.Fatal("fixture did not exercise the real worker retry loop", seen)
	}
	if receipt := items[0].ExecutionReceipt; receipt == nil || receipt.Attempt != 2 || receipt.MediaInputDigest != items[0].MediaInputDigest {
		t.Fatal("automatic retry lost its canonical input receipt")
	}
	if result := c.LastRunResult(); result == nil || result.Outcome != RunOutcomeUnverified || result.GoalSatisfied || result.Response == "" {
		t.Fatal("successful retry changed whole-run acceptance")
	}
	result := c.GetTaskResult(items[0].ID)
	if result == nil || result.Attempt != 2 || !strings.Contains(result.Summary, "Retry comparison") {
		t.Fatal("retry lost its accepted second-attempt result")
	}
	if outputMode == TaskOutputModeVerbatim && (result.RawOutputRef == nil || !strings.Contains(c.LastRunResult().Response, "artifact_ref=")) {
		t.Fatal("retry did not finalize its bound transcript")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.TaskID != items[0].ID || event.Type != string(EventTaskStarted) && event.Type != string(EventTaskFailed) {
			continue
		}
		var lifecycle struct {
			Status     TaskStatus `json:"status"`
			DispatchID string     `json:"dispatch_id"`
		}
		if err := json.Unmarshal(event.Payload, &lifecycle); err != nil {
			t.Fatal(err)
		}
		if lifecycle.DispatchID != "" && lifecycle.Status != TaskInProgress {
			t.Fatal("worker lease was published before its running transition")
		}
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	assertNativeMediaCost(t, events, seen[1]+seen[2])
}
