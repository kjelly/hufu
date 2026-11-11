package team

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func TestResultCollectionResponseChannelsPreserveAdmissionAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         agent.CollectorResponseChannel
		first        string
		text         string
		wantText     bool
		wantCalls    int32
		wantAccepted bool
	}{
		{"automatic fallback", agent.CollectorResponseAuto, `{"result":"broken JSON"}`, `{"status":"success","summary":"saved report"}`, true, 2, true},
		{"fallback still validates", agent.CollectorResponseAuto, `{"result":[]}`, `{"status":"invented","summary":"saved report"}`, true, 2, false},
		{"fallback rejects prose", agent.CollectorResponseAuto, `{"result":null}`, `analysis {"status":"success","summary":"saved report"}`, true, 2, false},
		{"semantic rejection stays on function", agent.CollectorResponseAuto, `{"result":{"status":"invented","summary":"saved report"}}`, "", false, 2, false},
		{"explicit function", agent.CollectorResponseFunction, `{"result":"broken JSON"}`, "", false, 2, false},
		{"legacy function", "", `{"result":"broken JSON"}`, "", false, 2, false},
		{"explicit text", agent.CollectorResponseTextJSON, "", `{"status":"success","summary":"saved report"}`, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				n := calls.Add(1)
				if n > tc.wantCalls {
					t.Error("collector exceeded bounded calls")
					http.Error(w, "extra call", http.StatusBadRequest)
					return
				}
				textChannel := tc.wantText && (tc.mode == agent.CollectorResponseTextJSON || n == 2)
				w.Header().Set("Content-Type", "text/event-stream")
				if textChannel {
					if tools, _ := request["tools"].([]any); len(tools) != 0 {
						t.Error("text channel exposed tools")
					}
					if choice, ok := request["tool_choice"]; ok && choice != "none" {
						t.Error("text channel allowed tool choice")
					}
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"text\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\ndata: {\"id\":\"text\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", tc.text)
					return
				}
				assertCollectorResponseChannel(t, request)
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"function\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"function\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", tc.first)
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
				c.session.Config.CollectorResponseChannel = tc.mode
			})
			worker := scriptWorker(textTurn("must not repeat work"))
			c.workerAgentOverride = worker
			err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "saved report", "original task")
			if (err == nil) != tc.wantAccepted || calls.Load() != tc.wantCalls || worker.Calls() != 0 {
				t.Fatalf("accepted=%v calls=%d worker=%d err=%v", err == nil, calls.Load(), worker.Calls(), err)
			}
			if !tc.wantAccepted && !strings.Contains(err.Error(), "model or an upstream provider/adapter") {
				t.Fatal("terminal collector error lost the possible model/provider cause hint")
			}
			item = c.todoItemByID(item.ID)
			checkpoint := currentResultCollection(item)
			if checkpoint.TurnsUsed != int(tc.wantCalls) || checkpoint.SourceRunID != receipt.RunID || checkpoint.ModelExecutionID != receipt.ModelExecutionID {
				t.Fatal("channel selection changed work identity or model budget")
			}
			if tc.wantAccepted {
				accepted, err := c.acceptedCollectionResult(t.Context(), item, checkpoint)
				if err != nil || accepted.Summary != "saved report" {
					t.Fatalf("canonical admission: %v", err)
				}
			}
			c = reopenRecoveryCoordinator(t, c)
			checkpoint = currentResultCollection(c.todoItemByID(item.ID))
			if tc.wantText {
				channel, err := c.collectionResponseChannel(t.Context(), checkpoint, modelprofile.CapabilityYes)
				if err != nil || channel != agent.CollectorResponseTextJSON {
					t.Fatalf("restart lost text channel: %s %v", channel, err)
				}
			}
			if tc.mode == agent.CollectorResponseAuto && tc.wantText && tc.wantCalls == 2 {
				events, err := c.EventJournal().ReadEvents(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				foundOriginal := false
				for _, event := range events {
					if event.Type != string(EventTaskResultCollectionCheckpointed) {
						continue
					}
					var payload resultCollectionEvent
					if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
						t.Fatal(err)
					}
					prior := payload.Receipt.ResultCollection
					if prior.TurnsUsed == 1 && prior.State == ResultCollectionReceived {
						blob, err := c.readCollectionBlobRecord(t.Context(), prior, "response", prior.Response)
						if err == nil && strings.Contains(string(blob.Data), tc.first) {
							foundOriginal = true
						}
					}
				}
				if !foundOriginal {
					t.Fatal("channel fallback lost original rejected arguments")
				}
			}
		})
	}
}

func TestResultCollectionChannelFallbackSurvivesRestartBeforeDispatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
	}))
	t.Cleanup(server.Close)
	c, item, ctx, receipt := resultCollectionProviderFixture(t, server.URL+"/v1", true, func(c *Coordinator) {
		c.session.Config.CollectorResponseChannel = agent.CollectorResponseAuto
	})
	checkpoint, err := c.beginResultCollection(ctx, item.ID, receipt, "saved report")
	if err != nil {
		t.Fatal(err)
	}
	_, request, err := c.prepareCollectionInvocation(ctx, c.session.Agents[item.Agent], checkpoint, "first-function", "")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "first-function", request)
	if err != nil {
		t.Fatal(err)
	}
	channel, err := c.collectionResponseChannel(t.Context(), checkpoint, modelprofile.CapabilityYes)
	if err != nil || channel != agent.CollectorResponseFunction {
		t.Fatalf("missing response triggered fallback: %s %v", channel, err)
	}
	raw := `{"result":"malformed claims"}`
	checkpoint, err = c.recordCollectionResponseUsage(ctx, checkpoint, []byte(raw), ExecutionUsage{}, &agent.StructuredResponseIssue{Code: "type", Pointer: "/result", Expected: "object", Actual: "string"})
	if err != nil {
		t.Fatal(err)
	}
	work := checkpoint.Work
	c = reopenRecoveryCoordinator(t, c)
	checkpoint = currentResultCollection(c.todoItemByID(item.ID))
	channel, err = c.collectionResponseChannel(t.Context(), checkpoint, modelprofile.CapabilityYes)
	if err != nil || channel != agent.CollectorResponseTextJSON || checkpoint.TurnsUsed != 1 || checkpoint.Work != work {
		t.Fatalf("restart changed channel/budget/work: %s %v", channel, err)
	}
	c.session.Config.CollectorResponseChannel = agent.CollectorResponseFunction
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.collectionResponseChannel(t.Context(), checkpoint, modelprofile.CapabilityYes); err == nil {
		t.Fatal("current configuration bypassed frozen channel policy")
	}
}
