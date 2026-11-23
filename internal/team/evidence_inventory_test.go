package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func seedReviewEvidenceInventory(t *testing.T, c *Coordinator, all, required []string) *TodoItem {
	t.Helper()
	snapshot, err := ResolveRunInputSnapshot(nil, nil, c.executionRunID, c.executionRunID, c.session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	c.sessionData = &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID}
	items := c.taskTracker.TodoList().Items()
	hashes := make([]map[string]string, 0, len(all))
	for _, id := range all {
		source := todoItemByID(items, id)
		result := source.TypedResult
		if result == nil {
			result = c.GetTaskResult(id)
		}
		if result == nil || result.StructuredPayload == nil {
			t.Fatalf("source %q has no accepted structured result", id)
		}
		hashes = append(hashes, map[string]string{"task_id": id, "payload_sha256": result.StructuredPayload.SHA256})
		source.TypedResult = result
		attachInventoryTestReceipt(c, source, snapshot)
	}
	id := "inventory"
	_, fixture := runtimeOutputRefFixture(t)
	fixture.ID, fixture.ContractID, fixture.PlanTaskID = id, "inventory-review-handoffs", "inventory-review-handoffs"
	fixture.TypedResult.TaskID, fixture.ExecutionReceipt.TaskID = id, id
	fixture.TypedResult.RunInputSnapshotID, fixture.TypedResult.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	fixture.RunInputSnapshotID, fixture.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	fixture.ExecutionReceipt.RunID = c.executionRunID
	fixture.ExecutionReceipt.RunInputSnapshotID, fixture.ExecutionReceipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	setInventoryTestOutput(t, c, fixture, all, required, hashes)
	c.taskTracker.TodoList().Restore(append(items, fixture))
	return fixture
}

func attachInventoryTestReceipt(c *Coordinator, item *TodoItem, snapshot *RunInputSnapshot) {
	item.RunInputSnapshotID, item.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	item.ExecutionReceipt = &ExecutionReceipt{
		RunID: c.executionRunID, TaskID: item.ID, ExitCode: new(0), TranscriptRef: "transcript",
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		ResultPayloadSHA256: item.TypedResult.StructuredPayload.SHA256, ResultValidation: ResultValidationValid,
	}
}

func setInventoryTestOutput(t *testing.T, c *Coordinator, item *TodoItem, all, required []string, hashes []map[string]string) {
	t.Helper()
	value := map[string]any{"passed": true, "source_task_ids": all, "required_critic_source_task_ids": required, "sources": hashes}
	normalized, err := inventoryFixtureJSON(map[string]any{"review_handoff_inventory": value})
	if err != nil {
		t.Fatal(err)
	}
	outputs, digest, err := CanonicalizeRuntimeOutputs(normalized)
	if err != nil {
		t.Fatal(err)
	}
	item.TypedResult.RuntimeOutputs, item.TypedResult.RuntimeOutputsHash = outputs, digest
	item.ExecutionReceipt.RuntimeOutputsHash = digest
	c.storeSubmittedTaskResult(item.ID, item.TypedResult)
	if c.taskTracker.TodoList().Has(item.ID) {
		items := c.taskTracker.TodoList().Items()
		*todoItemByID(items, item.ID) = *item
		c.taskTracker.TodoList().Restore(items)
	}
}

func TestHufuCodeReviewInventoryDispatch(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"empty", ""}, {"dispatch", ""}, {"duplicate", "duplicate or invalid source ID"}, {"missing", "has no accepted result"},
		{"hash mismatch", "payload hash mismatch"}, {"stale run", "no accepted receipt from this run"}, {"limit", "exceeding the limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			loaded := loadHufuCodeReviewTeam(t)
			c := newTestCoordinator(t)
			c.session.ResultContracts, c.session.ContractTasks = loaded.ResultContracts, loaded.ContractTasks
			c.session.Config.Delegation = loaded.Config.Delegation
			first, second := seedEmptyCriticReview(t, c), seedEmptyCriticReview(t, c)
			all, required := []string{first, second}, []string{second, first}
			if scenario.name == "empty" {
				required = []string{}
			}
			fixture := seedReviewEvidenceInventory(t, c, all, required)
			hashes := []map[string]string{
				{"task_id": first, "payload_sha256": todoByID(t, c, first).TypedResult.StructuredPayload.SHA256},
				{"task_id": second, "payload_sha256": todoByID(t, c, second).TypedResult.StructuredPayload.SHA256},
			}
			switch scenario.name {
			case "duplicate":
				required = []string{first, first}
			case "missing":
				required = []string{"absent"}
			case "hash mismatch":
				hashes[1]["payload_sha256"] = "stale"
			case "stale run":
				items := c.taskTracker.TodoList().Items()
				todoItemByID(items, second).ExecutionReceipt.RunID = "expired"
				c.taskTracker.TodoList().Restore(items)
			case "limit":
				for i := range 33 {
					required = append(required, fmt.Sprint(100+i))
				}
			}
			setInventoryTestOutput(t, c, fixture, all, required, hashes)
			request, err := decodeModelTaskDefsForMode([]byte(`{"tasks":[{"agent":"critic","contract_id":"critic-review","goal":"Audit review evidence"}]}`), true)
			if err != nil {
				t.Fatal(err)
			}
			bound, _, err := CompileTaskGoalContracts(c.session, request)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := c.expandFanOutTasks(bound)
			if scenario.want != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.want) {
					t.Fatalf("expected %q, got %v", scenario.want, err)
				}
				return
			}
			if err != nil || len(rows) != len(required) {
				t.Fatalf("rows %d, required %d: %v", len(rows), len(required), err)
			}
			synth := c.inventoryContract("synthesize-review")
			if len(rows) == 0 {
				ids, _, err := c.inventoryFanIn(synth.EvidenceInventory)
				if err != nil || !slices.Equal(ids, all) {
					t.Fatalf("empty required list blocked synthesis: %v %v", ids, err)
				}
				return
			}
			if !slices.Equal(rows[0].EvidenceFrom, []string{second}) || !slices.Equal(rows[1].EvidenceFrom, []string{first}) ||
				!slices.Equal(rows[1].OrderAfter, []int{0}) || rows[0].EvidenceInventoryBinding.OutputsHash != fixture.ExecutionReceipt.RuntimeOutputsHash {
				t.Fatalf("wrong inventory ordering or binding: %+v", rows)
			}
			if _, _, err := c.inventoryFanIn(synth.EvidenceInventory); err == nil || !strings.Contains(err.Error(), "has no accepted critic-review child") {
				t.Fatalf("missing critiqued source did not block synthesis: %v", err)
			}
		})
	}
}

func TestContractDataflowInventoryWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"accepted report", "empty critique inventory", "failed prerequisite", "checkpoint paused critique", "failed durable critique prefix"} {
		t.Run(scenario, func(t *testing.T) { runInventoryWorkflow(t, scenario) })
	}
}

func runInventoryWorkflow(t *testing.T, scenario string) {
	t.Helper()
	failDerivation := scenario == "failed prerequisite"
	critiqueRequired := scenario != "empty critique inventory"
	s := workflowTestSession(t)
	s.Config.Workflow.Phases = []string{"prepare", "verify"}
	s.Config.Policies.AllowPhaseSkip = true
	s.Config.Verification.Required = true
	s.Config.DefaultLLMBackend = "ollama"
	s.Config.ResultTransport = agent.ResultTransportTool
	for _, agent := range s.Agents {
		agent.Generation.Model = "ollama/test"
		agent.Tools = submitResultToolName
	}
	s.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Default: []byte("{}")}}
	project := t.TempDir()
	if err := s.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(project, "AGENTS.md")
	if !critiqueRequired {
		s.Config.RequiredResources = missingRequiredResourceConfig("inventory").RequiredResources
		if err := os.WriteFile(rules, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fields := `"kind":{"type":"string","const":"fixture"}`
	inputs := `[{"source_kind":"const","target":"/kind"}]`
	source := compileFixtureJSONSchema(t, "source.json", `{"type":"object","properties":{`+fields+`},"required":["kind"],"additionalProperties":false,"x-hufu-runtime-inputs":`+inputs+`}`)
	report := compileFixtureJSONSchema(t, "report.json", `{
		"type":"object","required":["audited","completion","coverage","details","inputs"],"additionalProperties":false,
		"properties":{"audited":{"type":"boolean","const":true},"completion":{"type":"object","properties":{"status":{"type":"string"},"summary":{"type":"string"}},"required":["status","summary"]},"coverage":{"type":"string"},"details":{"type":"string"},"inputs":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["task","sha","kind"],"properties":{"task":{"type":"string"},"sha":{"type":"string"},"kind":{"type":"string"}}}}},
		"x-hufu-evidence-inputs":{"runtime_owned":true,"runtime_complete":true,"items_pointer":"/inputs","complete_pointer":"/audited","task_id_pointer":"/task","hash_pointer":"/sha","value_bindings":{"/kind":"/kind"}},
		"x-hufu-runtime-inputs":[{"target":"/coverage","source_task":"derive","output":"coverage"},{"target":"/completion","source_task":"derive","output":"completion"}],
		"x-hufu-runtime-completion":"/completion"
	}`)
	s.ResultContracts = map[string]*CompiledResultContract{source.ID: source, report.ID: report}
	action := func(id, role string, phase Phase, optional bool) TaskDef {
		return TaskDef{ID: id, Agent: role, Phase: phase, Optional: optional, SideEffect: SideEffectNone, Recovery: RecoveryRetry,
			Action: &Action{Capability: "structured-actions", Type: id, Payload: "{}"},
			VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
				{Pointer: "/runtime_outputs", Op: "non_empty"},
			}}}
	}
	work := func(id, role string, optional bool, ref ResultContractRef) TaskDef {
		return TaskDef{ID: id, Agent: role, Phase: PhaseVerify, Optional: optional, SideEffect: SideEffectNone, Recovery: RecoveryRetry,
			Execution: ExecutionContract{RequiresResult: true}, ResultContract: &ref,
			VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
				{Pointer: "/summary", Op: "non_empty"},
			}}}
	}
	critic := work("critique", "auditor", true, source.ref(true))
	critic.EvidenceInventory = &EvidenceInventorySpec{TaskID: "inventory", Output: "inventory", IDsPointer: "/required",
		HashesPointer: "/sources", MaxSources: 4, Goal: "Check {source_task_id}"}
	synth := work("synthesize", "executor", true, report.ref(true))
	synth.DependsOnContracts = []string{"derive"}
	synth.EvidenceInventory = new(*critic.EvidenceInventory)
	synth.EvidenceInventory.IDsPointer, synth.EvidenceInventory.FromFanOut, synth.EvidenceInventory.Goal = "/all", critic.ID, ""
	audit := action("audit", "verifier", PhaseVerify, true)
	audit.DependsOnContracts = []string{synth.ID}
	audit.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/runtime_outputs/passed", Op: "equals", Value: true},
	}}
	s.ContractTasks = []TaskDef{action("prepare", "preparer", PhasePrepare, false), work("source-a", "auditor", false, source.ref(true)),
		work("source-b", "executor", false, source.ref(true)), action("inventory", "verifier", PhaseVerify, true), critic,
		action("derive", "verifier", PhaseVerify, true), synth, audit}

	var c *Coordinator
	var workerCalls []string
	provider := inventoryWorkflowProvider(func(_ context.Context, a Action) (map[string]any, error) {
		if failDerivation && a.Type == "derive" {
			return nil, fmt.Errorf("fixture derivation failed")
		}
		return runInventoryFixtureAction(c, a.Type, critiqueRequired)
	})
	s.ProviderRegistry = NewProviderRegistry()
	s.ProviderRegistry.Register("structured-actions", provider)
	var endRun func()
	c, endRun = startInventoryWorkflow(t, s, nil, &workerCalls)
	dispatch := func(tasks ...TaskDef) {
		t.Helper()
		if output, err := c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), tasks); err != nil {
			t.Fatalf("dispatch: %+v: %v (%s)", tasks, err, output)
		}
	}
	request := func(role, id string) TaskDef { return TaskDef{Agent: role, ContractID: id, Goal: id} }
	dispatch(request("preparer", "prepare"))
	dispatch(request("auditor", "source-a"), request("executor", "source-b"))
	dispatch(request("verifier", "inventory"))
	criticRequest := request("auditor", "critique")
	reportTasks := []TaskDef{request("verifier", "derive"), request("executor", "synthesize"), request("verifier", "audit")}
	if !critiqueRequired {
		if err := os.WriteFile(rules, []byte("changed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ExecuteTasks(t.Context(), []TaskDef{criticRequest}); err == nil || !strings.Contains(err.Error(), "resource lock drift") {
			t.Fatalf("empty expansion bypassed required resource checks: %v", err)
		}
		if err := os.WriteFile(rules, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		dispatch(criticRequest)
		dispatch(reportTasks...)
		audits := inventoryWorkflowOccurrences(c, "audit")
		if len(inventoryWorkflowOccurrences(c, "critique")) != 0 || slices.Contains(workerCalls, "critique") ||
			len(audits) != 1 || !isVerifySuccess(audits[0].VerifyResult) || c.phaseWorkflow.requireFinished() != nil {
			t.Fatalf("empty critique list did not reach an accepted report with zero critic calls: %v", workerCalls)
		}
		return
	}
	if _, err := c.ExecuteTasks(t.Context(), reportTasks); err == nil || !strings.Contains(err.Error(), "has no accepted critique child") {
		t.Fatalf("report was admitted without critique: %v", err)
	}

	if scenario == "checkpoint paused critique" {
		c.SetStepConfirmFn(func(_ context.Context, proposals []TaskDef) (bool, error) {
			proposals[1].OrderAfter[0] = 1
			return true, nil
		})
		pauseInventoryWorkflow(t, c, criticRequest)
		c.SetStepConfirmFn(nil)
		c, _ = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
		rows := inventoryWorkflowOccurrences(c, "critique")
		if len(rows) != 2 || rows[0].Status != TaskPaused || !rows[0].CheckpointPause || !slices.Equal(rows[1].OrderAfter, []string{rows[0].ID}) {
			t.Fatalf("replay lost checkpoint pause or ordering: %+v", rows)
		}
		if count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); err != nil || count != 0 ||
			c.todoItemByID(rows[1].ID).Status != TaskPending || slices.Contains(workerCalls, "critique") {
			t.Fatalf("successor ran before the checkpoint settled: count=%d calls=%v err=%v", count, workerCalls, err)
		}
		if err := c.projectCheckpointOutcome(&taskDiscipline{todoID: rows[0].ID}, CheckpointDecision{Action: CheckpointStop, Detail: "fixture stopped"}); err != nil {
			t.Fatal(err)
		}
		if count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); err != nil || count != 1 ||
			c.todoItemByID(rows[1].ID).Status != TaskDone {
			t.Fatalf("successor did not resume after its predecessor settled: count=%d calls=%v err=%v", count, workerCalls, err)
		}
		return
	}

	if scenario == "accepted report" {
		for _, declined := range []bool{false, true} {
			confirmInventoryBatch(t, c, []TaskDef{criticRequest}, declined, func(proposals []TaskDef) {
				if len(proposals) != 2 || len(proposals[0].EvidenceFrom) != 1 || len(proposals[1].EvidenceFrom) != 1 {
					t.Fatalf("operator received incomplete critique expansion: %+v", proposals)
				}
			})
			c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
			if count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); count != 0 || err != nil ||
				len(inventoryWorkflowOccurrences(c, "critique")) != 0 || slices.Contains(workerCalls, "critique") {
				t.Fatalf("unapproved critique survived step declined=%t: count=%d calls=%v err=%v", declined, count, workerCalls, err)
			}
		}
	}

	// A failure after the first creation append leaves a durable prefix.
	journal := &interruptInventoryDispatch{EventJournal: eventStoreJournal{store: c.eventStore}}
	c.SetEventJournal(journal)
	approved := false
	c.SetStepConfirmFn(func(_ context.Context, proposals []TaskDef) (bool, error) {
		if len(inventoryWorkflowOccurrences(c, "critique")) != 0 || len(proposals) != 2 {
			t.Fatalf("critic batch was already created or incomplete when presented for approval: %+v", proposals)
		}
		approved = true
		proposals[0].EvidenceInventoryBinding.SourceHash = "mutated"
		proposals[0].EvidenceInventory.MaxSources = 1
		return true, nil
	})
	if _, err := c.ExecuteTasks(t.Context(), []TaskDef{criticRequest}); err == nil || !strings.Contains(err.Error(), "interrupted inventory dispatch") {
		t.Fatalf("expected interrupted batch creation, got %v", err)
	}
	if !approved || critic.EvidenceInventory.MaxSources != 4 {
		t.Fatal("task creation lacked approval or exposed the frozen declaration")
	}
	c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
	failCritique := scenario == "failed durable critique prefix"
	if failCritique {
		c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			workerCalls = append(workerCalls, "critique")
			return nil, fmt.Errorf("fixture critique worker failed")
		}})
	}
	if count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); count != 1 || (!failCritique && err != nil) {
		t.Fatalf("resume existing inventory child: count=%d, %v", count, err)
	}
	accepted := inventoryWorkflowOccurrences(c, "critique")[0]
	if !isTerminalTaskStatus(accepted.Status) || (accepted.Status == TaskDone) == failCritique || (!failCritique && accepted.ExecutionReceipt == nil) {
		t.Fatalf("first critique did not complete: %+v", accepted)
	}
	c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
	if scenario == "accepted report" {
		assertInventoryCacheIsolation(t, c, accepted)
		goal := "Check " + inventoryWorkflowOccurrences(c, "source-b")[0].ID
		c.delegatedTasks[duplicateTaskIdentity("auditor", goal, critic.VerifySpec, "", "")] = 1
		c.SetTaskCache(inventoryPriorResultCache{TaskCache: c.TaskCache(), goal: goal, prior: accepted.TypedResult.Summary})
	}
	if failCritique {
		// The scheduler settles the missing occurrence before the phase
		// observes the first critic's terminal failure.
		if _, err := c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{criticRequest}); err == nil ||
			!strings.Contains(err.Error(), "fixture critique worker failed") {
			t.Fatalf("phase did not retain the failed critique after backfill: %v", err)
		}
		if _, err := c.ExecuteTasks(t.Context(), []TaskDef{criticRequest}); err == nil || !strings.Contains(err.Error(), "workflow is failed") {
			t.Fatalf("empty re-dispatch bypassed the failed phase: %v", err)
		}
	} else {
		dispatch(criticRequest)
		dispatch(criticRequest)
	}
	children := inventoryWorkflowOccurrences(c, "critique")
	if len(children) != 2 || children[0].ID != accepted.ID || !sameCanonicalEvidence(children[0].TypedResult, accepted.TypedResult) ||
		children[0].Status != accepted.Status || children[1].Status != TaskDone ||
		!sameCanonicalEvidence(children[0].ExecutionReceipt, accepted.ExecutionReceipt) ||
		children[1].ExecutionReceipt == nil || children[1].ExecutionReceipt.TaskID != children[1].ID ||
		!slices.Equal(children[1].EvidenceFrom, []string{inventoryWorkflowOccurrences(c, "source-b")[0].ID}) {
		t.Fatal("re-dispatch changed a settled child or duplicated the expansion")
	}
	if failCritique {
		if _, _, err := c.inventoryFanIn(synth.EvidenceInventory); err == nil || !strings.Contains(err.Error(), "has no accepted critique child") {
			t.Fatalf("report evidence was assembled after the first critique failed: %v", err)
		}
		if c.phaseWorkflow.requireFinished() == nil {
			t.Fatal("workflow finished without an accepted critique")
		}
		return
	}

	// An error inserting any member of the static batch rolls back its
	// predecessors. Reopen SQLite after each failed transaction.
	if !failDerivation {
		for _, declined := range []bool{false, true} {
			confirmInventoryBatch(t, c, reportTasks, declined, func(proposals []TaskDef) {
				if len(proposals) != len(reportTasks) || len(proposals[1].EvidenceFrom) != 4 {
					t.Fatalf("operator received incomplete bound report batch: %+v", proposals)
				}
			})
			c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
			if count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context())); count != 0 || err != nil {
				t.Fatalf("recovery after step declined=%t: count=%d err=%v", declined, count, err)
			}
			for _, task := range reportTasks {
				if len(inventoryWorkflowOccurrences(c, task.ContractID)) != 0 {
					t.Fatalf("unapproved report task %q survived step declined=%t", task.ContractID, declined)
				}
			}
		}
		for _, position := range []int{1, 2} {
			interruptReportCreation(t, c, reportTasks, position)
			c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
			for _, task := range reportTasks {
				if len(inventoryWorkflowOccurrences(c, task.ContractID)) != 0 {
					t.Fatalf("durable report prefix retained after member %d failed", position)
				}
			}
		}
		interruptReportWithdrawal(t, c, reportTasks, false)
		c, endRun = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
		for _, task := range reportTasks {
			if len(inventoryWorkflowOccurrences(c, task.ContractID)) != 0 {
				t.Fatalf("withdrawn report task %q survived replay", task.ContractID)
			}
		}
	}

	// Stop at the durable creation boundary before the report worker runs.
	if failDerivation {
		interruptReportWorkflow(t, c, reportTasks)
	} else {
		interruptReportWithdrawal(t, c, reportTasks, true)
	}
	c, _ = restartInventoryWorkflow(t, c, endRun, s, &workerCalls)
	count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context()))
	if failDerivation {
		synth := inventoryWorkflowOccurrences(c, "synthesize")[0]
		audit := inventoryWorkflowOccurrences(c, "audit")[0]
		if err == nil || count != 1 || slices.Contains(workerCalls, "synthesize") ||
			synth.Status != TaskBlocked || audit.Status != TaskBlocked || audit.ExecutionReceipt != nil {
			t.Fatalf("resumed report ran past failed derivation: count=%d calls=%v synthesis=%s audit=%s receipt=%v err=%v",
				count, workerCalls, synth.Status, audit.Status, audit.ExecutionReceipt, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	final := inventoryWorkflowOccurrences(c, "synthesize")
	audits := inventoryWorkflowOccurrences(c, "audit")
	if len(final) != 1 || final[0].Status != TaskDone || final[0].TypedResult.Summary != "2 sources and 2 critiques" ||
		len(final[0].EvidenceFrom) != 4 || len(audits) != 1 || !isVerifySuccess(audits[0].VerifyResult) || c.phaseWorkflow.requireFinished() != nil {
		t.Fatalf("workflow did not produce a verified report: %#v", final)
	}
	if !slices.Equal(workerCalls, []string{"source-a", "source-b", "critique", "critique", "synthesize"}) &&
		!slices.Equal(workerCalls, []string{"source-b", "source-a", "critique", "critique", "synthesize"}) {
		t.Fatalf("unexpected or repeated model calls: %v", workerCalls)
	}
}

func assertInventoryCacheIsolation(t *testing.T, c *Coordinator, accepted *TodoItem) {
	t.Helper()
	cache := c.TaskCache().(*defaultTaskCache)
	ordinary := inventoryWorkflowOccurrences(c, "source-a")[0]
	entries := cache.entriesFor("auditor")
	if !slices.ContainsFunc(entries, func(entry cachedTaskEntry) bool {
		return entry.pinned && entry.taskDesc == ordinary.Desc && entry.output == ordinary.Output
	}) || slices.ContainsFunc(entries, func(entry cachedTaskEntry) bool {
		return entry.taskDesc == accepted.Desc
	}) {
		t.Fatalf("recovery retained the wrong cache entries: %v", entries)
	}
	// An ordinary entry can have the same presentation text and verification.
	// Resetting a bound task must leave that unrelated entry intact.
	cache.Restore([]TaskCacheSeed{{AgentKey: "auditor", Task: accepted.Desc, Output: "ordinary output",
		VerifySpec: accepted.VerifySpec, Verify: accepted.Verify, VerifyMode: accepted.VerifyMode, Pinned: true}})
	task := taskDefFromTodoItem(accepted)
	scheduler := mustNewDAGScheduler(t, c, []TaskDef{task}, []*TodoItem{accepted}, nil)
	if err := scheduler.resetTask(t.Context(), 0, "fixture retry"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(cache.entriesFor("auditor"), func(entry cachedTaskEntry) bool {
		return entry.taskDesc == accepted.Desc && entry.output == "ordinary output"
	}) {
		t.Fatal("inventory retry evicted the ordinary task's cache entry")
	}
}

func pauseInventoryWorkflow(t *testing.T, c *Coordinator, task TaskDef) {
	t.Helper()
	const interrupted = "interrupted with a checkpoint pause"
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "todos_updated" && len(inventoryWorkflowOccurrences(c, "critique")) == 2 {
			panic(interrupted)
		}
	}
	defer func() {
		c.reportStatus = nil
		if value := recover(); value != interrupted {
			t.Fatalf("expected interruption after both critique occurrences were created, got %v", value)
		}
		item := inventoryWorkflowOccurrences(c, "critique")[0]
		if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "fixture started", "", nil); err != nil {
			t.Fatal(err)
		}
		decision := CheckpointDecision{Action: CheckpointNeedsHuman, Detail: "fixture paused"}
		if err := c.projectCheckpointOutcome(&taskDiscipline{todoID: item.ID}, decision); err != nil {
			t.Fatal(err)
		}
	}()
	_, err := c.ExecuteTasks(t.Context(), []TaskDef{task})
	t.Errorf("critique did not reach durable task creation: %v", err)
}

func interruptReportCreation(t *testing.T, c *Coordinator, tasks []TaskDef, position int) {
	t.Helper()
	journal := c.EventJournal().(AtomicEventJournal)
	c.SetEventJournal(rejectReportBatchMember{AtomicEventJournal: journal, store: c.eventStore, position: position, event: EventTaskCreated})
	defer c.SetEventJournal(journal)
	if _, err := c.ExecuteTasks(t.Context(), tasks); err == nil || !strings.Contains(err.Error(), "fixture interrupted report batch") {
		t.Fatalf("expected interrupted report creation at member %d, got %v", position, err)
	}
}

func confirmInventoryBatch(t *testing.T, c *Coordinator, tasks []TaskDef, declined bool, validate func([]TaskDef)) {
	t.Helper()
	want := "fixture confirmation canceled"
	if declined {
		want = "user declined task execution"
	}
	c.SetStepConfirmFn(func(_ context.Context, proposals []TaskDef) (bool, error) {
		validate(proposals)
		if declined {
			return false, nil
		}
		return false, errors.New(want)
	})
	defer c.SetStepConfirmFn(nil)
	if _, err := c.ExecuteTasks(t.Context(), tasks); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("step declined=%t: got %v", declined, err)
	}
}

// A similarity matcher can consider two goals equivalent even when they
// refer to distinct inventory sources. The runtime must own that decision.
type inventoryPriorResultCache struct {
	TaskCache
	goal  string
	prior string
}

func (c inventoryPriorResultCache) Lookup(ctx context.Context, request TaskCacheLookupRequest) (TaskCacheLookupResult, bool) {
	if request.AgentKey == "auditor" && request.Task == c.goal {
		return TaskCacheLookupResult{Output: c.prior}, true
	}
	return c.TaskCache.Lookup(ctx, request)
}

func interruptReportWithdrawal(t *testing.T, c *Coordinator, tasks []TaskDef, failRemoval bool) {
	t.Helper()
	journal := c.EventJournal().(AtomicEventJournal)
	fixture := rejectReportBatchMember{AtomicEventJournal: journal, store: c.eventStore, position: 1, failResource: true}
	if failRemoval {
		fixture.event = EventTaskRemoved
	}
	c.SetEventJournal(fixture)
	defer c.SetEventJournal(journal)
	if _, err := c.ExecuteTasks(t.Context(), tasks); err == nil || !strings.Contains(err.Error(), "fixture interrupted resource claim") ||
		strings.Contains(err.Error(), "fixture interrupted report batch") != failRemoval {
		t.Fatalf("expected pre-dispatch failure with atomic withdrawal failure=%t, got %v", failRemoval, err)
	}
}

type rejectReportBatchMember struct {
	AtomicEventJournal
	store        *EventStore
	position     int
	event        EventType
	failResource bool
}

func (j rejectReportBatchMember) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if j.failResource && event.Type == string(EventResourceClaimsResolved) {
		return RunEvent{}, fmt.Errorf("fixture interrupted resource claim")
	}
	return j.AtomicEventJournal.Append(ctx, event)
}

func (j rejectReportBatchMember) AppendBatchAtHead(ctx context.Context, events []RunEvent, head string) ([]RunEvent, error) {
	if len(events) != 3 || events[0].Type != string(j.event) {
		return j.AtomicEventJournal.AppendBatchAtHead(ctx, events, head)
	}
	db := j.store.store.DB()
	query := fmt.Sprintf(`CREATE TRIGGER refuse_report_member BEFORE INSERT ON events WHEN NEW.idempotency_key = '%s'
		BEGIN SELECT RAISE(ABORT, 'fixture interrupted report batch'); END`, events[j.position].IdempotencyKey)
	if _, err := db.ExecContext(ctx, query); err != nil {
		return nil, err
	}
	appended, err := j.AtomicEventJournal.AppendBatchAtHead(ctx, events, head)
	if _, dropErr := db.ExecContext(ctx, "DROP TRIGGER refuse_report_member"); dropErr != nil {
		return nil, dropErr
	}
	return appended, err
}

func interruptReportWorkflow(t *testing.T, c *Coordinator, tasks []TaskDef) {
	t.Helper()
	const interrupted = "interrupted before report submission"
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "todos_updated" && len(inventoryWorkflowOccurrences(c, "synthesize")) != 0 {
			panic(interrupted)
		}
	}
	defer func() {
		c.reportStatus = nil
		if value := recover(); value != interrupted {
			t.Fatalf("expected interruption after report task creation, got %v", value)
		}
	}()
	_, err := c.ExecuteTasks(t.Context(), tasks)
	t.Errorf("report did not reach durable task creation: %v", err)
}

type inventoryWorkflowProvider func(context.Context, Action) (map[string]any, error)

func (inventoryWorkflowProvider) Validate(Action) error { return nil }

func (p inventoryWorkflowProvider) Execute(ctx context.Context, action Action) (any, error) {
	value, err := p(ctx, action)
	if err != nil {
		return nil, err
	}
	value, err = inventoryFixtureJSON(value)
	return ActionResult{Outputs: value}, err
}

func inventoryFixtureJSON(value map[string]any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	err = json.Unmarshal(data, &normalized)
	return normalized, err
}

func startInventoryWorkflow(t *testing.T, session *TeamSession, source *ResumeSource, calls *[]string) (*Coordinator, func()) {
	t.Helper()
	c, err := NewCoordinator(session, "http://127.0.0.1:1/v1", "", nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	ctx := t.Context()
	if source == nil {
		c.SetSessionData(NewSession())
	} else {
		if err := c.AuthorizeSessionRestore(source); err != nil {
			t.Fatal(err)
		}
		c.SetSessionData(source.Session)
		ctx = WithSessionResume(ctx)
	}
	_, endRun := c.beginInvocationExecutionRun(ctx)
	endRun = sync.OnceFunc(endRun)
	t.Cleanup(endRun)
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, "review frozen evidence"); err != nil {
		t.Fatal(err)
	}
	if err := c.validateResumeSources(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.phaseWorkflow.Start(); err != nil {
		t.Fatal(err)
	}
	c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		ctx = tools.SetToolsAllowed(ctx, []string{submitResultToolName})
		id, _ := ctx.Value(todoIDKey{}).(string)
		item := todoItemByID(c.taskTracker.TodoList().Items(), id)
		*calls = append(*calls, item.ContractID)
		submit := func(body string) (fantasy.ToolResponse, error) {
			return (&submitResultTool{coordinator: c, todoID: id}).Run(ctx, fantasy.ToolCall{ID: "fixture-result", Name: submitResultToolName, Input: body})
		}
		body := `{"status":"success","summary":"checked","structured_payload":{}}`
		if item.ContractID == "synthesize" {
			for _, override := range []string{
				`{"status":"success","summary":"2 sources and 2 critiques","structured_payload":{"details":"frozen evidence"}}`,
				`{"structured_payload":{"details":"frozen evidence","coverage":"complete"}}`,
				`{"structured_payload":{"details":"frozen evidence","audited":true}}`,
			} {
				response, err := submit(override)
				if err == nil && !response.IsError {
					return nil, fmt.Errorf("accepted an override: %s", override)
				}
			}
			body = `{"structured_payload":{"details":"frozen evidence"}}`
		}
		response, err := submit(body)
		if err != nil || response.IsError {
			return nil, fmt.Errorf("submit %s: %s (%v)", item.ContractID, response.Content, err)
		}
		return textAgentResult("submitted"), nil
	}})
	return c, endRun
}

func restartInventoryWorkflow(t *testing.T, previous *Coordinator, endRun func(), session *TeamSession, calls *[]string) (*Coordinator, func()) {
	t.Helper()
	endRun()
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(session.Workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return startInventoryWorkflow(t, session, source, calls)
}

func runInventoryFixtureAction(c *Coordinator, name string, critiqueRequired bool) (map[string]any, error) {
	switch name {
	case "prepare":
		return map[string]any{"ready": true}, nil
	case "inventory":
		var ids []string
		var entries []map[string]string
		for _, contract := range []string{"source-a", "source-b"} {
			item := inventoryWorkflowOccurrences(c, contract)[0]
			ids = append(ids, item.ID)
			entries = append(entries, map[string]string{"task_id": item.ID, "payload_sha256": item.TypedResult.StructuredPayload.SHA256})
		}
		required := []string{}
		if critiqueRequired {
			required = ids
		}
		return map[string]any{"inventory": map[string]any{"all": ids, "required": required, "sources": entries}}, nil
	case "derive":
		summary := fmt.Sprintf("2 sources and %d critiques", len(inventoryWorkflowOccurrences(c, "critique")))
		return map[string]any{"coverage": "complete", "completion": map[string]any{"status": "success", "summary": summary}}, nil
	case "audit":
		report := inventoryWorkflowOccurrences(c, "synthesize")[0]
		wantEvidence := 2 + len(inventoryWorkflowOccurrences(c, "critique"))
		return map[string]any{"passed": report.Status == TaskDone && report.TypedResult.StructuredPayload != nil && len(report.EvidenceFrom) == wantEvidence}, nil
	default:
		return nil, fmt.Errorf("unexpected fixture action %s", name)
	}
}

func inventoryWorkflowOccurrences(c *Coordinator, id string) []*TodoItem {
	var matches []*TodoItem
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ContractID == id {
			matches = append(matches, item)
		}
	}
	return matches
}

type interruptInventoryDispatch struct {
	EventJournal
	count int
}

func (j *interruptInventoryDispatch) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == string(EventTaskCreated) {
		j.count++
		if j.count == 2 {
			return RunEvent{}, fmt.Errorf("interrupted inventory dispatch")
		}
	}
	return j.EventJournal.Append(ctx, event)
}

func compileFixtureJSONSchema(t *testing.T, name, data string) *CompiledResultContract {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	compiled, err := compileResultContractSchema(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}
