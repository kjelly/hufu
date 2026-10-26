package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func repairDiagnosticTurn(reason fantasy.FinishReason) scriptedTurn {
	return scriptedTurn{stream: func(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{
			FinishReason: reason,
			Usage:        fantasy.Usage{InputTokens: 200, OutputTokens: 100, ReasoningTokens: 90},
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "report ready"},
				fantasy.ReasoningContent{Text: "private reasoning"},
			},
		}}}}, nil
	}}
}

func TestProtocolRepairGenerationDiagnosticsPersist(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "resumed"}[resumed], func(t *testing.T) {
			c, id, task := toolEvidenceRepairFixture(t)
			c.repairAgentOverride = scriptWorker(repairDiagnosticTurn(fantasy.FinishReasonLength), repairDiagnosticTurn(fantasy.FinishReasonStop))
			ctx := withTestProtocolRepairInvocationContext(t.Context())
			if resumed {
				item := interruptedToolEvidenceFixture(t, c, id, task, 2)
				if _, err := c.resumeProtocolIncompleteTask(ctx, task, item); err == nil {
					t.Fatal("missing submission was accepted")
				}
			} else {
				c.workerAgentOverride = scriptWorker(observedDocumentTurn(t))
				if _, err := c.executeTask(ctx, task, id); err == nil {
					t.Fatal("missing submission was accepted")
				}
			}
			item := todoByID(t, c, id)
			history := item.ExecutionReceipt.RepairProvenance.History
			if len(history) != 2 {
				t.Fatalf("repair history count = %d", len(history))
			}
			for i, reason := range []fantasy.FinishReason{fantasy.FinishReasonLength, fantasy.FinishReasonStop} {
				d := history[i].Generation
				if d.Steps != 1 || d.FinishReason != string(reason) || d.InputTokens != 200 || d.OutputTokens != 100 || d.ReasoningTokens != 90 || d.TextBytes != len("report ready") || d.ReasoningBytes != len("private reasoning") || d.ToolCalls != 0 {
					t.Fatalf("repair %d lost its generation diagnostic: %#v", i+1, d)
				}
			}
			data, err := json.Marshal(taskTransitionPayload(item))
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewEventStore(c.session.Workspace, "run-diagnostics", "session-diagnostics")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskCreated), TaskID: id, Actor: "worker", Payload: data}); err != nil {
				t.Fatal(err)
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList(events)
			if len(replayed) != 1 || !reflect.DeepEqual(replayed[0].ExecutionReceipt.RepairProvenance.History, history) {
				t.Fatal("canonical task replay lost repair diagnostics")
			}
			session := NewSession()
			session.Tasks = []*TodoItem{item}
			if err := SaveSession(c.session.Workspace, session); err != nil {
				t.Fatal(err)
			}
			loaded, exists, err := LoadSessionReadOnly(c.session.Workspace)
			if err != nil || !exists || loaded == nil || len(loaded.Tasks) != 1 {
				t.Fatalf("diagnostic checkpoint cannot reload: exists=%v err=%v", exists, err)
			}
			if !reflect.DeepEqual(loaded.Tasks[0].ExecutionReceipt.RepairProvenance.History, history) {
				t.Fatal("checkpoint redaction changed diagnostic counters")
			}
		})
	}
}

func TestRepairGenerationDiagnosticLoadsLegacyRedaction(t *testing.T) {
	for _, raw := range []string{`90`, `"[REDACTED]"`, `"invalid"`} {
		t.Run(raw, func(t *testing.T) {
			var diagnostic RepairGenerationDiagnostic
			err := json.Unmarshal([]byte(`{"steps":1,"finish_reason":"length","output_tokens":24576,"reasoning_tokens":`+raw+`}`), &diagnostic)
			if raw == `"invalid"` {
				if err == nil {
					t.Fatal("invalid telemetry string was accepted")
				}
				return
			}
			want := int64(90)
			if raw == `"[REDACTED]"` {
				want = 0
			}
			if err != nil || diagnostic.ReasoningTokens != want || diagnostic.OutputTokens != 24576 || diagnostic.Steps != 1 || diagnostic.FinishReason != "length" {
				t.Fatalf("legacy diagnostic changed available evidence: %+v err=%v", diagnostic, err)
			}
			workspace := t.TempDir()
			putRuntimeDocument(t, workspace, runtimestore.DocumentSession, []byte(`{"tasks":[{"id":"1","execution_receipt":{"repair_provenance":{"history":[{"generation":{"steps":1,"reasoning_tokens":`+raw+`}}]}}}]}`))
			loaded, exists, err := LoadSessionReadOnly(workspace)
			if err != nil || !exists || loaded == nil || len(loaded.Tasks) != 1 {
				t.Fatalf("legacy diagnostic blocked session recovery: exists=%v err=%v", exists, err)
			}
			if got := loaded.Tasks[0].ExecutionReceipt.RepairProvenance.History[0].Generation; got.ReasoningTokens != want || got.Steps != 1 {
				t.Fatalf("nested legacy diagnostic = %+v", got)
			}
		})
	}
}

func TestProtocolRepairGenerationDiagnosticAvailability(t *testing.T) {
	for _, steps := range [][]fantasy.StepResult{nil, {{}}} {
		d := protocolRepairGenerationDiagnostic(steps)
		if d.Steps != len(steps) || (d.FinishReason == "unknown") != (len(steps) > 0) {
			t.Fatalf("missing generation and unknown termination were conflated: %#v", d)
		}
	}
}
