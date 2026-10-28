package team

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Collected claims may carry the advertised verification entries. json/v2 has
// no default time.Duration representation and rejects even the omitted zero,
// so these claims must use the tool transport's integer-nanosecond form.
func TestResultCollectionCommitsVerificationEntries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		verification string
		duration     time.Duration
	}{
		{name: "without_duration", verification: `{"command":"view artifact_ref=report","exit_code":0,"stdout":"verification passed"}`},
		{name: "with_duration", verification: `{"command":"go test ./...","exit_code":0,"duration":1500000000}`, duration: 1500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := `{"status":"success","summary":"review complete","verification":[` + tc.verification + `]}`
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				if calls.Add(1) > 1 {
					t.Error("valid claims needed a repair turn")
					http.Error(w, "extra call", 400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"claims\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", claims)
			}))
			t.Cleanup(server.Close)
			c, _, _, _ := resultCollectionProviderFixture(t, server.URL+"/v1", true)
			if _, err := c.decodeCollectionClaims(c.taskTracker.TodoList().Items()[0].ID, []byte(claims)); err != nil {
				t.Fatalf("advertised verification entry rejected: %v", err)
			}
			worker := scriptWorker(textTurn("Review complete; the verification report passed."))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			_, err := c.ExecuteTasks(t.Context(), []TaskDef{{ID: "review", Agent: "worker", Model: "ollama/test", Goal: "Review findings", SideEffect: SideEffectNone,
				Execution: ExecutionContract{RequiresResult: true, ForbidArtifacts: true}}})
			item := c.taskTracker.TodoList().Items()[1]
			checkpoint := currentResultCollection(item)
			if err != nil || item.Status != TaskDone || checkpoint == nil || checkpoint.State != ResultCollectionCommitted {
				t.Fatalf("status=%s detail=%s checkpoint=%+v error=%v", item.Status, item.Detail, checkpoint, err)
			}
			if worker.Calls() != 1 || calls.Load() != 1 {
				t.Fatalf("work=%d collection=%d", worker.Calls(), calls.Load())
			}
			assertVerification := func(source string, result *TaskResult) {
				t.Helper()
				if result == nil || len(result.Verification) != 1 || result.Verification[0].Duration != tc.duration || result.Verification[0].ExitCode != 0 {
					t.Fatalf("%s verification=%+v", source, result)
				}
			}
			assertVerification("accepted", item.TypedResult)
			reloaded, err := c.acceptedCollectionResult(t.Context(), item, checkpoint)
			if err != nil {
				t.Fatalf("accepted bytes cannot be reloaded: %v", err)
			}
			assertVerification("reloaded", reloaded)
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == string(EventTaskResultCollectionCheckpointed) {
					if err := validateResultCollectionEvent(event); err != nil {
						t.Fatalf("durable checkpoint rejected on replay: %v", err)
					}
				}
			}
			replayed := ReduceToTodoList(events)[1]
			if replayed.Status != TaskDone || currentResultCollection(replayed).State != ResultCollectionCommitted {
				t.Fatalf("replay status=%s", replayed.Status)
			}
			assertVerification("replayed", replayed.TypedResult)
		})
	}
}
