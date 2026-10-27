package team

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestResultCollectorPreservesWorkWhenBudgetOrCancellationStopsReporting(t *testing.T) {
	for _, scenario := range []string{"cancelled", "token_budget"} {
		t.Run(scenario, func(t *testing.T) {
			var modelCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/show":
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
				case "/api/ps":
					_, _ = fmt.Fprint(w, `{"models":[{"name":"test","context_length":131072}]}`)
				case "/v1/chat/completions":
					modelCalls.Add(1)
					http.Error(w, "unexpected model call", 500)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			} else {
				c.SetBudget(0, 1)
			}
			if err := c.collectWorkResult(ctx, c.session.Agents["worker"], checkpoint); err == nil {
				t.Fatal("reporting bypassed its budget/cancellation gate")
			}
			current := c.todoItemByID(item.ID)
			checkpoint = current.ExecutionReceipt.ResultCollection
			if modelCalls.Load() != 0 || current.TypedResult != nil || checkpoint.TurnsUsed > 1 {
				t.Fatalf("unbounded reporting: calls=%d checkpoint=%+v", modelCalls.Load(), checkpoint)
			}
			work, err := c.loadCollectionWork(t.Context(), current, checkpoint)
			if err != nil || string(work.Report) != "complete work report" || work.Receipt.TranscriptRef != receipt.TranscriptRef {
				t.Fatalf("reporting stop lost sealed work: %v", err)
			}
		})
	}
}
