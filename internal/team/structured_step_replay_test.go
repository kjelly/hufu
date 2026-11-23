package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
)

func stepGenerationEvidence(t *testing.T, governed bool) (*Coordinator, []RunEvent, ExecutionStepReceipt) {
	t.Helper()
	c := newTestCoordinator(t)
	c.session.Config.DefaultLLMBackend = "ollama"
	c.initEventStore()
	var err error
	c.providerManager, err = agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if governed {
		c.costManager = newProviderAdmissionCostManager(t, "ollama/gpt", cost.UnknownPriceDeny)
	}
	inner := &costAdmissionLanguageModel{usage: fantasy.Usage{InputTokens: 17, OutputTokens: 3, TotalTokens: 20}}
	model := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/gpt"))
	contract := ExecutionContract{Steps: []ExecutionStep{{ID: "inspect", Tool: "inspector", Effect: ExecutionEffectRead}}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect", Execution: contract}})[0]
	runner := StructuredStepRunnerFunc(func(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, AgentName: "worker", Purpose: "task_execution"})
		_, err := model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("inspect")}, MaxOutputTokens: new(int64(32))})
		return ExecutionStepResult{Stdout: "observed"}, err
	})
	result, err := c.RunStructuredTask(t.Context(), item.ID, 1, runner)
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	return c, events, result.Receipts[0]
}

func TestStructuredStepReplayRejectsSubstitutedInvocationSources(t *testing.T) {
	t.Parallel()
	c, events, _ := stepGenerationEvidence(t, false)
	index := slices.IndexFunc(events, func(e RunEvent) bool { return e.Type == string(EventStructuredStepInvocationObserved) })
	for _, tc := range []struct {
		name   string
		change func(*RunEvent, *structuredStepInvocationObserved)
	}{
		{name: "other run", change: func(e *RunEvent, _ *structuredStepInvocationObserved) { e.RunID = "other-run" }},
		{name: "other branch", change: func(e *RunEvent, _ *structuredStepInvocationObserved) { e.BranchID = "other-branch" }},
		{name: "other task", change: func(e *RunEvent, _ *structuredStepInvocationObserved) { e.TaskID = "other-task" }},
		{name: "other attempt", change: func(e *RunEvent, _ *structuredStepInvocationObserved) { e.Attempt++ }},
		{name: "other step", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.StepID = "other-step" }},
		{name: "other repair", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.RepairAttempt++ }},
		{name: "other occurrence", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.OccurrenceRevision++ }},
		{name: "other receipt", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.ReceiptID = "other-receipt" }},
		{name: "unknown invocation", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.ModelExecutionID = "invented" }},
		{name: "unknown profile", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.ProfileEventID = "invented" }},
		{name: "changed profile hash", change: func(_ *RunEvent, p *structuredStepInvocationObserved) { p.ProfileHash = strings.Repeat("0", 64) }},
		{name: "model authored", change: func(e *RunEvent, _ *structuredStepInvocationObserved) { e.Actor = "model" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := slices.Clone(events)
			payload, err := decodeStructuredStepInvocation(changed[index])
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&changed[index], &payload)
			changed[index].IdempotencyKey = changed[index].BranchID + ":step-invocation:" + payload.ReceiptID + ":" + payload.ModelExecutionID
			changed[index].Payload, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, changed); err == nil {
				t.Fatal("substituted invocation source was accepted")
			}
		})
	}
	duplicate := events[index]
	duplicate.ID += "-duplicate"
	changed := slices.Insert(slices.Clone(events), index, duplicate)
	if _, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, changed); err == nil {
		t.Fatal("duplicate invocation binding was accepted")
	}
}

func TestStructuredStepReceiptViewsSeparateReusedCallIDs(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	c.initEventStore()
	id := addTodo(t, c, "inspect")
	c.setCurrentTaskAttempt(id, 1)
	item := c.todoItemByID(id)
	receipt := ExecutionStepReceipt{ID: "provider-call", RunID: c.executionRunID, BranchID: c.EventStore().BranchID(),
		TaskID: id, Attempt: 1, OccurrenceRevision: item.OccurrenceRevision, StepID: "current", ExecutionKind: "tool_call"}
	registry := c.executionStepReceiptRegistry()
	if err := registry.Record(receipt); err != nil {
		t.Fatal(err)
	}
	history := receipt
	history.RunID, history.StepID = "history", "previous"
	if err := registry.Record(history); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get(receipt.ID); ok {
		t.Fatal("ambiguous unscoped receipt ID resolved to one occurrence")
	}
	view, err := c.taskStepReceiptRegistry(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := view.Get(receipt.ID)
	if !ok || got.StepID != "current" {
		t.Fatalf("current occurrence view=%+v", got)
	}
	if err := c.validateTaskResultReceiptClaims(id, &TaskResult{Attempt: 1, ReceiptIDs: []string{receipt.ID}}); err != nil {
		t.Fatal(err)
	}
	conflict := receipt
	conflict.StepID = "changed"
	if err := registry.Record(conflict); err == nil {
		t.Fatal("conflicting receipt replaced current evidence")
	}
	first := structuredStepReceiptEventKey(receipt.RunID, receipt.BranchID, id, 1, receipt.OccurrenceRevision, receipt.ID)
	second := structuredStepReceiptEventKey(history.RunID, history.BranchID, id, 1, history.OccurrenceRevision, history.ID)
	if first == second {
		t.Fatal("journal key aliases a historical occurrence")
	}
}

func TestStructuredStepInvocationIDsAreDetachedFromRegistry(t *testing.T) {
	t.Parallel()
	c, _, receipt := stepGenerationEvidence(t, false)
	want := receipt.ModelExecutionIDs[0]
	receipt.ModelExecutionIDs[0] = "overridden"
	stored, ok := c.executionStepReceiptRegistry().Get(receipt.ID)
	if !ok || stored.ModelExecutionIDs[0] != want {
		t.Fatal("caller changed stored invocation evidence")
	}
	stored.ModelExecutionIDs[0] = "overridden-again"
	stored, _ = c.executionStepReceiptRegistry().Get(receipt.ID)
	if stored.ModelExecutionIDs[0] != want {
		t.Fatal("registry read returned mutable invocation evidence")
	}
}

func TestStructuredStepRejectsUnjoinedProviderInvocation(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	c.initEventStore()
	contract := ExecutionContract{Steps: []ExecutionStep{{ID: "inspect", Tool: "inspector", Effect: ExecutionEffectRead}}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect", Execution: contract}})[0]
	var release func()
	var saved context.Context
	runner := StructuredStepRunnerFunc(func(ctx context.Context, _ StructuredStepRequest) (ExecutionStepResult, error) {
		saved = ctx
		var err error
		release, err = (providerRequestAdmission{c: c}).AcquireProviderInvocation(ctx, "test")
		return ExecutionStepResult{}, err
	})
	result, err := c.RunStructuredTask(t.Context(), item.ID, 1, runner)
	if release != nil {
		defer release()
	}
	if err == nil || result.State != StructuredExecutionFailed {
		t.Fatalf("unjoined invocation accepted: result=%+v err=%v", result, err)
	}
	if releaseAgain, err := (providerRequestAdmission{c: c}).AcquireProviderInvocation(saved, "test"); err == nil {
		if releaseAgain != nil {
			releaseAgain()
		}
		t.Fatal("closed step admitted another invocation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	events, err := c.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStructuredStepReceipts(ctx, c.session.Workspace, events); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled receipt read: %v", err)
	}
}

func TestStructuredStepAttributionDoesNotChangeGenerationCost(t *testing.T) {
	t.Parallel()
	c, events, receipt := stepGenerationEvidence(t, true)
	rows, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Invocations) != 1 || rows[0].Invocations[0].ExecutionTarget != "ollama/gpt" {
		t.Fatalf("actual invocation target=%+v", rows)
	}
	before := slices.DeleteFunc(slices.Clone(events), func(e RunEvent) bool {
		return e.Type == string(EventStructuredStepReceiptRecorded) || e.Type == string(EventStructuredStepInvocationObserved)
	})
	prior, err := ProjectCostView(before, c.executionRunID, receipt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ProjectCostView(events, c.executionRunID, receipt.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if prior.UsageDerivedMicros == nil || after.UsageDerivedMicros == nil || *prior.UsageDerivedMicros != 23 || *after.UsageDerivedMicros != 23 {
		t.Fatalf("cost changed: before=%+v after=%+v", prior, after)
	}
}

func TestStructuredStepCodecPreservesCompleteRedactedDiagnostics(t *testing.T) {
	t.Parallel()
	c, _, receipt := stepGenerationEvidence(t, false)
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), PutArtifactRequest{Kind: "artifact", Path: "api_key", Content: []byte("verified artifact")})
	if err != nil {
		t.Fatal(err)
	}
	receipt.Produced = []ArtifactRef{artifact.ArtifactRef}
	receipt.ProducedDigests = map[string]string{"api_key": artifact.ArtifactRef.SHA256}
	receipt.Stdout = strings.Repeat("durable-line\n", 20_000) + `{"api_key":"fixture-secret-123"}` + "\nlast-diagnostic"
	canonical, raw, err := encodeStructuredStepReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("fixture-secret-123")) || strings.Count(canonical.Stdout, "durable-line\n") != 20_000 || !strings.HasSuffix(canonical.Stdout, "last-diagnostic") {
		t.Fatal("diagnostics were leaked or truncated")
	}
	if canonical.ProducedDigests["api_key"] != artifact.ArtifactRef.SHA256 || canonical.StdoutRef.Bytes != int64(len(canonical.Stdout)) {
		t.Fatal("artifact or diagnostic evidence changed")
	}
	var corrupt structuredStepReceiptBlob
	if err := json.Unmarshal(raw, &corrupt); err != nil {
		t.Fatal(err)
	}
	corrupt.Receipt.Stdout += "substituted"
	changed, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeStructuredStepReceipt(changed); err == nil {
		t.Fatal("diagnostic digest mismatch was accepted")
	}
}
