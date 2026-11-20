package team

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestResultCollectionRaisesOutputLimitOnlyForNewGrantedTurns(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 262144","capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			return
		}
		assertCollectorResponseChannel(t, request)
		n := calls.Add(1)
		wantTokens := 32768
		if n == 3 {
			wantTokens = 65536
		}
		if request["max_tokens"] != float64(wantTokens) || n > 3 {
			t.Errorf("collector call %d output limit=%v; want %d", n, request["max_tokens"], wantTokens)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n < 3 {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		arguments := `{"result":{"status":"success","summary":"original report"}}`
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
	}))
	t.Cleanup(server.Close)
	c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.Agents["worker"].Generation.MaxTokens = "32768"
	})
	err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "original report", "original work")
	failure, ok := errors.AsType[*resultCollectionFailure](err)
	if !ok || failure.Checkpoint == nil || calls.Load() != 2 || failure.Checkpoint.TurnsUsed != 2 {
		t.Fatalf("original collection did not exhaust two turns: %v", err)
	}
	original := failure.Checkpoint
	c.recordCollectionStopReceipt(item.ID, failure)
	c.PersistFailureWithClassAndStatus(item.Agent, item.Goal, item.ID, failure.Error(), NeedsHuman, FailureProtocol, TaskBlocked)
	repair := WithResultRepairMaxOutput(WithResultOnlyRepair(t.Context(), item.ID), 65536)
	a, err := c.AssessResultCollectionRecovery(repair, item.ID)
	if err != nil || validateRecoveryAssessment(a) != nil || a.Disposition != RecoveryRepairClaims ||
		a.MaxOutputTokens != 65536 || a.AdditionalTurns != 2 || calls.Load() != 2 {
		t.Fatalf("preview did not grant a bounded collector-only increase: %+v, %v", a, err)
	}
	if smaller, err := c.AssessResultCollectionRecovery(WithResultRepairMaxOutput(repair, 32768), item.ID); err != nil || smaller.Disposition != RecoveryBlocked {
		t.Fatalf("preview admitted an unchanged output limit: %+v, %v", smaller, err)
	}
	c.executionRunID = "repaired-collection"
	if err := c.authorizeOperatorResultRepair(repair); err != nil {
		t.Fatal(err)
	}
	worker := scriptWorker(textTurn("must not replay worker tools"))
	c.workerAgentOverride = worker
	if _, err := c.executeTask(repair, taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID); err != nil {
		t.Fatal(err)
	}
	done := c.todoItemByID(item.ID)
	cp := currentResultCollection(done)
	if done.Status != TaskDone || calls.Load() != 3 || worker.Calls() != 0 || cp.Work != original.Work ||
		cp.TurnsUsed != 3 || cp.TurnLimit != 4 {
		t.Fatal("recovery changed work, repeated the worker, or reset consumed turns")
	}
	reopened := reopenRecoveryCoordinator(t, c)
	if durable := currentResultCollection(reopened.todoItemByID(item.ID)); !sameCanonicalEvidence(durable, cp) {
		t.Fatal("replay changed the recovered checkpoint")
	}
}
