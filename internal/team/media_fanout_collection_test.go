package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeMediaFanoutBlockedCollectionRetainsWorkAcrossProcesses(t *testing.T) {
	const rootEnv = "HUFU_NATIVE_FANOUT_COLLECTION_ROOT"
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	configure := func(session *TeamSession) {
		configureNativeMediaProcessResume(session, true)
		worker := session.Agents["worker"]
		worker.Tools, worker.ResultTransport, worker.MaxSteps = "view", agent.ResultTransportRuntime, 1
	}
	if root := os.Getenv(rootEnv); root != "" {
		checkpoint := LoadSession(filepath.Join(root, "control"))
		if checkpoint == nil || len(checkpoint.Tasks) != 1 {
			t.Fatal("missing sealed fanout checkpoint")
		}
		c := newMediaPolicyCoordinatorAtRoot(t, root, os.Getenv("HUFU_NATIVE_FANOUT_COLLECTION_ENDPOINT"), factory, providerintrospection.CapabilityYes, configure)
		c.SetSessionData(checkpoint)
		ctx := openMediaInvocation(t, c)
		assessment, err := c.AssessResultCollectionRecovery(ctx, checkpoint.Tasks[0].ID)
		if err != nil || assessment.Disposition != RecoveryBlocked || assessment.ContinuesWorkflow {
			t.Fatal("unsupported fanout collection acquired recovery authority", assessment, err)
		}
		if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(ctx, checkpoint.Tasks[0].ID)); err == nil {
			t.Fatal("unsupported fanout collection acquired an operator grant")
		}
		if count, err := c.ResumeInterruptedTasks(ctx); err != nil || count != 0 {
			t.Fatalf("blocked fanout resumed work: count=%d err=%v", count, err)
		}
		item := c.todoItemByID(checkpoint.Tasks[0].ID)
		if item == nil || item.Status != TaskBlocked || c.nativeMediaAcceptedWork(item) || len(item.ExecutionReceipts) != 2 {
			t.Fatal("blocked fanout fabricated accepted leaf results")
		}
		for _, receipt := range item.ExecutionReceipts {
			if receipt.ResultCollection == nil || !receipt.ResultCollection.FanoutLeaf || receipt.ResultCollection.TurnsUsed != 1 || receipt.AcceptedResult != nil {
				t.Fatal("sealed work or collector budget was replaced")
			}
		}
		c.saveCheckpoint()
		c.finalizePublicInvocationFailure(context.Canceled)
		return
	}
	type wireRequest struct {
		Model    string                   `json:"model"`
		Messages []mediaWorkerWireMessage `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	requests := make(chan wireRequest, 8)
	started := make(chan struct{}, 2)
	var recovering atomic.Bool
	paths := make(chan string, 1)
	removeSource := sync.OnceFunc(func() {
		if err := os.Remove(<-paths); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request wireRequest
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil ||
			(request.Model != "vision" && request.Model != "next") {
			http.Error(w, "invalid collection fixture", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			http.Error(w, "unexpected repeated work", http.StatusBadRequest)
			return
		}
		wire, err := json.Marshal(request.Messages)
		if err != nil {
			http.Error(w, "invalid messages", http.StatusBadRequest)
			return
		}
		observation := "SEALED_NATIVE_OBSERVATION_" + request.Model
		if len(nativeRepairImageTurns(t, request.Messages)) > 0 {
			if recovering.Load() {
				t.Error("result-only recovery repeated image analysis")
			}
			removeSource()
			writeNativeRepairStream(t, w, map[string]any{"role": "assistant", "content": observation}, "stop", request.Model)
			return
		}
		if recovering.Load() {
			t.Error("blocked recovery invoked a collector")
			http.Error(w, "blocked recovery", http.StatusForbidden)
			return
		}
		if !bytes.Contains(wire, []byte(observation)) || bytes.Contains(wire, []byte("base64,")) {
			t.Error("collector lost original text evidence or inherited pixels")
		}
		for _, tool := range request.Tools {
			if tool.Function.Name != "return_result" {
				t.Error("collector gained an execution tool")
			}
		}
		started <- struct{}{}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, configure)
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	paths <- path
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var watchers sync.WaitGroup
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
	_, runErr := c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Compare the images.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path, path}})
	cancel()
	watchers.Wait()
	items := c.TaskTracker().TodoList().Items()
	if runErr == nil || len(items) != 1 || items[0].Status == TaskDone || len(items[0].ExecutionReceipts) != 2 || len(requests) != 4 {
		for _, item := range items {
			t.Logf("interrupted fanout status=%s detail=%s receipts=%d", item.Status, item.Detail, len(item.ExecutionReceipts))
		}
		t.Fatalf("fanout did not seal both original work attempts: calls=%d err=%v", len(requests), runErr)
	}
	for _, receipt := range items[0].ExecutionReceipts {
		if receipt.ResultCollection == nil || !receipt.ResultCollection.FanoutLeaf || receipt.ResultCollection.TurnsUsed != 1 || receipt.AcceptedResult != nil {
			t.Fatal("interruption lost sealed work or fabricated completion")
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	recovering.Store(true)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		before := LoadSession(c.session.Workspace)
		priorReceipts, err := json.Marshal(before.Tasks[0].ExecutionReceipts, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		childCtx, childCancel := context.WithTimeout(t.Context(), 45*time.Second)
		command := exec.CommandContext(childCtx, executable, "-test.run=^TestNativeMediaFanoutBlockedCollectionRetainsWorkAcrossProcesses$", "-test.count=1")
		command.Env = append(os.Environ(), rootEnv+"="+c.session.Dir, "HUFU_NATIVE_FANOUT_COLLECTION_ENDPOINT="+server.URL+"/v1")
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		err = command.Run()
		childCancel()
		if err != nil {
			t.Fatalf("new-process blocked collection failed: %v\n%s", err, output.String())
		}
		if len(requests) != 4 {
			t.Fatalf("blocked collection recovery repeated work: calls=%d want=4", len(requests))
		}
		after := LoadSession(c.session.Workspace)
		afterReceipts, err := json.Marshal(after.Tasks[0].ExecutionReceipts, json.Deterministic(true))
		if err != nil || !bytes.Equal(priorReceipts, afterReceipts) {
			t.Fatal("sealed leaf receipts changed during readmission", err)
		}
	}
	models := make(map[string]int)
	images := make(map[string]int)
	for len(requests) > 0 {
		request := <-requests
		models[request.Model]++
		turns := nativeRepairImageTurns(t, request.Messages)
		if len(turns) > 0 {
			images[request.Model]++
			assertMediaWorkerWire(t, turns, items[0].MediaBinding)
		}
	}
	for _, model := range []string{"vision", "next"} {
		if models[model] != 2 || images[model] != 1 {
			t.Fatal("collection recovery changed native work/collector counts", models, images)
		}
	}
	events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}
