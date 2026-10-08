package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

type evidenceAuditCaptureAgent struct {
	c      *Coordinator
	prompt string
}

func (a *evidenceAuditCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.prompt = call.Prompt
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	response, err := (&submitResultTool{coordinator: a.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"audit delivered inputs"}`})
	if err != nil {
		return nil, err
	}
	if response.IsError {
		return nil, fmt.Errorf("submit audit result: %s", response.Content)
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "submitted"}}}}, nil
}

func (a *evidenceAuditCaptureAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt})
}

func TestExecuteWorkerReceivesAcceptedEvidenceBeforeSuccessfulAudit(t *testing.T) {
	c, source, audit, _, _ := evidenceDeliveryFixture(t)
	c.session.Workspace = t.TempDir()
	c.session.Agents = map[string]*agent.AgentDef{"auditor": {Name: "auditor", Role: "worker", Tools: "view", SideEffect: "none", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: -1}}
	audit.ContextManifests = nil
	audit.Execution.RequiresResult = true
	worker := &evidenceAuditCaptureAgent{c: c}
	c.workerAgentOverride = worker
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(audit), audit.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(worker.prompt, string(source.TypedResult.StructuredPayload.Value)) {
		t.Fatal("actual worker request lost the accepted dependency payload")
	}
	if got := c.todoItemByID(audit.ID); got.Status != TaskDone || got.TypedResult == nil {
		t.Fatalf("valid audit failed completion: %#v", got)
	}
}

func evidenceDeliveryFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, WorkerContextInput, CompiledContext) {
	t.Helper()
	contract := evidenceTestContract(t)
	contract.finalReport = nil
	ref := contract.ref(true)
	payload, err := validateStructuredResultPayload(contract, ref, []byte(evidenceTestPayload))
	if err != nil {
		t.Fatal(err)
	}
	c := newBudgetCoordinator(t)
	c.executionRunID = "run-delivery"
	c.session.ResultContracts = map[string]*CompiledResultContract{contract.ID: contract}
	source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", ResultContract: &ref}})[0]
	source.Status = TaskDone
	source.TypedResult = &TaskResult{TaskID: source.ID, Agent: source.Agent, Status: TaskResultStatusSuccess, Summary: "Generic evidence warning", StructuredPayload: payload}
	audit := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "audit supplied evidence", Goal: "audit supplied evidence", EvidenceFrom: []string{source.ID}, Execution: ExecutionContract{RequiresEvidence: true}}})[0]
	input := WorkerContextInput{Goal: "audit supplied evidence", TaskDef: taskDefFromTodoItem(audit), DependencyResults: c.dependencyResultsForTask(audit.ID)}
	compiled, err := CompileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	request := c.newTaskContextRequest(input.TaskDef, audit.ID, 1, ContextTriggerTaskDispatch, audit.Agent, "worker", nil)
	manifest, err := BuildContextInjectionManifest(request, compiled, nil, audit.Agent, time.Now(), agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().SetContextManifest(audit.ID, &manifest); err != nil {
		t.Fatal(err)
	}
	return c, source, audit, input, compiled
}

func TestRequiredEvidenceDeliversDowngradedPayloadAndSealedHash(t *testing.T) {
	c, source, audit, _, compiled := evidenceDeliveryFixture(t)
	for _, want := range []string{string(source.TypedResult.StructuredPayload.Value), source.TypedResult.StructuredPayload.SHA256, `"verdict":"unverified"`, "Original limitation.", "evidence downgrades 1"} {
		if !strings.Contains(compiled.Prompt, want) {
			t.Fatalf("worker handoff lacks %q: %s", want, compiled.Prompt)
		}
	}
	if strings.Contains(compiled.Prompt, `"verdict":"supported"`) {
		t.Fatal("handoff resurrected the pre-downgrade claim")
	}
	if err := c.validateRequiredEvidenceDelivery(audit.ID, 1, c.executionRunID); err != nil {
		t.Fatal(err)
	}
	response, err := (&submitResultTool{coordinator: c, todoID: audit.ID}).Run(occurrenceTestContext(c, audit.ID, 1), fantasy.ToolCall{Input: `{"status":"success","summary":"audit complete"}`})
	if err != nil || response.IsError || c.GetTaskResult(audit.ID) == nil {
		t.Fatalf("complete evidence handoff rejected: %#v %v", response, err)
	}
}

func TestRequiredEvidenceNeverUsesTruncatedCoordinatorPreview(t *testing.T) {
	longValue := `{"details":"` + strings.Repeat("x", resultPayloadContextMaxBytes+1) + `","end":"retained"}`
	input := WorkerContextInput{
		Goal: "audit", TaskDef: TaskDef{EvidenceFrom: []string{"1"}, Execution: ExecutionContract{RequiresEvidence: true}},
		DependencyResults: []TaskResult{{TaskID: "1", Status: TaskResultStatusSuccess, StructuredPayload: &ResultPayload{Value: json.RawMessage(longValue)}}},
	}
	compiled, err := CompileWorkerContext(t.Context(), input)
	if err != nil || !strings.Contains(compiled.Prompt, longValue) {
		t.Fatalf("full payload was not delivered: %v", err)
	}
	input.ModelContext = ModelContextSpec{ContextWindow: 512, MaxOutputTokens: 128}
	if _, err := CompileWorkerContext(t.Context(), input); err == nil {
		t.Fatal("required payload silently truncated to fit budget")
	}
}

func TestRequiredEvidenceRejectsMissingAndModifiedContext(t *testing.T) {
	_, _, _, input, compiled := evidenceDeliveryFixture(t)
	for _, name := range []string{"missing_input", "missing_item", "missing_prompt", "compressed", "changed"} {
		t.Run(name, func(t *testing.T) {
			candidate := compiled
			candidate.IncludedItems = append([]ContextItem(nil), compiled.IncludedItems...)
			candidateInput := input
			switch name {
			case "missing_input":
				candidateInput.DependencyResults = nil
			case "missing_item":
				candidate.IncludedItems = nil
			case "missing_prompt":
				candidate.Prompt = "summary only"
			default:
				for i := range candidate.IncludedItems {
					if candidate.IncludedItems[i].ID == "dependency_results" {
						if name == "compressed" {
							candidate.IncludedItems[i].Compressed = true
						} else {
							candidate.IncludedItems[i].Content = "summary only"
						}
					}
				}
			}
			if err := validateRequiredEvidenceContext(candidateInput, candidate); err == nil {
				t.Fatal("incomplete required evidence context accepted")
			}
		})
	}
}

func TestRequiredEvidenceSubmissionFailsClosedButKeepsIncompleteStatuses(t *testing.T) {
	for _, status := range []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusBlocked} {
		t.Run(status, func(t *testing.T) {
			c, _, audit, _, _ := evidenceDeliveryFixture(t)
			audit.ContextManifests = nil
			response, err := (&submitResultTool{coordinator: c, todoID: audit.ID}).Run(occurrenceTestContext(c, audit.ID, 1), fantasy.ToolCall{Input: `{"status":"` + status + `","summary":"missing input"}`})
			if err != nil || response.IsError != taskResultStatusIsSuccessful(status) {
				t.Fatalf("status %s response=%#v err=%v", status, response, err)
			}
			if taskResultStatusIsSuccessful(status) && c.GetTaskResult(audit.ID) != nil {
				t.Fatal("failed admission published a successful result")
			}
		})
	}
}

func TestRequiredEvidenceManifestIntegrityAndScope(t *testing.T) {
	for _, name := range []string{"legacy", "omitted", "compressed", "wrong_hash", "wrong_task", "wrong_attempt", "wrong_agent", "wrong_run", "repair", "tampered", "fanout_valid", "fanout_missing", "agent_case"} {
		t.Run(name, func(t *testing.T) {
			c, _, audit, _, _ := evidenceDeliveryFixture(t)
			manifest := &audit.ContextManifests[0]
			switch name {
			case "legacy", "omitted", "compressed", "wrong_hash":
				for i := range manifest.Items {
					entry := &manifest.Items[i]
					if entry.Kind != "dependency_result" {
						continue
					}
					switch name {
					case "legacy":
						entry.ContentHash = ""
					case "omitted":
						entry.Included = false
					case "compressed":
						entry.Compressed = true
					case "wrong_hash":
						entry.ContentHash = hashContentKey("summary only")
					}
				}
			case "wrong_task":
				manifest.TaskID = "other"
			case "wrong_attempt":
				manifest.Attempt++
			case "wrong_agent":
				manifest.Agent = "other"
			case "agent_case":
				manifest.Agent = strings.ToUpper(manifest.Agent)
			case "wrong_run":
				manifest.RunID = "other"
			case "repair":
				manifest.Trigger = ContextTriggerRepair
			case "tampered":
				manifest.RequestHash = "forged"
			case "fanout_valid", "fanout_missing":
				extra := *cloneContextInjectionManifest(manifest)
				extra.ModelExecutionID = "other-model"
				if name == "fanout_missing" {
					extra.Items = nil
				}
				extra.Fingerprint = contextManifestFingerprint(extra)
				audit.ContextManifests = append(audit.ContextManifests, extra)
			}
			if name != "tampered" {
				audit.ContextManifests[0].Fingerprint = contextManifestFingerprint(audit.ContextManifests[0])
			}
			err := c.validateRequiredEvidenceDelivery(audit.ID, 1, c.executionRunID)
			if (err == nil) != (name == "fanout_valid" || name == "agent_case") {
				t.Fatalf("delivery admission %s = %v", name, err)
			}
		})
	}
}

func TestRequiredEvidenceDeliverySurvivesReplayAndBlocksLegacyFinish(t *testing.T) {
	c, _, audit, _, _ := evidenceDeliveryFixture(t)
	var events []RunEvent
	for _, item := range c.taskTracker.TodoList().Items() {
		payload, err := json.Marshal(taskTransitionPayload(item))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload})
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker = &TaskTracker{todo: &TodoList{items: replayed}}
	if err := c.validateRequiredEvidenceDelivery(audit.ID, 1); err != nil {
		t.Fatalf("replay lost the proof of delivery: %v", err)
	}
	for _, item := range replayed {
		if item.ID == audit.ID {
			item.Status = TaskDone
			item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess, Attempt: 1, Summary: "legacy complete audit"}
			item.ContextManifests = nil
		}
	}
	response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"complete audit"}`})
	if err != nil || !response.IsError || c.finishCalled.Load() || !strings.Contains(response.Content, "required evidence") {
		t.Fatalf("legacy complete audit escaped finish gate: %#v %v", response, err)
	}
}

func TestEvidenceDispatchRejectsCorruptOrMissingPayload(t *testing.T) {
	for _, name := range []string{"missing", "hash", "schema"} {
		t.Run(name, func(t *testing.T) {
			c, source, audit, _, _ := evidenceDeliveryFixture(t)
			switch name {
			case "missing":
				source.TypedResult.StructuredPayload = nil
			case "hash":
				source.TypedResult.StructuredPayload.SHA256 = "forged"
			case "schema":
				source.ResultContract.SchemaSHA256 = "drifted"
			}
			if _, err := c.bindEvidenceSources([]TaskDef{taskDefFromTodoItem(audit)}); err == nil {
				t.Fatal("invalid dependency payload admitted")
			}
		})
	}
}

func TestRequiredEvidenceCannotBypassThroughSidecarOrTerminalTransition(t *testing.T) {
	c, _, audit, input, _ := evidenceDeliveryFixture(t)
	input.TaskDef.Sidecar = true
	if err := c.validateSidecarTaskContract(input.TaskDef); err == nil {
		t.Fatal("tool-less sidecar accepted a required evidence audit")
	}
	audit.Status = TaskInProgress
	audit.ContextManifests = nil
	if err := c.CommitTaskTransition(t.Context(), audit.ID, TaskInProgress, TaskDone, "complete", "complete", nil); err == nil {
		t.Fatal("fast-path terminal transition bypassed required evidence delivery")
	}
	if c.todoItemByID(audit.ID).Status == TaskDone {
		t.Fatal("rejected terminal transition changed the canonical state")
	}
}

func TestRequiredEvidenceTerminalGateUsesActualRetryAttempt(t *testing.T) {
	c, _, audit, _, _ := evidenceDeliveryFixture(t)
	audit.Status = TaskInProgress
	manifest := &audit.ContextManifests[0]
	manifest.Attempt = 2
	manifest.Trigger = ContextTriggerRetry
	manifest.Purpose = contextPurposeForTrigger(ContextTriggerRetry)
	manifest.Fingerprint = contextManifestFingerprint(*manifest)
	c.setCurrentTaskAttempt(audit.ID, 2)
	if err := c.CommitTaskTransition(t.Context(), audit.ID, TaskInProgress, TaskDone, "complete", "complete", nil); err != nil {
		t.Fatalf("valid second attempt was checked against first attempt: %v", err)
	}
}
