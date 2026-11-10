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
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestNativeMediaResumeInNewProcessUsesCanonicalCAS(t *testing.T) {
	const childRoot = "HUFU_NATIVE_MEDIA_RESUME_ROOT"
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	configure := func(session *TeamSession) {
		configureNativeMediaProcessResume(session, os.Getenv("HUFU_NATIVE_MEDIA_RESUME_FANOUT") == "1")
	}
	if root := os.Getenv(childRoot); root != "" {
		endpoint := os.Getenv("HUFU_NATIVE_MEDIA_RESUME_ENDPOINT")
		checkpoint := LoadSession(filepath.Join(root, "control"))
		if checkpoint == nil || len(checkpoint.Tasks) != 1 {
			t.Fatal("missing durable interrupted task")
		}
		c := newMediaPolicyCoordinatorAtRoot(t, root, endpoint, factory, providerintrospection.CapabilityYes, configure)
		c.SetSessionData(checkpoint)
		ctx := openMediaInvocation(t, c)
		if os.Getenv("HUFU_NATIVE_MEDIA_RESUME_RETRY") == "1" {
			if err := c.CommitTaskResetForRetry(ctx, checkpoint.Tasks[0].ID, "retry original native input"); err != nil {
				t.Fatal("new process retry reset", err)
			}
		}
		count, err := c.ResumeInterruptedTasks(ctx)
		if err != nil || count != 1 {
			for _, item := range c.TaskTracker().TodoList().Items() {
				t.Logf("resumed status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
			}
			t.Fatalf("new process resume: count=%d err=%v", count, err)
		}
		item := c.todoItemByID(checkpoint.Tasks[0].ID)
		if item == nil || item.Status != TaskDone || item.ExecutionReceipt == nil || item.ExecutionReceipt.MediaInputDigest != item.MediaInputDigest {
			t.Fatalf("resumed task lost completion or canonical receipt: %+v", item)
		}
		if nativeMediaHasModelFanout(item) && !c.nativeMediaAcceptedWork(item) {
			t.Fatal("resumed fanout lost accepted leaf results")
		}
		c.saveCheckpoint()
		return
	}
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			runNativeMediaRecoveryProcess(t, factory, configure, retry, false)
		})
	}
}

func TestNativeMediaFanoutResumeInNewProcessUsesCanonicalCAS(t *testing.T) {
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	configure := func(session *TeamSession) { configureNativeMediaProcessResume(session, true) }
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			runNativeMediaRecoveryProcess(t, factory, configure, retry, true)
		})
	}
}

func configureNativeMediaProcessResume(session *TeamSession, fanout bool) {
	session.Agents["worker"].Tools = "submit_result"
	session.Agents["worker"].ResultTransport = agent.ResultTransportTool
	session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
	if !fanout {
		return
	}
	session.Agents["worker"].ExtraModels = []string{"fixture/next"}
	provider := session.Config.Providers["fixture"]
	provider.MaxConcurrent = 2
	session.Config.Providers["fixture"] = provider
	session.MediaPolicy.AllowedTargets = append(session.MediaPolicy.AllowedTargets, media.Target{
		Backend: "fixture", ProviderKey: "fixture", Model: "next", ImageTokenUpperBound: 4096,
	})
}

func runNativeMediaRecoveryProcess(t *testing.T, factory agent.ProviderBoundaryFactory, configure func(*TeamSession), retry, fanout bool) {
	t.Helper()
	type observedRequest struct {
		Model    string
		Messages []mediaWorkerWireMessage
	}
	requests := make(chan observedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                   `json:"model"`
			Messages []mediaWorkerWireMessage `json:"messages"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil ||
			(request.Model != "vision" && (!fanout || request.Model != "next")) {
			http.Error(w, "invalid native resume request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- observedRequest{Model: request.Model, Messages: request.Messages}:
		default:
			http.Error(w, "unexpected repeat", http.StatusBadRequest)
			return
		}
		writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{
			"status": "success", "summary": "Offline resumed comparison.", "details": "Canonical images inspected by " + request.Model + ".",
		})
	}))
	t.Cleanup(server.Close)
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, configure)
	ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
	task.Execution.RequiresResult = true
	if fanout {
		task.ModelTopology = initialTaskModelTopology(c.session.Agents["worker"], "fixture/vision")
	}
	item := createMediaOccurrence(t, c, ctx, task)
	binding := item.MediaBinding.Clone()
	if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "interrupted before provider", "", nil); err != nil {
		t.Fatal(err)
	}
	c.saveCheckpoint()
	c.finalizePublicInvocationFailure(context.Canceled)
	end()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.session.Workspace, "input.png")); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, executable, "-test.run=^TestNativeMediaResumeInNewProcessUsesCanonicalCAS$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HUFU_NATIVE_MEDIA_RESUME_ROOT="+c.session.Dir, "HUFU_NATIVE_MEDIA_RESUME_ENDPOINT="+server.URL+"/v1")
	if fanout {
		cmd.Env = append(cmd.Env, "HUFU_NATIVE_MEDIA_RESUME_FANOUT=1")
	}
	wantRetries := 0
	if retry {
		cmd.Env = append(cmd.Env, "HUFU_NATIVE_MEDIA_RESUME_RETRY=1")
		wantRetries = 1
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		for len(requests) > 0 {
			for _, message := range (<-requests).Messages {
				if message.Role == "tool" {
					t.Logf("offline resume tool response: %.2000s", message.Content)
				}
			}
		}
		t.Fatalf("resume child failed: %v\n%s", err, output.String())
	}
	wantCalls := 1
	if fanout {
		wantCalls = 2
	}
	if len(requests) != wantCalls {
		t.Fatal("resume did not execute exactly its admitted native models")
	}
	seen := make(map[string]bool)
	for range wantCalls {
		request := <-requests
		if seen[request.Model] {
			t.Fatal("resume repeated a native model")
		}
		seen[request.Model] = true
		assertMediaWorkerWireForPrompt(t, request.Messages, binding, "compare image")
	}
	resumed := LoadSession(c.session.Workspace)
	if resumed == nil || len(resumed.Tasks) != 1 || resumed.Tasks[0].MediaBinding.Hash != binding.Hash || resumed.Tasks[0].MediaBinding.RunID != binding.RunID || resumed.Tasks[0].Retries != wantRetries {
		t.Fatal("new process recreated its original media owner or attempt")
	}
	if len(resumed.Tasks[0].ExecutionReceipts) != wantCalls {
		t.Fatal("resume lost independent native receipts")
	}
	if fanout && !c.nativeMediaAcceptedWork(resumed.Tasks[0]) {
		t.Fatal("new-process checkpoint lost accepted leaf evidence")
	}
	events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal("resumed SDK request failed independent audit", err)
	}
}
