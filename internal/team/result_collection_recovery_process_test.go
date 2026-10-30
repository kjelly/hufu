package team

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
