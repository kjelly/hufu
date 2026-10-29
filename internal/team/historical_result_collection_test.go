package team

import (
	"bytes"
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

	"github.com/kjelly/hufu/internal/agent"
)

func TestHistoricalResultCollectionPreservesOriginalDelivery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if strings.Contains(fmt.Sprint(request["messages"]), "complete deliverable") {
			t.Error("result collector automatically received the worker's historical source body")
		}
		body := `{"status":"success","summary":"historical evidence assessed"}`
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"historical-claims\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body)
	}))
	t.Cleanup(server.Close)
	c, item, _ := configuredHistoricalContextFixture(t, func(c *Coordinator) {
		c.session.Config.ResultTransport = agent.ResultTransportRuntime
		manager, err := agent.NewProviderManager(server.URL+"/v1", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.providerManager = manager
		c.modelProfileRuntime = NewModelProfileRuntime(manager, false)
	})
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	worker := scriptWorker(textTurn("Historical evidence assessed; analysis complete."))
	c.workerAgentOverride = worker
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	current := c.todoItemByID(item.ID)
	if current.Status != TaskDone || current.TypedResult == nil || current.TypedResult.Source != "collected" || worker.Calls() != 1 || calls.Load() != 1 {
		t.Fatalf("historical collection = status %s, worker calls %d, collection calls %d", current.Status, worker.Calls(), calls.Load())
	}
	if err := c.validateHistoricalResultDelivery(item.ID, 1, c.executionRunID); err != nil {
		t.Fatal(err)
	}
	work, err := c.loadCollectionWork(t.Context(), current, currentResultCollection(current))
	if err != nil || !strings.Contains(string(work.Input), "complete deliverable") {
		t.Fatalf("collector projection changed the durable original input: %v", err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil || len(replayed) != 1 || !reflect.DeepEqual(replayed[0].HistoricalResultBinding, current.HistoricalResultBinding) {
		t.Fatalf("collection replay changed original historical delivery: %v", err)
	}
	// Replay merges manifests by identity; collection and worker entries have
	// no ordering contract. Compare each complete canonical entry by request.
	wantManifests, gotManifests := slices.Clone(current.ContextManifests), slices.Clone(replayed[0].ContextManifests)
	compare := func(a, b ContextInjectionManifest) int { return strings.Compare(a.RequestID, b.RequestID) }
	slices.SortFunc(wantManifests, compare)
	slices.SortFunc(gotManifests, compare)
	want, err := json.Marshal(wantManifests, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(gotManifests, json.Deterministic(true))
	if err != nil || !bytes.Equal(want, got) {
		t.Fatalf("collection replay changed canonical manifest bytes: %v\nwant %s\ngot %s", err, want, got)
	}
}
