package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func boundReadBudgetFixture(t *testing.T, limit int, content string) (*boundDecisionFixture, fantasy.AgentTool, string) {
	t.Helper()
	f := newBoundDecisionFixture(t, 1, func(root map[string]any) {
		collection := root["x-hufu-fixed-collections"].([]any)[0].(map[string]any)
		group := collection["decision_groups"].([]any)[0].(map[string]any)
		group["context"].(map[string]any)["max_bytes"] = limit
	})
	store, err := NewFileArtifactStore(f.c.session.Workspace, f.c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), PutArtifactRequest{Kind: "test", Path: "budget.txt", Content: []byte(content), TaskID: f.sources[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := artifactAccessScopeFromContext(f.ctx)
	readScope := *scope
	readScope.AuthorizedRefs = append(append([]ArtifactRef(nil), scope.AuthorizedRefs...), artifact.ArtifactRef)
	f.ctx = context.WithValue(f.ctx, artifactAccessScopeKey, &readScope)
	view := f.c.gatePolicyTools([]fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(f.c.openArtifactRef))})[0]
	input, err := json.Marshal(map[string]any{"artifact_ref": artifact.ID, "byte_limit": len(content)})
	if err != nil {
		t.Fatal(err)
	}
	return f, view, string(input)
}

func TestBoundDecisionReadBudgetWithholdsWholeEncodedResponse(t *testing.T) {
	// The plain text fits; JSON escaping and canonical observation metadata do
	// not. The admission check must use the exact inference representation.
	content := "UNSEEN_SENTINEL" + strings.Repeat("\"\\\n", 1500)
	f, view, input := boundReadBudgetFixture(t, 7000, content)
	response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "too-large", Name: "view", Input: input})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "decision_read_budget_exhausted") {
		t.Fatalf("read was not withheld: %#v %v", response, err)
	}
	if strings.Contains(response.Content, "UNSEEN_SENTINEL") || len(tools.ViewArtifactReads(response)) != 0 {
		t.Fatalf("withheld content gained observation authority: %#v", response)
	}
	events, err := f.c.readActiveLineageEvents(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != string(EventDecisionArtifactObserved) {
			continue
		}
		var observation decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			t.Fatal(err)
		}
		if observation.CallID == "too-large" {
			t.Fatal("withheld read was published as observed evidence")
		}
	}
	if response := f.call(t, 0, "after-denial"); response.IsError {
		t.Fatalf("denial poisoned earlier evidence: %s", response.Content)
	}
	if _, rejection := f.c.structuredPayloadForSubmission(f.item.ID, f.submission(t, nil)); rejection != "" {
		t.Fatalf("valid result no longer representable: %s", rejection)
	}
}

func TestBoundDecisionReadBudgetSerializesConcurrentPublication(t *testing.T) {
	f, view, input := boundReadBudgetFixture(t, 8000, strings.Repeat("x", 4000))
	var wg sync.WaitGroup
	results := make(chan fantasy.ToolResponse, 2)
	errors := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			response, err := view.Run(f.ctx, fantasy.ToolCall{ID: fmt.Sprintf("parallel-%d", i), Name: "view", Input: input})
			results <- response
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	accepted, withheld := 0, 0
	for response := range results {
		if response.IsError && strings.Contains(response.Content, "decision_read_budget_exhausted") {
			withheld++
		} else if !response.IsError && len(tools.ViewArtifactReads(response)) == 1 {
			accepted++
		} else {
			t.Fatalf("unexpected read result: %#v", response)
		}
	}
	if accepted != 1 || withheld != 1 {
		t.Fatalf("parallel calls overcommitted context: accepted=%d withheld=%d", accepted, withheld)
	}
	if response := f.call(t, 0, "after-parallel"); response.IsError {
		t.Fatalf("admitted prefix exceeds inference capacity: %s", response.Content)
	}
}

func TestBoundDecisionReadBudgetDoesNotChargeIdempotentReadTwice(t *testing.T) {
	f, view, input := boundReadBudgetFixture(t, 8000, strings.Repeat("x", 4000))
	for range 2 {
		response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "replayed-read", Name: "view", Input: input})
		if err != nil || response.IsError {
			t.Fatalf("idempotent read charged twice: %#v %v", response, err)
		}
	}
	changed := strings.Replace(input, `"byte_limit":4000`, `"byte_limit":3999`, 1)
	response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "replayed-read", Name: "view", Input: changed})
	if !errors.Is(err, ErrDecisionIdempotencyConflict) || response.Content != "" {
		t.Fatalf("changed replay exposed unjournaled content: %#v %v", response, err)
	}
	if response := f.call(t, 0, "after-replay"); response.IsError {
		t.Fatalf("read replay poisoned inference: %s", response.Content)
	}
}

func TestBoundDecisionReadBudgetChecksEveryFinding(t *testing.T) {
	f, view, input := boundReadBudgetFixture(t, 8000, strings.Repeat("x", 4000))
	source := f.sources[0]
	compiled := f.c.session.ResultContracts[source.ResultContract.ID]
	raw, err := json.Marshal(map[string]any{"entries": []any{
		map[string]any{"id": "small"},
		map[string]any{"id": strings.Repeat("large finding ", 300)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.StructuredPayload, err = validateStructuredResultPayload(compiled, *source.ResultContract, raw)
	if err != nil {
		t.Fatal(err)
	}
	f.plans, err = f.c.fixedCollectionPlans(f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "second-row-capacity", Name: "view", Input: input})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "decision_read_budget_exhausted") {
		t.Fatalf("a later finding's capacity was ignored: %#v %v", response, err)
	}
	for i := range 2 {
		if response := f.call(t, i, fmt.Sprintf("row-%d", i)); response.IsError {
			t.Fatalf("earlier evidence cannot settle row %d: %s", i, response.Content)
		}
	}
}
