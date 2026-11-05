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
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
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
		c.saveCheckpoint()
		return
	}
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%t", retry), func(t *testing.T) {
			runNativeMediaRecoveryProcess(t, factory, configure, retry)
		})
	}
}

func runNativeMediaRecoveryProcess(t *testing.T, factory agent.ProviderBoundaryFactory, configure func(*TeamSession), retry bool) {
	t.Helper()
	requests := make(chan []mediaWorkerWireMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                   `json:"model"`
			Messages []mediaWorkerWireMessage `json:"messages"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || request.Model != "vision" {
			http.Error(w, "invalid native resume request", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request.Messages:
		default:
			http.Error(w, "unexpected repeat", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		arguments := `{"status":"success","summary":"Offline resumed comparison.","details":"Canonical images inspected by the offline fixture."}`
		fmt.Fprintf(w, "data: {\"id\":\"media-resume\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-resume\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", arguments)
		_, _ = io.WriteString(w, "data: {\"id\":\"media-resume\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
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
	wantRetries := 0
	if retry {
		cmd.Env = append(cmd.Env, "HUFU_NATIVE_MEDIA_RESUME_RETRY=1")
		wantRetries = 1
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		for len(requests) > 0 {
			for _, message := range <-requests {
				if message.Role == "tool" {
					t.Logf("offline resume tool response: %.2000s", message.Content)
				}
			}
		}
		t.Fatalf("resume child failed: %v\n%s", err, output.String())
	}
	if len(requests) != 1 {
		t.Fatal("resume did not execute exactly one native request")
	}
	assertMediaWorkerWireForPrompt(t, <-requests, binding, "compare image")
	resumed := LoadSession(c.session.Workspace)
	if resumed == nil || len(resumed.Tasks) != 1 || resumed.Tasks[0].MediaBinding.Hash != binding.Hash || resumed.Tasks[0].MediaBinding.RunID != binding.RunID || resumed.Tasks[0].Retries != wantRetries {
		t.Fatal("new process recreated its original media owner or attempt")
	}
	events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal("resumed SDK request failed independent audit", err)
	}
}
