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
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeMediaFanoutRetriesAndRepairsKeepIndependentEvidence(t *testing.T) {
	for _, recovery := range []string{"worker_retry", "result_repair"} {
		for _, outputMode := range []string{TaskOutputModeSummary, TaskOutputModeVerbatim} {
			t.Run(recovery+"/"+outputMode, func(t *testing.T) {
				testNativeMediaFanoutRecovery(t, recovery, outputMode)
			})
		}
	}
}

func testNativeMediaFanoutRecovery(t *testing.T, recovery, outputMode string) {
	t.Helper()
	const failureEvidence = "NATIVE_FANOUT_RATE_LIMIT_EVIDENCE"
	type wireRequest struct {
		Model    string                   `json:"model"`
		Messages []mediaWorkerWireMessage `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	type observedRequest struct {
		wireRequest
		Attempt int
	}
	requests := make(chan observedRequest, 32)
	ready := make(chan *Coordinator, 1)
	coordinator := sync.OnceValue(func() *Coordinator { return <-ready })
	paths := make(chan string, 1)
	removeSource := sync.OnceFunc(func() {
		if err := os.Remove(<-paths); err != nil {
			t.Error(err)
		}
	})
	var mu sync.Mutex
	modelCalls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request wireRequest
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" ||
			json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || (request.Model != "vision" && request.Model != "next") {
			http.Error(w, "invalid fanout recovery request", http.StatusBadRequest)
			return
		}
		events, err := coordinator().EventJournal().ReadEvents(t.Context())
		if err != nil {
			http.Error(w, "journal unavailable", http.StatusBadRequest)
			return
		}
		attempt := 0
		for _, event := range events {
			if event.Type != string(EventMediaProviderAdmitted) {
				continue
			}
			admission, err := decodeMediaProviderEvent(event)
			if err != nil {
				http.Error(w, "invalid admission", http.StatusBadRequest)
				return
			}
			if admission.Target.Model == request.Model {
				attempt = admission.Attempt
			}
		}
		select {
		case requests <- observedRequest{wireRequest: request, Attempt: attempt}:
		default:
			http.Error(w, "request budget exceeded", http.StatusBadRequest)
			return
		}
		removeSource()
		mu.Lock()
		modelCalls[request.Model]++
		call := modelCalls[request.Model]
		mu.Unlock()
		if recovery == "worker_retry" && attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "0.001")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, failureEvidence)
			return
		}
		observation := "INDEPENDENT_NATIVE_OBSERVATION_" + request.Model
		if recovery == "result_repair" && call == 1 {
			writeNativeRepairStream(t, w, map[string]any{"role": "assistant", "content": observation}, "stop", request.Model)
			return
		}
		writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{
			"status": "success", "summary": "Independent recovery.", "details": observation,
		})
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
		worker.ExtraModels = []string{"fixture/next"}
		if recovery == "worker_retry" {
			worker.MaxRetries = 1
		} else {
			worker.MaxSteps = 1
		}
		provider := session.Config.Providers["fixture"]
		provider.MaxConcurrent = 2
		session.Config.Providers["fixture"] = provider
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
		session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets, media.Target{
			Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 4096,
		})
	})
	ready <- c
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	paths <- path
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Compare the images.", OutputMode: outputMode,
		Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path, path}})
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || c.LastRunResult() == nil || !c.LastRunResult().GoalSatisfied {
		for _, item := range items {
			t.Logf("fanout status=%s detail=%s", item.Status, item.Detail)
		}
		t.Fatalf("fanout recovery did not complete: calls=%d err=%v", len(requests), runErr)
	}
	seen := make(map[string]map[int]int)
	for len(requests) > 0 {
		request := <-requests
		if seen[request.Model] == nil {
			seen[request.Model] = make(map[int]int)
		}
		seen[request.Model][request.Attempt]++
		imageTurns := nativeRepairImageTurns(t, request.Messages)
		wire, err := json.Marshal(request.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if recovery == "worker_retry" {
			if request.Attempt != 1 && request.Attempt != 2 {
				t.Fatal("retry escaped its admitted attempts")
			}
			if request.Attempt == 1 {
				assertMediaWorkerWire(t, request.Messages, items[0].MediaBinding)
			} else {
				assertMediaWorkerWireForPrompt(t, request.Messages, items[0].MediaBinding, failureEvidence)
			}
		} else if len(imageTurns) > 0 {
			assertMediaWorkerWire(t, imageTurns, items[0].MediaBinding)
		} else {
			other := "vision"
			if request.Model == "vision" {
				other = "next"
			}
			if bytes.Contains(wire, []byte("base64,")) || !bytes.Contains(wire, []byte("INDEPENDENT_NATIVE_OBSERVATION_"+request.Model)) ||
				bytes.Contains(wire, []byte("INDEPENDENT_NATIVE_OBSERVATION_"+other)) || len(request.Tools) != 1 || request.Tools[0].Function.Name != "submit_result" {
				t.Fatal("repair inherited pixels, execution tools, or another leaf's semantic result")
			}
		}
	}
	accepted := make(map[string]bool)
	for _, receipt := range items[0].ExecutionReceipts {
		if receipt.AcceptedResult == nil {
			continue
		}
		if accepted[receipt.ModelExecutionID] || receipt.MediaInputDigest != items[0].MediaInputDigest ||
			receipt.AcceptedResult.Details != "INDEPENDENT_NATIVE_OBSERVATION_"+receipt.ExecutionTarget.Model {
			t.Fatal("accepted recovery lost original leaf identity or semantic result")
		}
		accepted[receipt.ModelExecutionID] = true
		if recovery == "worker_retry" && receipt.Attempt != 2 {
			t.Fatal("retry accepted a failed attempt")
		}
		if recovery == "result_repair" && (receipt.Attempt != 1 || receipt.RepairProvenance == nil || !receipt.RepairProvenance.Success) {
			t.Fatal("repair replaced its original attempt or provenance")
		}
		if outputMode == TaskOutputModeVerbatim && (receipt.AcceptedResult.RawOutputRef == nil || !strings.Contains(c.LastRunResult().Response, "artifact_ref=")) {
			t.Fatal("fanout recovery lost its original transcript")
		}
	}
	if len(accepted) != 2 || len(seen) != 2 || !c.nativeMediaAcceptedWork(items[0]) {
		t.Fatal("fanout recovery omitted a leaf")
	}
	wantReceipts := 2
	for _, model := range []string{"vision", "next"} {
		if recovery == "worker_retry" {
			wantReceipts = 4
			if seen[model][1] == 0 || seen[model][2] != 1 {
				t.Fatal("fanout did not retain each model's failed and successful attempts", seen)
			}
		} else if seen[model][1] != 2 || len(seen[model]) != 1 {
			t.Fatal("result-only repair repeated work or changed its original attempt", seen)
		}
	}
	if len(items[0].ExecutionReceipts) != wantReceipts {
		t.Fatal("fanout discarded an original work receipt")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	if recovery == "worker_retry" {
		assertNativeMediaCost(t, events, 4)
	} else {
		assertNativeMediaCost(t, events, 4, cost.PurposeRepair, cost.PurposeRepair)
	}
	replayed := todoItemByID(ReduceToTodoList(events), items[0].ID)
	if replayed == nil || !c.nativeMediaAcceptedWork(replayed) {
		t.Fatal("canonical replay lost accepted recovery evidence")
	}
}
