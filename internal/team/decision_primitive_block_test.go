package team

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// TestDecisionPrimitiveRecordsBlockForTheAttempt checks that only a decided
// choice listed in block-on stops the attempt that received it.
func TestDecisionPrimitiveRecordsBlockForTheAttempt(t *testing.T) {
	for _, test := range []struct {
		name          string
		blockOn       []string
		minConfidence float64
		want          bool
	}{
		{name: "listed choice", blockOn: []string{"a"}, want: true},
		{name: "unlisted choice", blockOn: []string{"b"}},
		{name: "abstained", blockOn: []string{"a"}, minConfidence: .95},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(primitiveChoiceResponse))
			}))
			defer server.Close()
			entry := primitiveTestEntry(server.URL)
			entry.BlockOn = test.blockOn
			if test.minConfidence > 0 {
				entry.MinConfidence = new(test.minConfidence)
			}
			c := primitiveTestCoordinator(t, entry)
			ctx := primitiveTestContext(t, "helper")
			if response, err := c.coreTools[0].Run(ctx, primitiveTestCall("call", "text")); err != nil || response.IsError {
				t.Fatalf("decision: %v %#v", err, response)
			}
			decision, blocked := c.decisionBlock(ctx)
			if blocked != test.want || (test.want && decision != "classify=a") {
				t.Fatalf("block = %q %v, want %v", decision, blocked, test.want)
			}
			next := withInvocationMetadata(ctx, InvocationMetadata{RunID: "run-test", TaskID: "1", AgentName: "helper", Attempt: 2})
			if _, blocked := c.decisionBlock(next); blocked {
				t.Fatal("a later attempt inherited the block")
			}
		})
	}
}

// TestDecisionBlockStopsChangesAndUnblockedResults checks that a blocked
// attempt may still read, but cannot run a tool that changes state or report
// anything except blocked, whatever the model decides.
func TestDecisionBlockStopsChangesAndUnblockedResults(t *testing.T) {
	c := newDirectTypedCoordinator(t, "bash", nil, nil)
	metadata := InvocationMetadata{RunID: "run-block", TaskID: "7", AgentName: "worker", Attempt: 1}
	key, _ := decisionBlockKeyFor(metadata)
	c.decisionBlocks = map[decisionBlockKey]string{key: "step-risk=destructive"}
	ctx := withInvocationMetadata(gatewayTestContext(t, c, "bash"), metadata)

	for _, test := range []struct {
		command string
		wantRan bool
	}{
		{command: "rm -rf /var/tmp/old-builds"},
		{command: "ls -la /var/tmp", wantRan: true},
	} {
		t.Run(test.command, func(t *testing.T) {
			probe := &offloadProbeTool{name: "bash"}
			response, err := (&policyGatedTool{inner: probe, coordinator: c}).Run(ctx, fantasy.ToolCall{ID: "call", Name: "bash", Input: `{"command":` + strconv.Quote(test.command) + `}`})
			if err != nil {
				t.Fatal(err)
			}
			if test.wantRan != (probe.ran == 1) || test.wantRan == strings.Contains(response.Content, "decision_block") {
				t.Fatalf("ran=%d response=%q, want ran=%v", probe.ran, response.Content, test.wantRan)
			}
		})
	}

	submit := &submitResultTool{coordinator: c, todoID: "7"}
	for _, test := range []struct {
		status      string
		wantRefused bool
	}{
		{status: "success", wantRefused: true},
		{status: "failed", wantRefused: true},
		{status: "blocked"},
	} {
		t.Run("submit "+test.status, func(t *testing.T) {
			response, err := submit.Run(ctx, fantasy.ToolCall{ID: "submit", Name: submitResultToolName, Input: `{"status":"` + test.status + `","summary":"done"}`})
			if err != nil {
				t.Fatal(err)
			}
			if refused := strings.Contains(response.Content, "decision_block"); refused != test.wantRefused {
				t.Fatalf("response = %q, want refused=%v", response.Content, test.wantRefused)
			}
		})
	}

	// Another attempt of the same task is not blocked.
	other := withInvocationMetadata(context.WithValue(ctx, tools.AgentNameKey, "worker"), InvocationMetadata{RunID: "run-block", TaskID: "7", AgentName: "worker", Attempt: 2})
	probe := &offloadProbeTool{name: "bash"}
	if _, err := (&policyGatedTool{inner: probe, coordinator: c}).Run(other, fantasy.ToolCall{ID: "call", Name: "bash", Input: `{"command":"rm -rf /var/tmp/old-builds"}`}); err != nil || probe.ran != 1 {
		t.Fatalf("other attempt: ran=%d err=%v, want it unaffected by the block", probe.ran, err)
	}
}
