package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

type recordingSemanticRunInputResolver struct {
	value    json.RawMessage
	err      error
	requests []SemanticRunInputRequest
}

func (r *recordingSemanticRunInputResolver) Resolve(_ context.Context, request SemanticRunInputRequest) (json.RawMessage, error) {
	r.requests = append(r.requests, request)
	if r.err != nil {
		return nil, r.err
	}
	return json.RawMessage(string(r.value)), nil
}

type deadlineSemanticRunInputResolver struct {
	sawDeadline bool
}

func (r *deadlineSemanticRunInputResolver) Resolve(ctx context.Context, _ SemanticRunInputRequest) (json.RawMessage, error) {
	_, r.sawDeadline = ctx.Deadline()
	return nil, context.DeadlineExceeded
}

func semanticScopeDefinition(mode string) RunInputDefinition {
	return RunInputDefinition{
		Name:     "review.scope",
		Required: true,
		Schema: RunInputSchema{
			Type: "object",
			Properties: map[string]RunInputSchema{
				"kind":    {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"last_n"`), json.RawMessage(`"revision_range"`), json.RawMessage(`"since"`)}},
				"count":   {Type: "integer", Minimum: new(1.0), Maximum: new(100.0)},
				"history": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"first_parent"`)}},
				"head":    {Type: "string", MinLength: new(1), MaxLength: new(160)},
				"base":    {Type: "string", MaxLength: new(160)},
			},
			RequiredProperties:   []string{"kind", "count", "history", "head"},
			AdditionalProperties: new(false),
		},
		Resolver: &RunInputResolverSpec{
			ID: "review-scope-v1", Capability: "resolve-scope", Type: "resolve_review_scope",
			Mode: mode, Source: "invocation_prompt", SideEffect: "none", Timeout: 10,
		},
	}
}

func TestSemanticRunInputUsesTypedJSONAndCoreProvenance(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"head":"HEAD","count":5,"history":"first_parent","kind":"last_n"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)

	assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "審查最近5個的 git commit", nil, "run-1", true)
	if err != nil {
		t.Fatalf("resolveRunInputCandidates: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("assignments = %#v, want one typed assignment", assignments)
	}
	assignment := assignments[0]
	if assignment.Name != "review.scope" || assignment.Source != RunInputSourceResolver || assignment.ResolverID != "review-scope-v1" || assignment.ResolverVersion != semanticRunInputResolverVersion {
		t.Fatalf("assignment provenance = %#v", assignment)
	}
	if string(assignment.RawValue) != `{"count":5,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("canonical typed value = %s", assignment.RawValue)
	}
	if len(semantic.requests) != 1 || semantic.requests[0].InputName != "review.scope" || semantic.requests[0].Schema.Type != "object" {
		t.Fatalf("semantic requests = %#v", semantic.requests)
	}
}

func TestSemanticRunInputRejectsInvalidOutputAndFallsBackDeterministically(t *testing.T) {
	cases := []struct {
		name  string
		value json.RawMessage
		err   error
	}{
		{name: "unknown property", value: json.RawMessage(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD","command":"git log"}`)},
		{name: "invalid count", value: json.RawMessage(`{"kind":"last_n","count":0,"history":"first_parent","head":"HEAD"}`)},
		{name: "malformed", value: json.RawMessage(`not json`)},
		{name: "provider error", value: nil, err: errors.New("timeout")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
			semantic := &recordingSemanticRunInputResolver{value: test.value, err: test.err}
			coordinator.SetSemanticRunInputResolver(semantic)
			fallback := &testRunInputResolverProvider{response: RunInputResolverResponse{
				Status: "matched", Value: json.RawMessage(`{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}`), ResolverVersion: "3",
			}}
			coordinator.session.ProviderRegistry = NewProviderRegistry()
			coordinator.session.ProviderRegistry.Register("resolve-scope", fallback)

			assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "review scope", nil, "run-1", true)
			if err != nil {
				t.Fatalf("fallback resolution: %v", err)
			}
			if len(assignments) != 1 || string(assignments[0].RawValue) != `{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}` {
				t.Fatalf("fallback assignments = %#v", assignments)
			}
			if len(assignments[0].Evidence) != 0 {
				t.Fatalf("command-shaped semantic value was persisted as semantic evidence: %#v", assignments[0].Evidence)
			}
			if len(fallback.requests) != 1 {
				t.Fatalf("deterministic fallback requests = %#v, want one", fallback.requests)
			}

		})
	}
}

func TestSemanticRunInputRejectsCommandShapedRevisionValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "head", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git log --all"}`},
		{name: "head flags", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git --no-pager log --all"}`},
		{name: "base", value: `{"kind":"revision_range","history":"first_parent","head":"HEAD","base":"git log --all"}`},
		{name: "base executable path", value: `{"kind":"revision_range","history":"first_parent","head":"HEAD","base":"/usr/bin/git -C /repo log"}`},
		{name: "pipe separator", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git|cat"}`},
		{name: "semicolon separator", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git;true"}`},
		{name: "path semicolon separator", value: `{"kind":"revision_range","history":"first_parent","head":"HEAD","base":"/usr/bin/git;true"}`},
		{name: "command substitution", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"$(git log)"}`},
		{name: "standalone executable", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git"}`},
		{name: "shell escaped backslash", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"g\\it log"}`},
		{name: "shell escaped quote", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"g'it' log"}`},
		{name: "windows executable", value: `{"kind":"last_n","count":5,"history":"first_parent","head":"git.exe log"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
			coordinator.SetSemanticRunInputResolver(&recordingSemanticRunInputResolver{value: json.RawMessage(test.value)})
			fallback := &testRunInputResolverProvider{response: RunInputResolverResponse{
				Status: "matched", Value: json.RawMessage(`{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}`), ResolverVersion: "3",
			}}
			coordinator.session.ProviderRegistry = NewProviderRegistry()
			coordinator.session.ProviderRegistry.Register("resolve-scope", fallback)

			assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "review scope", nil, "run-1", true)
			if err != nil {
				t.Fatalf("command-shaped semantic value was not safely downgraded: %v", err)
			}
			if len(assignments) != 1 || string(assignments[0].RawValue) != `{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}` {
				t.Fatalf("fallback assignments = %#v", assignments)
			}
			if len(fallback.requests) != 1 {
				t.Fatalf("deterministic fallback requests = %#v, want one", fallback.requests)
			}

			coordinator.executionRunID = "run-command-shaped"
			coordinator.initEventStore()
			if err := coordinator.resolveRunInputsForInvocation(t.Context(), "review scope"); err != nil {
				t.Fatalf("persist fallback resolution: %v", err)
			}
			snapshot := coordinator.RunInputSnapshot()
			if snapshot == nil || len(snapshot.Inputs) != 1 || len(snapshot.Inputs[0].Evidence) != 0 {
				t.Fatalf("command-shaped semantic evidence persisted in snapshot: %#v", snapshot)
			}
			events, err := coordinator.EventStore().ReadEvents()
			if err != nil {
				t.Fatalf("read persisted events: %v", err)
			}
			for _, event := range events {
				if EventType(event.Type) == EventRunInputsResolved && strings.Contains(string(event.Payload), "git") {
					t.Fatalf("command-shaped semantic value persisted in event payload: %s", event.Payload)
				}
			}
		})
	}
}

func TestSemanticRunInputAppliesResolverTimeoutBeforeFallback(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &deadlineSemanticRunInputResolver{}
	coordinator.SetSemanticRunInputResolver(semantic)
	fallback := &testRunInputResolverProvider{response: RunInputResolverResponse{
		Status: "matched", Value: json.RawMessage(`{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}`), ResolverVersion: "3",
	}}
	coordinator.session.ProviderRegistry = NewProviderRegistry()
	coordinator.session.ProviderRegistry.Register("resolve-scope", fallback)

	assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "review scope", nil, "run-1", true)
	if err != nil {
		t.Fatalf("timeout fallback resolution: %v", err)
	}
	if !semantic.sawDeadline || len(assignments) != 1 || len(fallback.requests) != 1 {
		t.Fatalf("semantic timeout=%t assignments=%#v fallback requests=%#v", semantic.sawDeadline, assignments, fallback.requests)
	}
}

func TestSemanticRunInputOutputRejectsProseAndMultipleDocuments(t *testing.T) {
	for _, raw := range []string{
		"Here is the JSON: {}",
		"```json\n{}\n```",
		`{"kind":"last_n"}{"count":5}`,
	} {
		if _, err := decodeOneSemanticJSONValue(raw); err == nil {
			t.Fatalf("accepted non-single JSON response %q", raw)
		}
	}
}

func TestDryRunNeverInvokesSemanticRunInputResolver(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	definition := semanticScopeDefinition(runInputResolverModeSemanticJSON)
	definition.Default = json.RawMessage(`{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}`)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{definition}
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)
	coordinator.session.ProviderRegistry = NewProviderRegistry()
	coordinator.session.ProviderRegistry.Register("resolve-scope", &testRunInputResolverProvider{response: RunInputResolverResponse{Status: "no_match"}})

	snapshot, err := coordinator.previewRunInputs(t.Context(), "審查最近5個 commit")
	if err != nil {
		t.Fatalf("previewRunInputs: %v", err)
	}
	if len(semantic.requests) != 0 {
		t.Fatalf("dry-run invoked semantic resolver: %#v", semantic.requests)
	}
	if snapshot == nil || string(snapshot.Inputs[0].CanonicalValue) != `{"count":10,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("dry-run snapshot = %#v", snapshot)
	}
}

func TestPreCancelledDirectAgentSkipsSemanticResolverAndProviderBoundary(t *testing.T) {
	coordinator := newDirectTerminationCoordinator(t, directTerminationAgent{})
	coordinator.sidecarModel = "test-sidecar"
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)
	var boundaryCalls atomic.Int32
	coordinator.providerBoundaryStart = func(context.Context, string) error {
		boundaryCalls.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := coordinator.RunDirectAgent(ctx, "worker", "審查最近5個 commit"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDirectAgent error = %v, want context.Canceled", err)
	}
	if got := len(semantic.requests); got != 0 {
		t.Fatalf("pre-cancelled direct invocation called semantic resolver %d time(s), want zero", got)
	}
	if got := boundaryCalls.Load(); got != 0 {
		t.Fatalf("pre-cancelled direct invocation started provider boundary %d time(s), want zero", got)
	}
}

func TestSemanticRunInputPersistsTypedSnapshotProvenanceAndHash(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.SetSessionData(NewSession())
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	coordinator.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{
		"resolve-scope": {Command: []string{"resolver-fixture"}},
	}
	coordinator.SetSemanticRunInputResolver(&recordingSemanticRunInputResolver{
		value: json.RawMessage(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD"}`),
	})
	coordinator.executionRunID = "run-semantic"
	coordinator.initEventStore()
	if err := coordinator.resolveRunInputsForInvocation(t.Context(), "審查最近5個的 git commit"); err != nil {
		t.Fatalf("resolveRunInputsForInvocation: %v", err)
	}
	snapshot := coordinator.RunInputSnapshot()
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateRunInputSnapshot: %v", err)
	}
	if snapshot.Inputs[0].Source != RunInputSourceResolver || snapshot.Inputs[0].ResolverVersion != semanticRunInputResolverVersion || string(snapshot.Inputs[0].CanonicalValue) != `{"count":5,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("semantic snapshot input = %#v", snapshot.Inputs[0])
	}
	events, err := coordinator.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if EventType(event.Type) != EventRunInputsResolved {
			continue
		}
		var persisted RunInputSnapshot
		if err := json.Unmarshal(event.Payload, &persisted); err != nil {
			t.Fatalf("decode run_inputs_resolved: %v", err)
		}
		if err := ValidateRunInputSnapshot(&persisted); err != nil {
			t.Fatalf("persisted snapshot validation: %v", err)
		}
		if persisted.SnapshotHash != snapshot.SnapshotHash {
			t.Fatalf("persisted snapshot hash = %q, want %q", persisted.SnapshotHash, snapshot.SnapshotHash)
		}
		found = true
	}
	if !found {
		t.Fatal("run_inputs_resolved event was not persisted")
	}
}

func TestSemanticRunInputResumeReusesFrozenSnapshotWithoutCallingLLM(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.SetSessionData(NewSession())
	definition := semanticScopeDefinition(runInputResolverModeSemanticJSON)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{definition}
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{definition}, []RunInputAssignment{{
		Name: definition.Name, RawValue: []byte(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD"}`),
		Source: RunInputSourceResolver, ResolverID: definition.Resolver.ID, ResolverVersion: semanticRunInputResolverVersion,
		Evidence: []InputEvidence{{Source: RunInputSourceResolver, Location: "invocation_prompt", Kind: "semantic_json"}},
	}}, "run-old", "run-old:invocation", coordinator.session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.sessionData.RunInputSnapshots = []RunInputSnapshot{*snapshot}
	coordinator.sessionData.ActiveRunInputSnapshotID = snapshot.ID
	coordinator.taskTracker.TodoList().SetRunID("run-old")
	item := coordinator.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interrupted"}})[0]
	item.Status = TaskInProgress
	coordinator.executionRunID = "run-resumed"
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"kind":"last_n","count":99,"history":"first_parent","head":"HEAD"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)

	if err := coordinator.resolveRunInputsForInvocation(t.Context(), "審查最近99個 commit"); err != nil {
		t.Fatalf("resume input resolution: %v", err)
	}
	if len(semantic.requests) != 0 {
		t.Fatalf("resume invoked semantic resolver: %#v", semantic.requests)
	}
	if got := coordinator.RunInputSnapshot(); got == nil || got.SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("resume snapshot = %#v, want frozen snapshot %q", got, snapshot.SnapshotHash)
	}
}
