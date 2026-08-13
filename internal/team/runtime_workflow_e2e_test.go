package team

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

func TestActionTelemetryAndContextPersistence_E2E(t *testing.T) {
	// 1. Setup a fake Ollama server to act as a mock provider
	step := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 1st request: Preparer (PhasePrepare) - calls a read-only tool
		// 2nd request: Preparer (PhasePrepare) - finishes
		// 3rd request: Executor (PhaseExecute) - calls a write tool
		// 4th request: Executor (PhaseExecute) - calls a write tool
		// 5th request: Executor (PhaseExecute) - finishes
		// 6th request: Verifier (PhaseVerify) - finishes
		var resp string
		switch step {
		case 0: // prepare read
			resp = `{"model":"test","message":{"role":"assistant","tool_calls":[{"function":{"name":"read_file","arguments":{"path":"test.txt"}}}]},"done":true}`
		case 1: // prepare finish
			resp = `{"model":"test","message":{"role":"assistant","content":"prepare done","tool_calls":[{"function":{"name":"finish","arguments":{"status":"success"}}}]},"done":true}`
		case 2: // audit finish
			resp = `{"model":"test","message":{"role":"assistant","content":"audit done","tool_calls":[{"function":{"name":"finish","arguments":{"status":"success"}}}]},"done":true}`
		case 3: // execute write
			resp = `{"model":"test","message":{"role":"assistant","tool_calls":[{"function":{"name":"write_file","arguments":{"path":"out1.txt"}}}]},"done":true}`
		case 4: // execute write 2
			resp = `{"model":"test","message":{"role":"assistant","tool_calls":[{"function":{"name":"write_file","arguments":{"path":"out2.txt"}}}]},"done":true}`
		case 5: // execute finish
			resp = `{"model":"test","message":{"role":"assistant","content":"execute done","tool_calls":[{"function":{"name":"finish","arguments":{"status":"success"}}}]},"done":true}`
		case 6: // verify finish
			resp = `{"model":"test","message":{"role":"assistant","content":"verify done","tool_calls":[{"function":{"name":"finish","arguments":{"status":"success"}}}]},"done":true}`
		default:
			resp = `{"model":"test","message":{"role":"assistant","content":"done","tool_calls":[{"function":{"name":"finish","arguments":{"status":"success"}}}]},"done":true}`
		}
		step++
		fmt.Fprintln(w, resp)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	ts := httptest.NewUnstartedServer(handler)
	ts.Listener = listener
	ts.Start()
	defer ts.Close()

	tmpDir, err := os.MkdirTemp("", "TestActionTelemetry_E2E")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	session := &TeamSession{
		Workspace: tmpDir,
		Config: agent.TeamConfig{
			Name: "test-team",
			Workflow: agent.WorkflowConfig{
				Phases: []string{string(PhasePrepare), string(PhaseAudit), string(PhaseExecute), string(PhaseVerify)},
			},
			Policies: agent.WorkflowPolicies{
				AllowPhaseSkip: true,
			},
		},
	}
	session.Agents = map[string]*agent.AgentDef{
		"preparer": {Role: "prepare", Generation: agent.GenerationParams{Model: "test"}},
		"auditor":  {Role: "audit", Generation: agent.GenerationParams{Model: "test"}},
		"executor": {Role: "execute", Generation: agent.GenerationParams{Model: "test"}},
		"verifier": {Role: "verify", Generation: agent.GenerationParams{Model: "test"}},
	}
	session.ProviderRegistry = NewProviderRegistry()
	session.ContractTasks = []TaskDef{
		{ContractID: "prepare", Phase: PhasePrepare, Agent: "preparer"},
		{ContractID: "audit", Phase: PhaseAudit, Agent: "auditor"},
		{ContractID: "execute", Phase: PhaseExecute, Agent: "executor"},
		{ContractID: "verify", Phase: PhaseVerify, Agent: "verifier"},
	}

	pm, _ := agent.NewProviderManager("", "", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: ts.URL},
	})
	c := &Coordinator{
		session:         session,
		providerManager: pm,
		taskTracker:     NewTaskTracker(),
	}
	es, err := NewEventStore(tmpDir, "run-e2e", "session-e2e")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = es

	phaseWorkflow, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	c.phaseWorkflow = phaseWorkflow

	c.reportStatus = func(event StatusEvent) {}

	// Need to initialize the execution event logger
	finalizeRun := c.beginExecutionRun()

	runID := c.executionRunID

	// Wait for workflow to finish
	_ = c.phaseWorkflow.Start()

	// Execute tool calls manually by executing tasks against the mock provider
	ctx := context.Background()
	if _, err := c.executeTask(ctx, TaskDef{Agent: "preparer", Phase: PhasePrepare, Goal: "test"}, "todo_1"); err != nil {
		t.Logf("todo_1 err: %v", err)
	}

	// Advance phase to Execute
	c.phaseWorkflow.mu.Lock()
	c.phaseWorkflow.state = PhaseExecute
	c.phaseWorkflow.mu.Unlock()

	if _, err := c.executeTask(ctx, TaskDef{Agent: "executor", Phase: PhaseExecute, Goal: "test2"}, "todo_2"); err != nil {
		t.Logf("todo_2 err: %v", err)
	}

	// Add one more action so we get 2 action_started and 2 action_completed
	if _, err := c.executeTask(ctx, TaskDef{Agent: "executor", Phase: PhaseExecute, Goal: "test3"}, "todo_3"); err != nil {
		t.Logf("todo_3 err: %v", err)
	}

	// Ensure we wait a moment
	time.Sleep(100 * time.Millisecond)
	finalizeRun()

	// 1. Verify Context Persistence
	contextFile := filepath.Join(tmpDir, "runtime", "receipts", runID+"-context.json")
	ctxData, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("failed to read context snapshot: %v", err)
	}
	if string(ctxData) == "" {
		t.Errorf("context snapshot is empty")
	}
	var persistedContext ExecutionContext
	if err := json.Unmarshal(ctxData, &persistedContext); err != nil {
		t.Fatalf("runtime context is not valid JSON: %v", err)
	}
	if err := persistedContext.Validate(); err != nil {
		t.Fatalf("persisted runtime context failed schema validation: %v", err)
	}
	if persistedContext.RunID != runID || persistedContext.CurrentPhase != PhasePrepare {
		t.Fatalf("persisted runtime context identity/phase = run_id %q, phase %q", persistedContext.RunID, persistedContext.CurrentPhase)
	}
	if persistedContext.RepositoryRoot != tmpDir || persistedContext.ArtifactPaths["receipts"] != filepath.Join(tmpDir, "runtime", "receipts") {
		t.Fatalf("persisted runtime context paths = %#v", persistedContext)
	}

	// 2. Verify Event Store JSONL (Action telemetry order and fields)
	f, err := os.Open(filepath.Join(tmpDir, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)

	var events []map[string]interface{}
	for scanner.Scan() {
		var ev map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
			events = append(events, ev)
		}
	}

	actionStartedFound := 0
	actionCompletedFound := 0
	toolObservationStartedFound := 0
	toolObservationCompletedFound := 0

	for _, ev := range events {
		t.Logf("Event: %v", ev)
		eventType, ok := ev["type"].(string)
		if !ok {
			continue
		}
		if eventType == "tool_observation_started" {
			toolObservationStartedFound++
		} else if eventType == "tool_observation_completed" {
			toolObservationCompletedFound++
		} else if eventType == "action_started" {
			actionStartedFound++
		} else if eventType == "action_completed" {
			actionCompletedFound++
			payload, ok := ev["payload"].(map[string]interface{})
			if !ok {
				t.Errorf("action_completed missing payload")
				continue
			}
			if payload["action_id"] == nil || payload["action_id"].(string) == "" {
				t.Errorf("action_completed missing action_id")
			}
			if payload["tool_name"] == nil || payload["tool_name"].(string) == "" {
				t.Errorf("action_completed missing tool_name")
			}
			artifacts, ok := payload["artifacts"].([]interface{})
			if !ok || len(artifacts) == 0 {
				t.Errorf("action_completed missing artifacts (receipt/transcript ref)")
			}
		}
	}

	if toolObservationStartedFound < 1 {
		t.Errorf("expected at least 1 tool_observation_started (from prepare), got %d", toolObservationStartedFound)
	}
	if toolObservationCompletedFound < 1 {
		t.Errorf("expected at least 1 tool_observation_completed, got %d", toolObservationCompletedFound)
	}
	if actionStartedFound < 2 {
		t.Errorf("expected at least 2 action_started (from execute), got %d", actionStartedFound)
	}
	if actionCompletedFound < 2 {
		t.Errorf("expected at least 2 action_completed, got %d", actionCompletedFound)
	}
}
