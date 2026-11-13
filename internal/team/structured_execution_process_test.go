package team

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/providerproxy"
)

// A killed child and a fresh store, rather than reused process-local maps,
// establish the recovery boundary. All side effects belong to the fixture.
func TestStructuredExecutionCrashRecovery(t *testing.T) {
	const childRoot = "HUFU_STRUCTURED_RECOVERY_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		runStructuredRecoveryChild(t, root, os.Getenv("HUFU_STRUCTURED_RECOVERY_BOUNDARY"))
		return
	}
	for _, tc := range []struct {
		boundary            string
		wantMutations       int
		wantStatus          TaskStatus
		wantMutationReceipt bool
	}{
		{"before_mutation", 0, TaskBlocked, false},
		{"effect_without_settlement", 1, TaskBlocked, false},
		{"reconcile_effect_without_settlement", 1, TaskBlocked, false},
		{"receipt_before_followup", 1, TaskBlocked, true},
		{"completed", 1, TaskDone, true},
		{"validator_failure", 0, TaskError, false},
		{"repair_exhausted", 0, TaskError, false},
		{"cancelled", 0, TaskError, true},
		{"readonly_before_call", 0, TaskDone, false},
	} {
		t.Run(tc.boundary, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"control", "project"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			stopStructuredRecoveryChild(t, root, tc.boundary)
			beforeLog := readStructuredRecoveryLog(t, root)
			before := readStructuredRecoveryCount(t, root, "mutation")
			if before != tc.wantMutations {
				t.Fatalf("before mutations=%d want=%d", before, tc.wantMutations)
			}
			c := structuredRecoveryCoordinator(t, root)
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := ReplayTodoList(events)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%+v err=%v", tasks, err)
			}
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(tasks)
			original, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
			if err != nil {
				t.Fatal(err)
			}
			assertStructuredRecoveryFailures(t, tasks[0], original, tc.boundary)
			foundMutation := false
			for _, row := range original {
				foundMutation = foundMutation || row.Receipt.StepID == "mutate"
				if tc.boundary == "cancelled" && row.Receipt.StepID == "mutate" && (row.Receipt.ExitCode == 0 || row.Receipt.Stderr == "") {
					t.Fatal("cancelled mutation lost its failed-attempt evidence")
				}
			}
			if foundMutation != tc.wantMutationReceipt {
				t.Fatalf("mutation receipt=%t", foundMutation)
			}
			var completedResult []byte
			if tc.boundary == "completed" {
				if tasks[0].TypedResult == nil {
					t.Fatal("completed source lost its accepted result")
				}
				completedResult, err = json.Marshal(tasks[0].TypedResult, json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
			}
			c.SetStructuredStepRunner(structuredRecoveryRunner(t, c, root, ""))
			if _, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			current := todoByID(t, c, tasks[0].ID)
			if tc.boundary == "completed" {
				restoredResult, err := json.Marshal(current.TypedResult, json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
				if string(restoredResult) != string(completedResult) {
					t.Fatal("restart changed the completed canonical result")
				}
			}
			if current.Status != tc.wantStatus {
				t.Fatalf("status=%s detail=%s failure=%+v", current.Status, current.Detail, current.FailureEvent)
			}
			if got := readStructuredRecoveryCount(t, root, "mutation"); got != before {
				t.Fatalf("resume replayed external mutation: %d -> %d", before, got)
			}
			if tc.boundary == "readonly_before_call" && readStructuredRecoveryCount(t, root, "read") != 1 {
				t.Fatal("read-only task did not safely retry")
			}
			if tc.boundary != "readonly_before_call" && readStructuredRecoveryLog(t, root) != beforeLog {
				t.Fatal("recovery repeated producer, validator or side-effect work")
			}
			if current.Status == TaskBlocked {
				disposition := ReconcileOnly
				if tc.boundary == "reconcile_effect_without_settlement" {
					disposition = NeedsHuman
					if current.RecoveryState != RecoveryStateUnknown {
						t.Fatalf("unsettled external effect gained a known resolution: %s", current.RecoveryState)
					}
				}
				assertRecoveryFailureEvent(t, current, FailurePolicy, disposition)
			}
			if tc.boundary == "validator_failure" || tc.boundary == "repair_exhausted" {
				want := 1
				if tc.boundary == "repair_exhausted" {
					want = 2
				}
				if readStructuredRecoveryCount(t, root, "produce") != want || readStructuredRecoveryCount(t, root, "validate") != want {
					t.Fatal("validator failure escaped its declared producer repair budget")
				}
			}
			afterEvents, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			after, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, afterEvents)
			if err != nil {
				t.Fatal(err)
			}
			byID := make(map[string]ExecutionStepReceipt)
			for _, row := range after {
				byID[row.Receipt.ID] = row.Receipt
			}
			for _, row := range original {
				prior, err := json.Marshal(row.Receipt, json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
				restored, err := json.Marshal(byID[row.Receipt.ID], json.Deterministic(true))
				if err != nil {
					t.Fatal(err)
				}
				if string(prior) != string(restored) {
					t.Fatal("recovery changed historical receipt bytes")
				}
			}
		})
	}
}

func structuredRecoveryCoordinator(t *testing.T, root string) *Coordinator {
	t.Helper()
	c := newProcessRecoveryCoordinator(t, root, "http://127.0.0.1:1/v1", nil)
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	return c
}

func newProcessRecoveryCoordinator(t *testing.T, root, endpoint string, configure func(*Coordinator)) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t, withMaxRetries(0), withCoordinatorWorkspace(filepath.Join(root, "control")))
	c.projectDir = filepath.Join(root, "project")
	if err := c.session.SetCompatibilityWorkspaceScope(c.projectDir); err != nil {
		t.Fatal(err)
	}
	c.maxConcurrent = 1
	c.delegatedTasks = make(map[string]int)
	c.session.Config.DefaultLLMBackend = "ollama"
	c.session.Agents["worker"].Generation.Model = "ollama/test"
	manager, err := agent.NewProviderManagerWithBoundaryFactory(endpoint, "", nil, func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	})
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	if configure != nil {
		configure(c)
	}
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.initEventStore()
	return c
}

type structuredRecoveryCancelKey struct{}

func runStructuredRecoveryChild(t *testing.T, root, boundary string) {
	c := structuredRecoveryCoordinator(t, root)
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	effect := SideEffectExternalWrite
	var recovery RecoveryPolicy
	if boundary == "reconcile_effect_without_settlement" {
		recovery = RecoveryReconcile
	}
	if boundary == "readonly_before_call" {
		contract = ExecutionContract{Steps: []ExecutionStep{{ID: "read", Tool: "reader", Effect: ExecutionEffectRead}}}
		effect = SideEffectNone
	}
	if boundary == "repair_exhausted" {
		contract.Steps[1].OnFailure = StepFailureRepairable
		contract.Steps[1].MaxRepairs = 1
	}
	c.SetStructuredStepRunner(structuredRecoveryRunner(t, c, root, boundary))
	runCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runCtx = context.WithValue(runCtx, structuredRecoveryCancelKey{}, cancel)
	_, err := c.ExecuteTasks(runCtx, []TaskDef{{Agent: "worker", Goal: "exercise durable structured recovery", Model: "ollama/test", SideEffect: effect, Recovery: recovery, Execution: contract}})
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("no durable occurrence: %v", err)
	}
	if boundary == "completed" && (err != nil || items[0].Status != TaskDone) {
		t.Fatalf("completion: %+v %v", items[0], err)
	}
	if boundary == "validator_failure" || boundary == "repair_exhausted" || boundary == "cancelled" {
		if err == nil || items[0].Status == TaskDone {
			t.Fatalf("failure became success: %+v %v", items[0], err)
		}
	}
	structuredRecoveryBoundary(t, c)
}

func structuredRecoveryRunner(t *testing.T, c *Coordinator, root, boundary string) StructuredStepRunner {
	return StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		appendStructuredRecoveryCount(t, root, "step:"+request.Step.ID)
		switch request.Step.ID {
		case "produce":
			appendStructuredRecoveryCount(t, root, "produce")
			store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
			if err != nil {
				return ExecutionStepResult{}, err
			}
			artifact, err := store.Put(ctx, PutArtifactRequest{Path: "draft.txt", Content: []byte("approved fixture payload"), RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: "worker"})
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": artifact.ArtifactRef}}, err
		case "validate":
			appendStructuredRecoveryCount(t, root, "validate")
			if boundary == "validator_failure" || boundary == "repair_exhausted" {
				return ExecutionStepResult{ExitCode: 1, Stderr: "offline validator rejected original draft"}, errors.New("fixture validation failed")
			}
		case "mutate":
			if boundary == "before_mutation" {
				structuredRecoveryBoundary(t, c)
			}
			if boundary == "cancelled" {
				cancel := ctx.Value(structuredRecoveryCancelKey{}).(context.CancelFunc)
				cancel()
				return ExecutionStepResult{}, ctx.Err()
			}
			appendStructuredRecoveryCount(t, root, "mutation")
			if boundary == "effect_without_settlement" || boundary == "reconcile_effect_without_settlement" {
				structuredRecoveryBoundary(t, c)
			}
		case "verify":
			if boundary == "receipt_before_followup" {
				structuredRecoveryBoundary(t, c)
			}
		case "read":
			if boundary == "readonly_before_call" {
				structuredRecoveryBoundary(t, c)
			}
			appendStructuredRecoveryCount(t, root, "read")
		default:
			return ExecutionStepResult{}, fmt.Errorf("unexpected fixture step %s", request.Step.ID)
		}
		return ExecutionStepResult{Stdout: "complete offline fixture result"}, nil
	})
}

func structuredRecoveryBoundary(t *testing.T, c *Coordinator) {
	t.Helper()
	c.saveCheckpoint()
	if t.Failed() {
		t.Fatal("child fixture failed before the recovery boundary")
	}
	fmt.Println("STRUCTURED_RECOVERY_READY")
	var input [1]byte
	if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
		t.Fatal(err)
	}
	t.Fatal("parent must kill child at the handshake")
}

func stopStructuredRecoveryChild(t *testing.T, root, boundary string) {
	t.Helper()
	stopRecoveryProcess(t, root, "TestStructuredExecutionCrashRecovery", boundary)
}

func stopRecoveryProcess(t *testing.T, root, testName, boundary string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+testName+"$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HUFU_STRUCTURED_RECOVERY_ROOT="+root, "HUFU_STRUCTURED_RECOVERY_BOUNDARY="+boundary)
	cmd.Dir = filepath.Join(root, "project")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics strings.Builder
	cmd.Stderr = &diagnostics
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = input.Close() })
	t.Cleanup(stop)
	reader := bufio.NewReader(output)
	var payload string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			stop()
			t.Fatalf("child never reached %s: %v; %s", boundary, err, diagnostics.String())
		}
		if suffix, ok := strings.CutPrefix(strings.TrimSpace(line), "STRUCTURED_RECOVERY_READY"); ok {
			payload = strings.TrimSpace(suffix)
			break
		}
	}
	stop()
	if strings.Contains(diagnostics.String(), "WARNING: DATA RACE") {
		t.Fatalf("child reported a race: %s", diagnostics.String())
	}
	return payload
}

func appendStructuredRecoveryCount(t *testing.T, root, kind string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(root, "external-counts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprintln(file, kind)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func readStructuredRecoveryCount(t *testing.T, root, kind string) int {
	t.Helper()
	count := 0
	for line := range strings.SplitSeq(readStructuredRecoveryLog(t, root), "\n") {
		if line == kind {
			count++
		}
	}
	return count
}

func readStructuredRecoveryLog(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "external-counts"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestStructuredExecutionRecoveryRejectsChangedSources(t *testing.T) {
	for _, mode := range []string{"policy_drift", "missing_receipt", "corrupt_receipt", "sibling_branch"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "control")
			for _, name := range []string{"control", "project"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			boundary := "receipt_before_followup"
			if mode == "policy_drift" {
				boundary = "readonly_before_call"
			}
			stopStructuredRecoveryChild(t, root, boundary)
			beforeCounts := readStructuredRecoveryLog(t, root)
			store, err := OpenEventStore(workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			var originalReceipts []string
			var source structuredStepReceiptRecorded
			forkID := ""
			for _, event := range events {
				if event.Type == eventTypeForTaskStatus(TaskInProgress) && forkID == "" {
					forkID = event.ID
				}
				if event.Type == string(EventStructuredStepReceiptRecorded) {
					source, err = decodeStructuredStepReceiptEvent(event)
					if err != nil {
						t.Fatal(err)
					}
					originalReceipts = append(originalReceipts, source.ReceiptID)
				}
			}
			if mode != "policy_drift" && len(originalReceipts) == 0 {
				t.Fatal("child did not persist original receipts")
			}
			var damagedPath string
			switch mode {
			case "missing_receipt", "corrupt_receipt":
				damagedPath = filepath.Join(workspace, logsDir, "artifacts", "data", source.Artifact.ID)
				if mode == "missing_receipt" {
					err = os.Remove(damagedPath)
				} else {
					err = os.WriteFile(damagedPath, []byte("corrupt fixture receipt"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "sibling_branch":
				if forkID == "" {
					t.Fatal("no pre-step fork boundary")
				}
				tree, err := LoadSessionTree(workspace)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ForkSessionBranch(t.Context(), workspace, tree, store, "sibling-before-steps", forkID); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			var configure func(*Coordinator)
			if mode == "policy_drift" {
				configure = func(c *Coordinator) { c.session.Agents["worker"].Generation.Model = "ollama/changed" }
			}
			c := newProcessRecoveryCoordinator(t, root, "http://127.0.0.1:1/v1", configure)
			c.SetStructuredStepRunner(structuredRecoveryRunner(t, c, root, ""))
			selected, err := c.readActiveLineageEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := ReplayTodoList(selected)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%d err=%v", len(tasks), err)
			}
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(tasks)
			if mode == "sibling_branch" {
				for _, id := range originalReceipts {
					if _, found := c.executionStepReceiptRegistry().Get(id); found {
						t.Fatal("sibling gained a later receipt from the other branch")
					}
				}
				if err := c.AdmitExecutionPolicy(); err != nil {
					t.Fatal(err)
				}
				if _, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context())); err != nil {
					t.Fatal(err)
				}
				assertRecoveryFailureEvent(t, todoByID(t, c, tasks[0].ID), FailurePolicy, ReconcileOnly)
				if todoByID(t, c, tasks[0].ID).Status != TaskBlocked {
					t.Fatal("unproven sibling work became complete")
				}
			} else {
				if err := c.AdmitExecutionPolicy(); err == nil {
					t.Fatal("changed source passed startup admission")
				}
				if mode == "missing_receipt" || mode == "corrupt_receipt" {
					if !c.SessionData().RecoveryRequired {
						t.Fatal("damaged canonical evidence did not require recovery")
					}
					if _, found := c.executionStepReceiptRegistry().Get(source.ReceiptID); found {
						t.Fatal("damaged receipt gained authority")
					}
					raw, readErr := os.ReadFile(damagedPath)
					if mode == "missing_receipt" && !errors.Is(readErr, os.ErrNotExist) {
						t.Fatal("admission reconstructed missing historical evidence")
					}
					if mode == "corrupt_receipt" && (readErr != nil || string(raw) != "corrupt fixture receipt") {
						t.Fatal("admission rewrote corrupted historical evidence")
					}
				}
				if _, err := c.ExecuteTasks(t.Context(), []TaskDef{taskDefFromTodoItem(tasks[0])}); err == nil {
					t.Fatal("unadmitted task executed")
				}
			}
			if readStructuredRecoveryLog(t, root) != beforeCounts {
				t.Fatal("source rejection re-executed fixture work")
			}
			after, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			originals := make(map[string]RunEvent, len(after))
			for _, event := range after {
				originals[event.ID] = event
			}
			for _, event := range events {
				actual, found := originals[event.ID]
				if !found || actual.Hash != event.Hash || string(actual.Payload) != string(event.Payload) {
					t.Fatal("recovery rewrote original journal evidence")
				}
			}
		})
	}
}

func assertStructuredRecoveryFailures(t *testing.T, item *TodoItem, rows []StructuredStepEvidence, boundary string) {
	t.Helper()
	if item.Status != TaskError {
		return
	}
	// Structured task errors retain the existing generic execution classification;
	// the complete step receipt preserves validator and cancellation diagnostics.
	assertRecoveryFailureEvent(t, item, FailureExecution, RetryNone)
	if boundary != "validator_failure" && boundary != "repair_exhausted" {
		return
	}
	want := 1
	if boundary == "repair_exhausted" {
		want = 2
	}
	failed := 0
	for _, row := range rows {
		receipt := row.Receipt
		if receipt.StepID != "validate" {
			continue
		}
		if receipt.ExitCode == 0 || receipt.Stderr == "" || receipt.RepairAttempt != failed {
			t.Fatal("failed validator lost its complete bounded-attempt evidence")
		}
		failed++
	}
	if failed != want {
		t.Fatalf("failed validator receipts=%d want=%d", failed, want)
	}
}
