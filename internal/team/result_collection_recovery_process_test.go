package team

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestResultCollectionRecoveryCompetingProcesses(t *testing.T) {
	const childKey = "HUFU_RECOVERY_CHILD_WORKSPACE"
	if workspace := os.Getenv(childKey); workspace != "" {
		c := newTestCoordinator(t)
		c.session.Workspace = workspace
		c.projectDir = os.Getenv("HUFU_RECOVERY_CHILD_PROJECT")
		if err := c.session.SetCompatibilityWorkspaceScope(c.projectDir); err != nil {
			t.Fatal(err)
		}
		c.maxConcurrent = 1
		c.session.Config.DefaultLLMBackend = execution.OllamaBackendName
		c.session.Agents["worker"].Generation.Model = "ollama/test"
		var err error
		c.providerManager, err = agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.executionPolicy, err = newExecutionPolicyState(c)
		if err != nil {
			t.Fatal(err)
		}
		c.initEventStore()
		events, err := c.EventJournal().ReadEvents(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := ReplayTodoList(events)
		if err != nil {
			t.Fatal(err)
		}
		c.taskTracker = NewTaskTracker()
		c.taskTracker.TodoList().AddReserved(tasks)
		id := os.Getenv("HUFU_RECOVERY_CHILD_TASK")
		a, err := c.AssessResultCollectionRecovery(t.Context(), id)
		if err != nil || a.Disposition != RecoveryCommitLocal {
			t.Fatalf("child assessment=%+v err=%v", a, err)
		}
		fmt.Println("RECOVERY_READY")
		var release [1]byte
		if _, err := io.ReadFull(os.Stdin, release[:]); err != nil {
			t.Fatal(err)
		}
		c.executionRunID = os.Getenv("HUFU_RECOVERY_CHILD_RUN")
		if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err == nil {
			fmt.Println("RECOVERY_ACCEPTED")
		} else {
			fmt.Println("RECOVERY_REFUSED")
		}
		return
	}
	c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"original limitation"}`, true, RepairFailureSource)
	workspace, project := c.session.Workspace, c.projectDir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	type child struct {
		cmd    *exec.Cmd
		input  io.WriteCloser
		output *bufio.Reader
	}
	children := make([]child, 0, 2)
	for i := range 2 {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestResultCollectionRecoveryCompetingProcesses$")
		cmd.Env = append(os.Environ(), childKey+"="+workspace, "HUFU_RECOVERY_CHILD_PROJECT="+project, "HUFU_RECOVERY_CHILD_TASK="+id, fmt.Sprintf("HUFU_RECOVERY_CHILD_RUN=process-%d", i))
		cmd.Stderr = os.Stderr
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		children = append(children, child{cmd, input, bufio.NewReader(output)})
	}
	for _, child := range children {
		line, err := child.output.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "RECOVERY_READY" {
			t.Fatalf("child not ready: %q %v", line, err)
		}
	}
	for _, child := range children {
		if _, err := child.input.Write([]byte("!")); err != nil {
			t.Fatal(err)
		}
		_ = child.input.Close()
	}
	accepted := 0
	for _, child := range children {
		line, err := child.output.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		switch strings.TrimSpace(line) {
		case "RECOVERY_ACCEPTED":
			accepted++
		case "RECOVERY_REFUSED":
		default:
			t.Fatalf("unexpected child result: %q", line)
		}
		if _, err := io.Copy(io.Discard, child.output); err != nil {
			t.Fatal(err)
		}
		if err := child.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted grants=%d", accepted)
	}
	store, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	item := todoItemByID(tasks, id)
	if item == nil || item.Status != TaskProtocolIncomplete || len(item.ExecutionReceipt.RepairProvenance.OperatorGrants) != 1 {
		t.Fatalf("competing recovery projection=%+v", item)
	}
}

// The original worker completes its external effect and the collector commits
// claims, but the child is killed before TaskDone. Only canonical bytes survive.
func TestResultCollectionCrashBeforeTaskCompletion(t *testing.T) {
	const childRoot = "HUFU_STRUCTURED_RECOVERY_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		runCollectionRecoveryChild(t, root)
		return
	}
	for _, mode := range []string{"intact", "missing_work", "mismatched_work"} {
		t.Run(mode, func(t *testing.T) { checkCollectionCrashRecovery(t, mode) })
	}
}

func checkCollectionCrashRecovery(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"control", "project"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	payload := stopRecoveryProcess(t, root, "TestResultCollectionCrashBeforeTaskCompletion", "collection")
	var handshake struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal([]byte(payload), &handshake, json.RejectUnknownMembers(true)); err != nil || handshake.Endpoint == "" {
		t.Fatalf("handshake=%q err=%v", payload, err)
	}
	c := newCollectionRecoveryCoordinator(t, root, handshake.Endpoint)
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%d err=%v", len(tasks), err)
	}
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(tasks)
	original := tasks[0]
	checkpoint := currentResultCollection(original)
	if original.Status != TaskInProgress || checkpoint == nil || checkpoint.State != ResultCollectionCommitted || original.TypedResult == nil {
		t.Fatalf("wrong interruption boundary: status=%s checkpoint=%+v", original.Status, checkpoint)
	}
	before, err := json.Marshal(original.TypedResult, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	workBefore, err := c.loadCollectionWork(t.Context(), original, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	originalReceipt, err := json.Marshal(workBefore.Receipt, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	beforeCounts := readStructuredRecoveryLog(t, root)
	if readStructuredRecoveryCount(t, root, "mutation") != 1 || readStructuredRecoveryCount(t, root, "collector") != 1 {
		t.Fatalf("unexpected original work: %s", beforeCounts)
	}
	worker := scriptWorker(failTurn(errors.New("worker replay is forbidden")))
	c.workerAgentOverride = worker
	if mode != "intact" {
		path := filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", checkpoint.Work.ArtifactID)
		if mode == "missing_work" {
			err = os.Remove(path)
		} else {
			err = os.WriteFile(path, []byte("mismatched fixture work"), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
		current := todoByID(t, c, original.ID)
		if err == nil || current.Status != TaskBlocked || worker.Calls() != 0 || readStructuredRecoveryLog(t, root) != beforeCounts {
			t.Fatalf("untrusted work resumed: status=%s worker=%d err=%v", current.Status, worker.Calls(), err)
		}
		assertRecoveryFailureEvent(t, current, FailureProtocol, NeedsHuman)
		if cp := currentResultCollection(current); cp == nil || cp.Work != checkpoint.Work || cp.TurnsUsed != checkpoint.TurnsUsed {
			t.Fatal("rejected recovery changed work identity or collector budget")
		}
		return
	}
	count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
	current := todoByID(t, c, original.ID)
	if err != nil || count != 1 || current.Status != TaskDone || worker.Calls() != 0 {
		t.Fatalf("resume count=%d status=%s worker=%d detail=%s err=%v", count, current.Status, worker.Calls(), current.Detail, err)
	}
	after, err := json.Marshal(current.TypedResult, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	restored := currentResultCollection(current)
	if string(before) != string(after) || restored.TurnsUsed != checkpoint.TurnsUsed || restored.Work != checkpoint.Work {
		t.Fatal("local completion changed canonical claims or spent another collection turn")
	}
	workAfter, err := c.loadCollectionWork(t.Context(), current, restored)
	if err != nil {
		t.Fatal(err)
	}
	receiptAfter, err := json.Marshal(workAfter.Receipt, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if string(originalReceipt) != string(receiptAfter) || string(workBefore.Report) != string(workAfter.Report) || readStructuredRecoveryLog(t, root) != beforeCounts {
		t.Fatal("recovery changed original work, receipt or external effects")
	}
	if count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context())); err != nil || count != 0 || worker.Calls() != 0 || readStructuredRecoveryLog(t, root) != beforeCounts {
		t.Fatalf("completed task was replayed: count=%d err=%v", count, err)
	}
}

func newCollectionRecoveryCoordinator(t *testing.T, root, endpoint string) *Coordinator {
	t.Helper()
	return newProcessRecoveryCoordinator(t, root, endpoint, func(c *Coordinator) {
		def := c.session.Agents["worker"]
		def.ResultTransport = agent.ResultTransportRuntime
		def.Generation.ContextWindow = 131072
		def.Generation.MaxTokens = "1024"
		c.modelProfileRuntime = NewModelProfileRuntime(c.providerManager, false)
		c.providerBoundaryStarted = true
		c.providerBoundaryStart = func(context.Context, string) error { return nil }
	})
}

func runCollectionRecoveryChild(t *testing.T, root string) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		assertCollectorResponseChannel(t, request)
		appendStructuredRecoveryCount(t, root, "collector")
		w.Header().Set("Content-Type", "text/event-stream")
		claims := `{"status":"success","summary":"original external fixture work is complete"}`
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"collected\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", claims)
	}))
	t.Cleanup(server.Close)
	endpoint := server.URL + "/v1"
	c := newCollectionRecoveryCoordinator(t, root, endpoint)
	c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		appendStructuredRecoveryCount(t, root, "mutation")
		return textAgentResult("Complete original external work report."), nil
	}})
	c.SetEventJournal(collectionCompletionPauseJournal{AtomicEventJournal: c.EventJournal().(AtomicEventJournal), pause: func(event RunEvent) {
		if event.Type != string(EventTaskCompleted) {
			return
		}
		if t.Failed() {
			t.Fatal("child fixture failed before collection commit")
		}
		raw, err := json.Marshal(struct {
			Endpoint string `json:"endpoint"`
		}{endpoint})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("STRUCTURED_RECOVERY_READY " + string(raw))
		var input [1]byte
		if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
			t.Fatal(err)
		}
		t.Fatal("parent must kill the child before TaskDone")
	}})
	_, err := c.ExecuteTasks(t.Context(), []TaskDef{{Agent: "worker", Goal: "complete original external fixture work", Model: "ollama/test", SideEffect: SideEffectExternalWrite, Execution: ExecutionContract{RequiresResult: true}}})
	t.Fatalf("child did not pause before TaskDone: %v", err)
}

type collectionCompletionPauseJournal struct {
	AtomicEventJournal
	pause func(RunEvent)
}

func (j collectionCompletionPauseJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	j.pause(event)
	return j.AtomicEventJournal.Append(ctx, event)
}
