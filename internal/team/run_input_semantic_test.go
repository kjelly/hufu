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
	values   []json.RawMessage
	err      error
	requests []SemanticRunInputRequest
}

func (r *recordingSemanticRunInputResolver) Resolve(_ context.Context, request SemanticRunInputRequest) (json.RawMessage, error) {
	r.requests = append(r.requests, request)
	if r.err != nil {
		return nil, r.err
	}
	if len(r.values) > 0 {
		index := min(len(r.requests)-1, len(r.values)-1)
		return json.RawMessage(string(r.values[index])), nil
	}
	return json.RawMessage(string(r.value)), nil
}

const testSemanticValidatorVersion = "candidate-validator-1"

func semanticCandidateValidator(validate func(json.RawMessage) error) *testRunInputResolverProvider {
	return &testRunInputResolverProvider{resolve: func(request RunInputResolverRequest) (RunInputResolverResponse, error) {
		if len(request.CandidateValue) == 0 {
			return RunInputResolverResponse{Status: "no_match"}, nil
		}
		if validate != nil {
			if err := validate(request.CandidateValue); err != nil {
				return RunInputResolverResponse{Status: "invalid", Diagnostic: err.Error()}, nil
			}
		}
		return RunInputResolverResponse{
			Status: "matched", Value: json.RawMessage(string(request.CandidateValue)), ResolverVersion: testSemanticValidatorVersion,
		}, nil
	}}
}

func registerSemanticCandidateValidator(coordinator *Coordinator, validate func(json.RawMessage) error) *testRunInputResolverProvider {
	provider := semanticCandidateValidator(validate)
	coordinator.session.ProviderRegistry = NewProviderRegistry()
	coordinator.session.ProviderRegistry.Register("resolve-scope", provider)
	return provider
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
				"kind":    {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"last_n"`), json.RawMessage(`"revision_range"`), json.RawMessage(`"since"`), json.RawMessage(`"working_tree"`)}},
				"count":   {Type: "integer", Minimum: new(1.0), Maximum: new(100.0)},
				"history": {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"first_parent"`)}},
				"head":    {Type: "string", MinLength: new(1), MaxLength: new(160)},
				"base":    {Type: "string", MaxLength: new(160)},
			},
			RequiredProperties:   []string{"kind", "history", "head"},
			AdditionalProperties: new(false),
		},
		Resolver: &RunInputResolverSpec{
			ID: "review-scope-v1", Capability: "resolve-scope", Type: "resolve_review_scope",
			Mode: mode, Source: "invocation_prompt", SideEffect: "none", Timeout: 10,
			SemanticGuidance: "A named commit means kind=last_n, count=1, and head=<commit>. working_tree is only for uncommitted changes and requires head=HEAD.",
		},
	}
}

func TestSemanticRunInputUsesTypedJSONAndCoreProvenance(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"head":"HEAD","count":5,"history":"first_parent","kind":"last_n"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)
	provider := registerSemanticCandidateValidator(coordinator, nil)

	assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "審查最近5個的 git commit", nil, "run-1", true)
	if err != nil {
		t.Fatalf("resolveRunInputCandidates: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("assignments = %#v, want one typed assignment", assignments)
	}
	assignment := assignments[0]
	if assignment.Name != "review.scope" || assignment.Source != RunInputSourceResolver || assignment.ResolverID != "review-scope-v1" || assignment.ResolverVersion != semanticRunInputResolverVersion+"+"+testSemanticValidatorVersion {
		t.Fatalf("assignment provenance = %#v", assignment)
	}
	if string(assignment.RawValue) != `{"count":5,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("canonical typed value = %s", assignment.RawValue)
	}
	if len(semantic.requests) != 1 || semantic.requests[0].InputName != "review.scope" || semantic.requests[0].Schema.Type != "object" || semantic.requests[0].Guidance == "" {
		t.Fatalf("semantic requests = %#v", semantic.requests)
	}
	if len(provider.requests) != 1 || string(provider.requests[0].CandidateValue) != `{"count":5,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("candidate validation requests = %#v", provider.requests)
	}
}

func TestSemanticRunInputRepairsTeamInvalidCandidateBeforeSnapshot(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{values: []json.RawMessage{
		json.RawMessage(`{"kind":"working_tree","history":"first_parent","head":"af6205cd743eba5b62412d9f4347952c74f37576"}`),
		json.RawMessage(`{"kind":"last_n","count":1,"history":"first_parent","head":"af6205cd743eba5b62412d9f4347952c74f37576"}`),
	}}
	coordinator.SetSemanticRunInputResolver(semantic)
	provider := registerSemanticCandidateValidator(coordinator, func(candidate json.RawMessage) error {
		var value struct {
			Kind string `json:"kind"`
			Head string `json:"head"`
		}
		if err := json.Unmarshal(candidate, &value); err != nil {
			return err
		}
		if value.Kind == "working_tree" && value.Head != "HEAD" {
			return errors.New("working_tree review scope requires head HEAD")
		}
		return nil
	})

	assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "Review commit af6205cd743eba5b62412d9f4347952c74f37576", nil, "run-1", true)
	if err != nil {
		t.Fatalf("resolveRunInputCandidates: %v", err)
	}
	if len(assignments) != 1 || string(assignments[0].RawValue) != `{"count":1,"head":"af6205cd743eba5b62412d9f4347952c74f37576","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("repaired assignments = %#v", assignments)
	}
	if len(semantic.requests) != 2 || string(semantic.requests[1].PreviousValue) == "" || !strings.Contains(semantic.requests[1].Diagnostic, "requires head HEAD") {
		t.Fatalf("semantic repair requests = %#v", semantic.requests)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("candidate validation requests = %#v", provider.requests)
	}
}

func TestSemanticRunInputCandidateValidatorCannotRewriteOrClaimEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		response RunInputResolverResponse
		want     string
	}{
		{
			name: "rewrite",
			response: RunInputResolverResponse{
				Status: "matched", Value: json.RawMessage(`{"kind":"last_n","count":2,"history":"first_parent","head":"HEAD"}`), ResolverVersion: "validator-1",
			},
			want: "changed the semantic candidate",
		},
		{
			name: "evidence",
			response: RunInputResolverResponse{
				Status: "matched", Value: json.RawMessage(`{"kind":"last_n","count":1,"history":"first_parent","head":"HEAD"}`), ResolverVersion: "validator-1",
				Evidence: []RunInputResolverEvidence{{Source: "prompt", Start: 0, End: 1, Kind: "scope"}},
			},
			want: "candidate validator returned prompt evidence",
		},
		{
			name:     "no match",
			response: RunInputResolverResponse{Status: "no_match"},
			want:     "candidate validator returned no_match",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
			coordinator.SetSemanticRunInputResolver(&recordingSemanticRunInputResolver{
				value: json.RawMessage(`{"kind":"last_n","count":1,"history":"first_parent","head":"HEAD"}`),
			})
			coordinator.session.ProviderRegistry = NewProviderRegistry()
			coordinator.session.ProviderRegistry.Register("resolve-scope", &testRunInputResolverProvider{response: test.response})

			if _, err := coordinator.resolveRunInputCandidates(t.Context(), "review latest commit", nil, "run-1", true); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("candidate validator error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSemanticRunInputRejectsInvalidOutputAfterOneRepair(t *testing.T) {
	cases := []struct {
		name  string
		value json.RawMessage
	}{
		{name: "unknown property", value: json.RawMessage(`{"kind":"last_n","count":5,"history":"first_parent","head":"HEAD","command":"git log"}`)},
		{name: "invalid count", value: json.RawMessage(`{"kind":"last_n","count":0,"history":"first_parent","head":"HEAD"}`)},
		{name: "malformed", value: json.RawMessage(`not json`)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
			coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
			semantic := &recordingSemanticRunInputResolver{value: test.value}
			coordinator.SetSemanticRunInputResolver(semantic)
			provider := registerSemanticCandidateValidator(coordinator, nil)

			if _, err := coordinator.resolveRunInputCandidates(t.Context(), "review scope", nil, "run-1", true); err == nil || !strings.Contains(err.Error(), "input_invalid") {
				t.Fatalf("invalid semantic output error = %v", err)
			}
			if len(semantic.requests) != 2 {
				t.Fatalf("semantic requests = %#v, want initial plus one repair", semantic.requests)
			}
			if len(provider.requests) != 0 {
				t.Fatalf("schema-invalid candidate reached team validator: %#v", provider.requests)
			}
		})
	}
}

func TestSemanticRunInputUnavailableFallsBackDeterministically(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{err: errors.New("timeout")}
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
	if len(assignments) != 1 || string(assignments[0].RawValue) != `{"kind":"last_n","count":7,"history":"first_parent","head":"HEAD"}` || len(assignments[0].Evidence) != 0 {
		t.Fatalf("fallback assignments = %#v", assignments)
	}
	if len(fallback.requests) != 1 || len(fallback.requests[0].CandidateValue) != 0 {
		t.Fatalf("deterministic fallback requests = %#v", fallback.requests)
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
			semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(test.value)}
			coordinator.SetSemanticRunInputResolver(semantic)
			provider := registerSemanticCandidateValidator(coordinator, nil)

			if _, err := coordinator.resolveRunInputCandidates(t.Context(), "review scope", nil, "run-1", true); err == nil || !strings.Contains(err.Error(), "input_invalid") {
				t.Fatalf("command-shaped semantic value error = %v", err)
			}
			if len(semantic.requests) != 2 {
				t.Fatalf("semantic requests = %#v, want one repair", semantic.requests)
			}
			if len(provider.requests) != 0 {
				t.Fatalf("command-shaped candidate reached team validator: %#v", provider.requests)
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
	registerSemanticCandidateValidator(coordinator, nil)
	coordinator.executionRunID = "run-semantic"
	coordinator.initEventStore()
	if err := coordinator.resolveRunInputsForInvocation(t.Context(), "審查最近5個的 git commit"); err != nil {
		t.Fatalf("resolveRunInputsForInvocation: %v", err)
	}
	snapshot := coordinator.RunInputSnapshot()
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateRunInputSnapshot: %v", err)
	}
	if snapshot.Inputs[0].Source != RunInputSourceResolver || snapshot.Inputs[0].ResolverVersion != semanticRunInputResolverVersion+"+"+testSemanticValidatorVersion || string(snapshot.Inputs[0].CanonicalValue) != `{"count":5,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
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
