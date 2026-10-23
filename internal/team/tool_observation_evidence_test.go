package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func historicalEvidenceContract(t *testing.T) *CompiledResultContract {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(textEvidenceTestSchema(t, true)), &schema); err != nil {
		t.Fatal(err)
	}
	spec := schema["x-hufu-tool-evidence"].(map[string]any)
	spec["allow_historical"], spec["historical_pointer"] = true, "/history"
	source := schema["properties"].(map[string]any)["findings"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["sources"].(map[string]any)["items"].(map[string]any)
	source["properties"].(map[string]any)["history"] = map[string]any{"type": []string{"object", "null"}}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "history.json", string(raw))
	compiled, err := compileResultContractSchema(dir, "history.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestObservationReadBindsOnlyDeliveredHistoricalEvidence(t *testing.T) {
	for _, mode := range []string{"read", "search_only", "unread_quote", "no_opt_in", "fresh_preferred", "repair", "wrong_transcript", "missing_read_result", "failed_read_result", "duplicate_read", "changed_delivery"} {
		t.Run(mode, func(t *testing.T) {
			c, ctx, _ := observationFixture(t)
			event := captureObservation(t, c, ctx, "source__read_document", `{"service":"public"}`, textEvidenceTestRecords()[1].Output)
			item, ctx := nextObservationTask(t, c)
			compiled := historicalEvidenceContract(t)
			if mode == "no_opt_in" {
				compiled.toolEvidence.AllowHistorical = false
			}
			ref := compiled.ref(true)
			item.ResultContract = &ref
			c.session.ResultContracts = map[string]*CompiledResultContract{ref.ID: compiled}
			transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, "worker")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			if mode == "wrong_transcript" {
				transcript.attempt = 2
			}
			ctx = context.WithValue(ctx, taskTranscriptKey{}, transcript)
			_, _ = recallResponse(t, c, ctx, "search", `{"action":"search","query":"exact words"}`)
			if mode != "search_only" {
				// Read only "exact words", not the rest of the source body.
				response, _ := recallResponse(t, c, ctx, "read-fragment", fmt.Sprintf(`{"action":"read","observation_id":%q,"offset":4,"limit":11}`, event.ID))
				if response.IsError {
					t.Fatal(response.Content)
				}
			}
			if mode == "fresh_preferred" {
				for _, r := range textEvidenceTestRecords() {
					if r.Event == "tool_call" {
						err = transcript.RecordToolCall(r.ToolCallID, r.Tool, r.Input)
					} else {
						err = transcript.RecordToolResult(r.ToolCallID, r.Tool, r.Output, r.Error)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "repair" {
				// Result-only repair retains the runner's original evidence even
				// after closing the original tool stream.
				if err := transcript.Close(); err != nil {
					t.Fatal(err)
				}
			}
			raw := evidenceTestPayload
			if mode == "unread_quote" {
				raw = strings.ReplaceAll(raw, "exact words", "document")
			}
			records := transcript.evidenceRecords()
			for i := range records {
				if records[i].Event != "tool_result" || records[i].ToolCallID != "read-fragment" {
					continue
				}
				switch mode {
				case "missing_read_result":
					records[i].Event = "missing"
				case "failed_read_result":
					records[i].Error = true
				case "duplicate_read":
					records = append(records, records[i])
				case "changed_delivery":
					records[i].Output = strings.ReplaceAll(records[i].Output, "exact words", "other words")
				}
			}
			payload, err := validateStructuredResultPayload(compiled, ref, []byte(raw), records)
			if err != nil {
				t.Fatal(err)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			accepted := mode == "read" || mode == "repair" || mode == "fresh_preferred"
			if (payload.EvidenceDowngrades == 0) != accepted {
				t.Fatalf("unexpected evidence credit: %s", payload.Value)
			}
			if accepted && mode != "fresh_preferred" {
				if evidenceString(value, "/findings/0/sources/0/call_id") != "read-fragment" || evidenceString(value, "/findings/0/sources/0/history/observation_id") != event.ID || evidenceString(value, "/findings/0/sources/0/history/source_call_id") != "source-call" {
					t.Fatalf("lost original/current read provenance: %s", payload.Value)
				}
			}
			if mode == "fresh_preferred" && (evidenceString(value, "/findings/0/sources/0/call_id") != "observed" || evidenceString(value, "/findings/0/sources/0/history/observation_id") != "") {
				t.Fatalf("historical evidence hid a fresh fetch: %s", payload.Value)
			}
		})
	}
}

func TestObservationGatewayCapturesFullLogicalResultBeforeTruncation(t *testing.T) {
	c, ctx, _ := observationFixture(t)
	target := gatewayTestTarget()
	target.Name, target.Server, target.NativeName = "source__read_document", "source", "read_document"
	target.InputSchema = map[string]any{"type": "object", "required": []any{"service"}, "additionalProperties": false, "properties": map[string]any{"service": map[string]any{"type": "string"}}}
	content := strings.Repeat("historical document line\n", 15000)
	executor := &gatewayTestExecutor{text: content}
	gateway := newDynamicToolGateway(c, executor, []DynamicToolTarget{target})
	response, err := (&policyGatedTool{coordinator: c, inner: gateway}).Run(ctx, fantasy.ToolCall{ID: "gateway-source", Name: dynamicToolGatewayName, Input: gatewayCallInput(target.Name)})
	if err != nil || response.IsError || !strings.Contains(response.Content, "truncated by hufu") {
		t.Fatalf("gateway: %s, %v", response.Content[:min(len(response.Content), 100)], err)
	}
	events := observationEvents(t, c, EventToolObservationRecorded)
	if len(events) != 1 {
		t.Fatalf("logical observations: %d", len(events))
	}
	p, body, err := c.loadToolObservation(t.Context(), events[0])
	if err != nil || p.Tool != target.Name || body.Output != content {
		t.Fatalf("full logical result not retained: tool=%s bytes=%d err=%v", p.Tool, len(body.Output), err)
	}
	_, ctx = nextObservationTask(t, c)
	read, _ := recallResponse(t, c, ctx, "read", fmt.Sprintf(`{"action":"read","observation_id":%q}`, events[0].ID))
	if read.IsError || executor.calls != 1 {
		t.Fatalf("recall replayed source or failed: %s; source calls=%d", read.Content, executor.calls)
	}
}

func TestObservationPolicyGateDoesNotCaptureDeniedOrFailedCalls(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c, ctx, _ := observationFixture(t)
			tool := &observationProbe{fail: mode == "failed"}
			if mode == "denied" {
				c.session.Config.ToolsDenied = []string{"view"}
				ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{recallObservationToolName})
			}
			_, err := (&policyGatedTool{coordinator: c, inner: tool}).Run(ctx, fantasy.ToolCall{ID: "call", Name: "view", Input: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			if (len(observationEvents(t, c, EventToolObservationRecorded)) == 1) != (mode == "allowed") {
				t.Fatalf("captured %s call", mode)
			}
			if mode == "denied" && tool.calls != 0 {
				t.Fatal("denied source executed")
			}
		})
	}
}

type observationProbe struct {
	coordToolBase
	calls int
	fail  bool
}

func (*observationProbe) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: "view"} }
func (p *observationProbe) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.calls++
	if p.fail {
		return fantasy.NewTextErrorResponse("source unavailable"), nil
	}
	return fantasy.NewTextResponse("successful observation"), nil
}
