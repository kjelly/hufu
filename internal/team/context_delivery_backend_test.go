package team

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// Capture the actual fake app-server input, without an OpenAI proxy or a
// diagnostic instruction in the model prompt.
func TestContextDeliveryAgentBackendReceivesCompiledRequiredFragments(t *testing.T) {
	for _, mode := range []string{"legacy", "active"} {
		t.Run(mode, func(t *testing.T) { testContextDeliveryAgentBackend(t, mode) })
	}
}

func testContextDeliveryAgentBackend(t *testing.T, mode string) {
	t.Helper()
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", SideEffect: "workspace_write",
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c := newCodexE2ECoordinatorBase(t, workspace, []fakeCodexStep{
		codexInitializeStep(t), codexThreadStartStep(t, "context-thread", workspace),
		fakeCodexTurnCompletedStep(t, "context-turn", validProposalJSON("")),
	}, worker)
	if mode == "active" {
		manager, err := agent.NewProviderManager("", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.providerManager = manager
		c.session.Config.ContextRouting.QueryMode = "active"
		state, err := newExecutionPolicyState(c)
		if err != nil {
			t.Fatal(err)
		}
		c.executionPolicy = state
		if err := c.AdmitExecutionPolicy(); err != nil {
			t.Fatal(err)
		}
	}
	item := admitCodexE2ETask(t, c, worker, nil)
	_, err := c.executeTask(t.Context(), TaskDef{Agent: "worker", Goal: "baseline worker task", Execution: ExecutionContract{RequiresResult: true}}, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	scriptData, err := os.ReadFile(os.Getenv(fakeCodexServerScriptEnvVar))
	if err != nil {
		t.Fatal(err)
	}
	var script fakeCodexScript
	if err := json.Unmarshal(scriptData, &script); err != nil {
		t.Fatal(err)
	}
	requests, err := os.ReadFile(script.RawLogPath)
	if err != nil {
		t.Fatal(err)
	}
	turns := 0
	for line := range bytes.SplitSeq(requests, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var request struct {
			Method string `json:"method"`
			Params struct {
				Input []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatal(err)
		}
		if request.Method != "turn/start" {
			continue
		}
		turns++
		var input strings.Builder
		for _, part := range request.Params.Input {
			if part.Type == "text" {
				input.WriteString(part.Text)
			}
		}
		for _, sentinel := range []string{"id=current_task", "baseline worker task", "id=agent_instructions", "## External Provider Result Protocol", "WorkerResultProposal"} {
			if !strings.Contains(input.String(), sentinel) {
				t.Errorf("agent backend input missing %q", sentinel)
			}
		}
	}
	if turns != 1 {
		t.Fatalf("agent backend received %d turns, want 1", turns)
	}
	if len(c.todoItemByID(item.ID).ContextManifests) == 0 {
		t.Fatal("backend delivery has no durable manifest")
	}
	for _, manifest := range c.todoItemByID(item.ID).ContextManifests {
		if mode == "active" && (manifest.SchemaVersion != 3 || manifest.Query == nil || !supportedDeliveryContextManifest(manifest)) {
			t.Fatal("agent backend lost frozen active query attribution")
		}
	}
}
