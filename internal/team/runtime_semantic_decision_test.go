package team

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
	"github.com/kjelly/hufu/internal/sidecar"
)

func TestRuntimeAgentSelectionReplaysLargeCandidateDomain(t *testing.T) {
	c, generator := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "", `{"token":"A22"}`)
	candidates := make([]sidecar.TeamSummary, 22)
	for i := range candidates {
		candidates[i] = sidecar.TeamSummary{Name: fmt.Sprintf("worker-%d", i), Description: "Authorized candidate"}
	}
	for range 2 {
		selection, err := c.AgentPool().Sidecar().SelectAgent(t.Context(), "Select a worker", candidates)
		if err != nil || !selection.Accepted || selection.Agent != candidates[21].Name {
			t.Fatalf("selection=%#v error=%v", selection, err)
		}
	}
	records, err := c.DecisionPrimitiveResults(t.Context())
	if err != nil || len(records) != 2 || !records[1].Reused {
		t.Fatalf("records=%#v error=%v", records, err)
	}
	if generator.calls.Load() != 1 {
		t.Fatalf("provider calls=%d, want one", generator.calls.Load())
	}
}

func TestRuntimeDecisionSharedDefaultsAndConfigurationDrift(t *testing.T) {
	server := newControlModelServer(t, 200, controlChoice("o", 0, .99))
	c, legacy := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "", `{"token":"A1"}`)
	defaults := settings.Config{Backend: "systemone", SystemOne: settings.SystemOne{Model: "shared-model", Endpoint: server.URL}}.WithSource("global")
	c.session.GlobalDecisionRuntime = defaults
	invocation := sidecar.DecisionInvocation{Name: "runtime_route", Question: "Choose a route", Input: "accepted request", Options: []catalog.Option{{ID: "fast"}, {ID: "team"}}, MaxCalls: 100}
	result, err := c.runRuntimeChoice(t.Context(), nil, invocation)
	if err != nil || result.Status != decisionrt.StatusDecided || result.Model != "shared-model" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if legacy.calls.Load() != 0 || server.calls.Load() != 1 {
		t.Fatal("unexpected extra inference")
	}
	entries, err := catalog.New(map[string]catalog.Entry{"route": {Version: "1", Kind: decisionrt.KindChoice, Question: invocation.Question, Options: invocation.Options, Agents: []string{"worker"}}}, catalog.WithRuntimeConfig(defaults))
	if err != nil {
		t.Fatal(err)
	}
	records, err := c.DecisionPrimitiveResults(t.Context())
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v error=%v", records, err)
	}
	transport := entries.Transports()["route"]
	if records[0].Transport.Model != transport.Model || records[0].Transport.Endpoint != transport.Endpoint || records[0].Transport.Sources["model"] != "global" {
		t.Fatalf("different effective defaults: %#v %#v", records[0].Transport, transport)
	}
	hash, err := defaults.Fingerprint("sidecar")
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = &executionPolicyState{snapshot: &ExecutionPolicySnapshot{DecisionRuntimeHash: hash}}
	c.session.GlobalDecisionRuntime.SystemOne.Model = "changed-model"
	if _, err := c.runRuntimeChoice(t.Context(), nil, invocation); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("changed policy accepted: %v", err)
	}
	if server.calls.Load() != 1 {
		t.Fatal("drift reached provider")
	}
}

func TestRuntimeDecisionRepairAndBudgetReuseOnlySettled(t *testing.T) {
	c, generator := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "", `{"token":"A0"}`)
	s := c.AgentPool().Sidecar()
	invocation := sidecar.DecisionInvocation{Name: "runtime_route", Question: "Select a route", Input: "original request", Options: []catalog.Option{{ID: "fast"}, {ID: "team"}}, MaxCalls: 1}
	if _, err := c.runRuntimeChoice(t.Context(), s, invocation); err != nil {
		t.Fatal(err)
	}
	c.SetBudget(1, 0)
	c.sessionTime = time.Now().Add(-2 * time.Second)
	repair := withInvocationMetadata(t.Context(), InvocationMetadata{Purpose: "result_repair"})
	if _, err := c.runRuntimeChoice(repair, s, invocation); err != nil {
		t.Fatalf("settled replay must not spend inference budget: %v", err)
	}
	invocation.Input = "changed request"
	if _, err := c.runRuntimeChoice(repair, s, invocation); err == nil || !strings.Contains(err.Error(), "repair_inference_denied") {
		t.Fatalf("repair inferred: %v", err)
	}
	if _, err := c.runRuntimeChoice(t.Context(), s, invocation); err == nil || !strings.Contains(err.Error(), "budget_exceeded") {
		t.Fatalf("exhausted budget inferred: %v", err)
	}
	c.SetBudget(60, 0)
	c.sessionTime = time.Now()
	if _, err := c.runRuntimeChoice(t.Context(), s, invocation); err == nil || !strings.Contains(err.Error(), "call_limit") {
		t.Fatalf("exhausted call limit inferred: %v", err)
	}
	if generator.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want one", generator.calls.Load())
	}
}

func TestRuntimeDecisionConcurrentReplayAndChangedCandidates(t *testing.T) {
	c, generator := newControlTestCoordinator(t, control.PathReviewer, control.ModeOff, "", `{"token":"A0"}`)
	s := c.AgentPool().Sidecar()
	invocation := sidecar.DecisionInvocation{Name: "runtime_route", Question: "Select a route", Input: map[string]string{"state": "accepted"}, Options: []catalog.Option{{ID: "fast"}, {ID: "team"}}, MaxCalls: 100}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.runRuntimeChoice(t.Context(), s, invocation); err != nil {
				t.Errorf("decision: %v", err)
			}
		})
	}
	wg.Wait()
	if generator.calls.Load() != 1 {
		t.Fatalf("parallel calls=%d", generator.calls.Load())
	}
	invocation.Options = []catalog.Option{{ID: "other"}, {ID: "team"}}
	if _, err := c.runRuntimeChoice(t.Context(), s, invocation); err != nil {
		t.Fatal(err)
	}
	if generator.calls.Load() != 2 {
		t.Fatal("same-sized candidate drift reused old result")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	invocation.Input = "cancelled request"
	if _, err := c.runRuntimeChoice(ctx, s, invocation); err == nil {
		t.Fatal("cancelled request accepted")
	}
	if generator.calls.Load() != 2 {
		t.Fatal("cancelled request reached provider")
	}
}
