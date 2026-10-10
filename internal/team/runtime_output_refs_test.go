package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestExecuteTasksResolvesInitialStaticRuntimeContextBeforeTodoCreation(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{BindInitialTaskContracts: true, InitialBatch: []string{"worker"}})
	c.session.Workspace = t.TempDir()
	c.session.ContractTasks = []TaskDef{{
		ID: "consume", Agent: "worker", Constraints: "{observation}",
		FactRefs: []FactRef{{Name: "observation", TaskID: "missing", RuntimeOutput: "observation"}},
	}}
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "consume"}})
	if err == nil || !strings.Contains(err.Error(), "runtime output is not canonical") {
		t.Fatalf("initial static reference was not resolved: %v", err)
	}
	if len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("unresolved static context admitted a task")
	}
}

func runtimeOutputRefFixture(t *testing.T) (*Coordinator, *TodoItem) {
	t.Helper()
	definitions := []RunInputDefinition{{Name: "request", Schema: RunInputSchema{Type: "object"}, Required: true}}
	snapshot := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{Name: "request", RawValue: []byte(`{"count":2}`), Source: RunInputSourceCLI}})
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"observation": map[string]any{"count": 2, "literal": "{suffix}"}})
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "prepare-outputs", ContractID: "prepare-outputs", Agent: "runtime"}})[0]
	item.Status = TaskDone
	item.RunInputSnapshotID, item.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	item.MaterializedActionPayloadHash = "sha256:payload"
	item.TypedResult = &TaskResult{
		TaskID: item.ID, Status: TaskResultStatusSuccess, Summary: "prepared", Source: "runtime",
		RuntimeOutputs: outputs, RuntimeOutputsHash: digest, Facts: map[string]any{"suffix": "changed"},
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
	}
	zero := 0
	item.ExecutionReceipt = &ExecutionReceipt{
		RunID: "run-output-refs", TaskID: item.ID, Attempt: 1, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: &zero,
		TranscriptRef: "sha256-transcript", ActionInvocationID: "action-1", RuntimeOutputsHash: digest,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, MaterializedActionPayloadHash: item.MaterializedActionPayloadHash,
	}
	c := &Coordinator{
		session: &TeamSession{RunInputDefinitions: definitions}, taskTracker: tracker, executionRunID: "run-output-refs",
		sessionData: &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID},
	}
	c.storeSubmittedTaskResult(item.ID, item.TypedResult)
	return c, item
}

func TestRuntimeOutputRefsAreReceiptVerifiedAndPreserveLiteralTokens(t *testing.T) {
	c, item := runtimeOutputRefFixture(t)
	resolved, err := c.resolveFactRefs([]TaskDef{{
		Goal: "Inspect {observation}", Constraints: "{suffix}", FactRefs: []FactRef{
			{Name: "observation", TaskID: "prepare-outputs", RuntimeOutput: "observation"},
			{Name: "suffix", TaskID: item.ID, Fact: "suffix"},
		},
	}})
	if err != nil || resolved[0].Goal != `Inspect {"count":2,"literal":"{suffix}"}` || resolved[0].Constraints != "changed" {
		t.Fatalf("canonical value changed or unresolved: %#v, %v", resolved, err)
	}
}

func TestRuntimeOutputRefsFailClosedForUnavailableOrUntrustedSources(t *testing.T) {
	for _, scenario := range []string{"pending", "model", "digest", "receipt", "snapshot", "old_run", "missing_output", "ambiguous", "mixed_selectors"} {
		t.Run(scenario, func(t *testing.T) {
			c, item := runtimeOutputRefFixture(t)
			ref := FactRef{Name: "observation", TaskID: "prepare-outputs", RuntimeOutput: "observation"}
			items := []*TodoItem{item}
			switch scenario {
			case "pending":
				item.Status = TaskInProgress
			case "model":
				item.TypedResult.Source = "submitted"
			case "digest":
				item.TypedResult.RuntimeOutputsHash = "sha256:tampered"
			case "receipt":
				item.ExecutionReceipt.RuntimeOutputsHash = "sha256:other"
			case "snapshot":
				item.RunInputSnapshotID = "other-snapshot"
			case "old_run":
				item.ExecutionReceipt.RunID = "run-prior"
			case "missing_output":
				ref.RuntimeOutput = "missing"
			case "ambiguous":
				duplicate := *item
				duplicate.ID = "2"
				items = append(items, &duplicate)
			case "mixed_selectors":
				ref.Fact = "suffix"
			}
			c.taskTracker.TodoList().Restore(items)
			if _, err := c.resolveFactRefs([]TaskDef{{Goal: "{observation}", FactRefs: []FactRef{ref}}}); err == nil {
				t.Fatalf("authorized %s source", scenario)
			}
		})
	}
}

func TestStaticRuntimeContextSurvivesResolutionAndRebinding(t *testing.T) {
	c, _ := runtimeOutputRefFixture(t)
	c.session.Config.Delegation = agent.DelegationPolicy{BindTaskGoalContracts: true}
	c.session.ContractTasks = []TaskDef{{
		ID: "join", Agent: "consumer", Constraints: "Canonical observation: {observation}",
		FactRefs: []FactRef{{Name: "observation", TaskID: "prepare-outputs", RuntimeOutput: "observation"}},
	}}
	bound, effective, err := CompileTaskGoalContracts(c.session, []TaskDef{{Agent: "consumer", ContractID: "join", Goal: "Join results", Constraints: "guessed metadata"}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.resolveFactRefs(bound)
	if err != nil {
		t.Fatal(err)
	}
	final, _, err := CompileTaskGoalContracts(c.session, resolved)
	if err != nil || final[0].Constraints != `Canonical observation: {"count":2,"literal":"{suffix}"}` || len(final[0].FactRefs) != 0 {
		t.Fatalf("rebind lost resolved context: %#v, %v", final, err)
	}
	raw, err := json.Marshal(effective[0])
	if err != nil || !strings.Contains(string(raw), `"runtime_output":"observation"`) {
		t.Fatalf("policy omitted static source: %s, %v", raw, err)
	}
	c.session.ContractTasks[0].FactRefs[0].RuntimeOutput = "other"
	_, changed, err := CompileTaskGoalContracts(c.session, []TaskDef{{Agent: "consumer", ContractID: "join"}})
	if err != nil || changed[0].Hash == effective[0].Hash {
		t.Fatalf("source change did not change policy identity: %v", err)
	}
}

func TestInitialStaticRuntimeContextSurvivesResolutionAndRebinding(t *testing.T) {
	c, _ := runtimeOutputRefFixture(t)
	c.session.Config.Delegation = agent.DelegationPolicy{BindInitialTaskContracts: true, InitialBatch: []string{"consumer"}}
	c.session.ContractTasks = []TaskDef{{
		ID: "consume", Agent: "consumer", Constraints: "{observation}",
		FactRefs: []FactRef{{Name: "observation", TaskID: "prepare-outputs", RuntimeOutput: "observation"}},
	}}
	bound, effective, err := CompileInitialTaskContracts(c.session, []TaskDef{{Agent: "consumer"}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.resolveFactRefs(bound)
	if err != nil {
		t.Fatal(err)
	}
	final, _, err := CompileInitialTaskContracts(c.session, resolved)
	if err != nil || final[0].Constraints != `{"count":2,"literal":"{suffix}"}` || len(final[0].FactRefs) != 0 || len(effective[0].ContextFactRefs) != 1 {
		t.Fatalf("initial binding lost canonical context: %#v, %v", final, err)
	}
}

func TestHufuCodeReviewSynthesisReceivesPrepareOutputsWithoutCoordinatorConstraints(t *testing.T) {
	c, item := runtimeOutputRefFixture(t)
	loaded := loadHufuCodeReviewTeam(t)
	c.session.Config = loaded.Config
	c.session.ContractTasks = loaded.ContractTasks
	item.PlanTaskID, item.ContractID = "produce-workset", "produce-workset"
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{"requested": map[string]any{"count": 10}, "satisfied": true}})
	if err != nil {
		t.Fatal(err)
	}
	item.TypedResult.RuntimeOutputs, item.TypedResult.RuntimeOutputsHash = outputs, digest
	item.ExecutionReceipt.RuntimeOutputsHash = digest
	verifier := *item
	verifier.ID, verifier.PlanTaskID, verifier.ContractID = "2", "verify-targeted-go-tests", "verify-targeted-go-tests"
	resultCopy, receiptCopy := *item.TypedResult, *item.ExecutionReceipt
	verifier.TypedResult, verifier.ExecutionReceipt = &resultCopy, &receiptCopy
	verifier.TypedResult.TaskID, verifier.ExecutionReceipt.TaskID = verifier.ID, verifier.ID
	outputs, digest, err = CanonicalizeRuntimeOutputs(map[string]any{"targeted_go_tests": map[string]any{"passed": true, "exit_code": 0}})
	if err != nil {
		t.Fatal(err)
	}
	verifier.TypedResult.RuntimeOutputs, verifier.TypedResult.RuntimeOutputsHash = outputs, digest
	verifier.ExecutionReceipt.RuntimeOutputsHash = digest
	inventory := *item
	inventory.ID, inventory.PlanTaskID, inventory.ContractID = "3", "inventory-review-handoffs", "inventory-review-handoffs"
	inventoryResultCopy, inventoryReceiptCopy := *item.TypedResult, *item.ExecutionReceipt
	inventory.TypedResult, inventory.ExecutionReceipt = &inventoryResultCopy, &inventoryReceiptCopy
	inventory.TypedResult.TaskID, inventory.ExecutionReceipt.TaskID = inventory.ID, inventory.ID
	outputs, digest, err = CanonicalizeRuntimeOutputs(map[string]any{"review_handoff_inventory": map[string]any{"passed": true, "source_task_ids": []any{"4", "13"}, "required_critic_source_task_ids": []any{"13"}}})
	if err != nil {
		t.Fatal(err)
	}
	inventory.TypedResult.RuntimeOutputs, inventory.TypedResult.RuntimeOutputsHash = outputs, digest
	inventory.ExecutionReceipt.RuntimeOutputsHash = digest
	c.taskTracker.TodoList().Restore([]*TodoItem{item, &verifier, &inventory})
	bound, _, err := CompileTaskGoalContracts(c.session, []TaskDef{{Agent: "synthesizer", ContractID: "synthesize-review", Goal: "Synthesize all results"}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.resolveFactRefs(bound)
	if err != nil {
		t.Fatal(err)
	}
	final, _, err := CompileTaskGoalContracts(c.session, resolved)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`scope: {"requested":{"count":10},"satisfied":true}`, `targeted_go_tests: {"exit_code":0,"passed":true}`, `"required_critic_source_task_ids":["13"]`} {
		if !strings.Contains(final[0].Constraints, want) {
			t.Fatalf("static synthesis context omitted %s: %s", want, final[0].Constraints)
		}
	}
}
