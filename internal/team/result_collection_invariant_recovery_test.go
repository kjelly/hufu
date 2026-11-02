package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResultCollectionRecoveryRepairsProjectedInvariantBounds(t *testing.T) {
	var document map[string]any
	if err := json.Unmarshal([]byte(projectionTestSchema), &document); err != nil {
		t.Fatal(err)
	}
	document["properties"].(map[string]any)["assessments"] = invariantAssessmentClaimsSchema(false)
	rawSchema, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileResultContractDocument("projected.json", rawSchema)
	if err != nil {
		t.Fatal(err)
	}
	review := TaskDef{ID: "review", Agent: "worker", Phase: PhaseVerify, InvariantVerification: InvariantVerificationReport,
		SideEffect: SideEffectNone, Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}, ResultContract: new(compiled.ref(true))}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		prompt := fmt.Sprint(request["messages"])
		if !strings.Contains(prompt, `"maxLength":256`) {
			t.Error("collector prompt lost the missing-evidence length bound")
		}
		if tools, _ := request["tools"].([]any); len(tools) != 0 {
			t.Error("result-only collection exposed worker tools")
		}
		n := calls.Add(1)
		if n > 3 {
			t.Error("repair exceeded its bounded model budget")
			http.Error(w, "extra turn", http.StatusBadRequest)
			return
		}
		if n == 2 {
			for _, pointer := range []string{"assessments/0/missing_evidence/0", "assessments/0/missing_evidence/1"} {
				if !strings.Contains(prompt, pointer) {
					t.Errorf("repair omitted a simultaneous violation: %s", pointer)
				}
			}
		}
		first, second := collectionInvariantSummary(288), collectionInvariantSummary(262)
		if n == 2 {
			first = "focused boundary test unavailable"
		}
		if n == 3 {
			first, second = "focused boundary test unavailable", "consumer definition outside assigned snapshot"
		}
		body := fmt.Sprintf(`{"status":"completed_with_gaps","summary":"bounded review completed","structured_payload":{"findings":[{"summary":"retained observation","severity":"info"}],"assessments":[{"invariant_id":"safe","status":"unknown","summary":"Boundary evidence is incomplete","missing_evidence":[%q,%q]}]}}`, first, second)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"invariants\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.Config.Delegation.BindTaskGoalContracts = true
		c.session.ContractTasks = []TaskDef{review}
		c.session.Config.MaxRetries = 0
		c.session.Agents["worker"].MaxRetries = 0
		c.session.InvariantCatalog = []InvariantDefinition{{ID: "safe", Statement: "Preserve the declared boundary", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
		c.session.AgentResultContracts = map[string]ResultContractRef{"worker": compiled.ref(true)}
	})
	worker := scriptWorker(textTurn("Review completed with one observation and two evidence gaps. Retain both gaps and the observation."))
	c.workerAgentOverride = worker
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.delegatedTasks = make(map[string]int)
	testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
	_, err = c.ExecuteTasks(t.Context(), []TaskDef{{ContractID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review boundary"}})
	item := c.taskTracker.TodoList().Items()[1]
	cp := currentResultCollection(item)
	if err == nil || item.Status != TaskBlocked || cp == nil || cp.TurnsUsed != 2 || worker.Calls() != 1 || cp.FailureReason != RepairFailureInvalidSchema {
		t.Fatalf("original rejection: status=%s detail=%s repair=%+v error=%v", item.Status, item.Detail, item.ExecutionReceipt.RepairProvenance, err)
	}
	originalWork, err := c.loadCollectionWork(t.Context(), item, cp)
	if err != nil {
		t.Fatal(err)
	}
	c = reopenRecoveryCoordinator(t, c)
	// Public resume reapplies hash-verified static contracts to legacy event
	// projections. Exercise that same boundary rather than a raw reducer alone.
	c.applyLiveTaskProjection(c.taskTracker.TodoList().Items())
	if c.todoItemByID(item.ID).InvariantVerification != InvariantVerificationReport {
		t.Fatal("restart lost the original invariant contract")
	}
	before, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
	if err != nil || a.Disposition != RecoveryRepairClaims || a.AdditionalTurns != 2 || validateRecoveryAssessment(a) != nil {
		t.Fatalf("projected format rejection was not repairable: %+v error=%v", a, err)
	}
	after, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) || calls.Load() != 2 {
		t.Fatal("read-only assessment mutated evidence or called a provider")
	}
	c.executionRunID = "result-recovery"
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
		t.Fatal(err)
	}
	c = reopenRecoveryCoordinator(t, c)
	c.applyLiveTaskProjection(c.taskTracker.TodoList().Items())
	c.executionRunID = "result-recovery"
	if c.todoItemByID(item.ID).InvariantVerification != InvariantVerificationReport {
		t.Fatal("repair grant lost the original invariant contract")
	}
	unusedWorker := scriptWorker(textTurn("must never replay work"))
	c.workerAgentOverride = unusedWorker
	if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID); err != nil {
		t.Fatal(err)
	}
	done := c.todoItemByID(item.ID)
	committed := currentResultCollection(done)
	if done.Status != TaskDone || unusedWorker.Calls() != 0 || calls.Load() != 3 || committed.Work != cp.Work {
		t.Fatal("repair repeated original work or changed its immutable evidence")
	}
	work, err := c.loadCollectionWork(t.Context(), done, committed)
	if err != nil || !reflect.DeepEqual(work.Receipt, originalWork.Receipt) || string(work.Report) != string(originalWork.Report) {
		t.Fatal("repair changed the original receipt or report")
	}
	result := done.TypedResult
	if result == nil || result.Status != TaskResultStatusCompletedWithGaps || len(result.Findings) != 1 || result.Findings[0].Summary != "retained observation" || result.InvariantVerification.Assessments[0].Status != InvariantUnknown || len(result.InvariantVerification.Assessments[0].MissingEvidence) != 2 {
		t.Fatal("format repair changed a finding, invariant judgment, or gap membership")
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList(events)
	index := slices.IndexFunc(replayed, func(task *TodoItem) bool { return task.ID == item.ID })
	if index < 0 || replayed[index].Status != TaskDone || !reflect.DeepEqual(replayed[index].TypedResult, result) {
		t.Fatal("replay changed the accepted result")
	}
}
