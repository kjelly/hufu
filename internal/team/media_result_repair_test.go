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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeWorkerResultOnlyRepairFinalizesOriginalAttempt(t *testing.T) {
	for _, mode := range []string{TaskOutputModeSummary, TaskOutputModeVerbatim} {
		t.Run(mode, func(t *testing.T) { testNativeResultOnlyRepair(t, mode) })
	}
}

func testNativeResultOnlyRepair(t *testing.T, outputMode string) {
	const observation = "CANONICAL_NATIVE_REPAIR_OBSERVATION"
	type repairRequest struct {
		Model    string                   `json:"model"`
		Messages []mediaWorkerWireMessage `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	requests := make(chan repairRequest, 4)
	var calls atomic.Int32
	var source string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request repairRequest
		if r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || request.Model != "vision" {
			http.Error(w, "invalid repair fixture request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			http.Error(w, "unexpected extra repair request", http.StatusBadRequest)
			return
		}
		if calls.Add(1) == 1 {
			if err := os.Remove(source); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeNativeRepairStream(t, w, map[string]any{"role": "assistant", "content": observation}, "stop")
			return
		}
		writeNativeRepairStream(t, w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"index": 0, "id": "submit-repair", "type": "function", "function": map[string]any{
				"name": "submit_result", "arguments": `{"status":"success","summary":"Canonical repair.","details":"CANONICAL_NATIVE_REPAIR_OBSERVATION"}`,
			},
		}}}, "tool_calls")
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		session.Agents["worker"].MaxSteps = 1
	})
	source = filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(source, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Compare the images.", OutputMode: outputMode,
		Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{source, source}})
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || len(requests) != 2 {
		for _, item := range items {
			t.Logf("repair task: status=%s detail=%s", item.Status, item.Detail)
		}
		t.Fatalf("native result-only repair did not finalize: calls=%d err=%v", len(requests), runErr)
	}
	worker, repair := <-requests, <-requests
	// Last-step instructions may repeat the goal as text. The original
	// image-bearing turn itself must still be unique, ordered and unchanged.
	assertMediaWorkerWire(t, nativeRepairImageTurns(t, worker.Messages), items[0].MediaBinding)
	encoded, err := json.Marshal(repair.Messages)
	if err != nil || bytes.Contains(encoded, []byte("image_url")) || bytes.Contains(encoded, []byte("base64,")) || !bytes.Contains(encoded, []byte(observation)) {
		t.Fatal("result-only repair lost text evidence or inherited image bytes", err)
	}
	if len(repair.Tools) != 1 || repair.Tools[0].Function.Name != "submit_result" {
		t.Fatal("result-only repair gained execution tools")
	}
	result := c.GetTaskResult(items[0].ID)
	if result == nil || result.Attempt != 1 || !strings.Contains(result.Details, observation) {
		t.Fatal("result-only repair changed its original attempt or observation")
	}
	if outputMode == TaskOutputModeVerbatim && (result.RawOutputRef == nil || !strings.Contains(c.LastRunResult().Response, "artifact_ref=")) {
		t.Fatal("result-only repair did not finalize its original transcript")
	}
	receipt := items[0].ExecutionReceipt
	if receipt == nil || receipt.Attempt != 1 || len(items[0].ExecutionReceipts) != 1 || receipt.MediaInputDigest != items[0].MediaInputDigest || receipt.RepairProvenance == nil || !receipt.RepairProvenance.Attempted {
		t.Fatal("result-only repair lost its original receipt or repair provenance")
	}
	if outcome := c.LastRunResult(); outcome == nil || outcome.GoalSatisfied || outcome.Outcome != RunOutcomeUnverified {
		t.Fatal("result-only repair invented whole-run acceptance")
	}
	c.conversationHistoryMu.Lock()
	history := cloneMessages(c.conversationHistory)
	c.conversationHistoryMu.Unlock()
	if len(historyReferences(history)) != 2 {
		t.Fatal("text-only repair rewrote original media history")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runs, imageGrants := 0, 0
	for _, event := range events {
		if event.Type == string(EventRunStarted) {
			runs++
		}
		if event.Type == string(EventMediaProviderAdmitted) {
			imageGrants++
		}
	}
	if runs != 1 || imageGrants != 1 {
		t.Fatal("result-only repair replayed image analysis or created another run")
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	accounting, err := costEventsFromRunEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	var purposes []cost.Purpose
	reservations := make(map[string]bool)
	settlements := make(map[string]bool)
	for _, event := range accounting {
		if reserved := event.Reservation; reserved != nil {
			if reserved.TaskID != items[0].ID || reserved.RunID != receipt.RunID || reserved.OccurrenceAttempt != 1 || reserved.EstimatedInputTokens <= 0 || reservations[reserved.ProviderInvocationID] {
				t.Fatal("repair cost reservation changed the original occurrence")
			}
			reservations[reserved.ProviderInvocationID] = true
			purposes = append(purposes, reserved.Purpose)
		}
		if settled := event.Settlement; settled != nil {
			if !reservations[settled.ProviderInvocationID] || settlements[settled.ProviderInvocationID] || settled.Usage != nil || settled.EstimateSource != cost.EstimateAdmissionBound {
				t.Fatal("repair lost its distinct settlement or fabricated actual usage")
			}
			settlements[settled.ProviderInvocationID] = true
		}
	}
	if !slices.Equal(purposes, []cost.Purpose{cost.PurposeWorker, cost.PurposeRepair}) || len(settlements) != 2 {
		t.Fatal("image analysis and text-only repair did not own separate accounting pairs", purposes)
	}
}

func nativeRepairImageTurns(t *testing.T, messages []mediaWorkerWireMessage) []mediaWorkerWireMessage {
	t.Helper()
	var turns []mediaWorkerWireMessage
	for _, message := range messages {
		content := bytes.TrimSpace(message.Content)
		if len(content) == 0 || content[0] != '[' {
			continue
		}
		var parts []mediaWorkerWirePart
		if err := json.Unmarshal(content, &parts); err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			if part.Type == "image_url" {
				turns = append(turns, message)
				break
			}
		}
	}
	return turns
}

func writeNativeRepairStream(t *testing.T, w http.ResponseWriter, delta map[string]any, finish string, models ...string) {
	t.Helper()
	model := "vision"
	if len(models) > 0 {
		model = models[0]
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for index, payload := range []map[string]any{delta, {}} {
		var reason any
		if index == 1 {
			reason = finish
		}
		data, err := json.Marshal(map[string]any{"id": "native-repair", "object": "chat.completion.chunk", "created": 1, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": payload, "finish_reason": reason}}})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
