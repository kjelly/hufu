package team

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestRuntimeWorkflow_Integration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "TestRuntimeWorkflow_Integration")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.MkdirAll(filepath.Join(tmpDir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	session := &TeamSession{
		Workspace: tmpDir,
		Config: agent.TeamConfig{
			Name: "test-team",
			Workflow: agent.WorkflowConfig{
				Phases: []string{string(PhasePrepare), string(PhaseAudit), string(PhaseExecute), string(PhaseVerify)},
			},
			Policies: agent.WorkflowPolicies{
				FailFast: true,
			},
			MaxSteps: 5,
		},
	}
	session.Agents = map[string]*agent.AgentDef{
		"preparer": {Role: "prepare", Generation: agent.GenerationParams{Model: "test-model"}},
	}
	session.ProviderRegistry = NewProviderRegistry()
	session.ContractTasks = []TaskDef{
		{Phase: PhasePrepare, Agent: "preparer"},
	}

	pm, _ := agent.NewProviderManager("", "", nil)
	c := &Coordinator{
		session:         session,
		providerManager: pm,
	}

	es, err := NewEventStore(tmpDir, "run-integration", "session-integration")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = es

	phaseWorkflow, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	var emitted []map[string]interface{}
	phaseWorkflow.setEventEmitter(func(eventType string, phase Phase, details LifecycleEventPayload) {
		details.Phase = string(phase)

		var rawMap map[string]interface{}
		b, _ := json.Marshal(details)
		_ = json.Unmarshal(b, &rawMap)

		c.emitEvent(eventType, "runtime", "", details)
		emitted = append(emitted, map[string]interface{}{
			"type":    eventType,
			"phase":   phase,
			"details": rawMap,
		})
	})
	c.phaseWorkflow = phaseWorkflow

	if err := c.phaseWorkflow.Start(); err != nil {
		t.Fatal(err)
	}

	c.phaseWorkflow.failLocked("test-component", "test-provider", "TEST", "test failure", false, PhaseStatusFailure)

	// Close the event logger so we can read the file
	if c.eventStore != nil {
		c.eventStore.Close()
	}

	f, err := os.Open(filepath.Join(tmpDir, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)

	startedFound := false
	failedFound := false

	for scanner.Scan() {
		var ev map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}

		evtType, _ := ev["type"].(string)
		if evtType == "phase_started" {
			if payload, ok := ev["payload"].(map[string]interface{}); ok {
				if p, _ := payload["phase"].(string); p == string(PhasePrepare) {
					startedFound = true
				} else {
					t.Errorf("expected PhasePrepare, got %s", p)
				}
			} else {
				t.Errorf("payload is not a map")
			}
		} else if evtType == "phase_failed" {
			if payload, ok := ev["payload"].(map[string]interface{}); ok {
				if p, _ := payload["phase"].(string); p == string(PhasePrepare) {
					failedFound = true
				} else {
					t.Errorf("expected PhasePrepare, got %s", p)
				}
				if actor, _ := payload["agent"].(string); actor != "test-component" {
					t.Errorf("missing or wrong actor in phase_failed")
				}
				if prov, _ := payload["provider"].(string); prov != "test-provider" {
					t.Errorf("missing or wrong provider in phase_failed")
				}
				if sig, ok := payload["failure_signature"].(string); !ok || sig == "" {
					t.Errorf("missing or wrong signature in phase_failed")
				}
			} else {
				t.Errorf("payload is not a map")
			}
		}
	}

	if !startedFound {
		t.Errorf("phase_started event not found in event_store.jsonl")
	}
	if !failedFound {
		t.Errorf("phase_failed event not found in event_store.jsonl")
	}
}
