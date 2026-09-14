package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

type testRunInputResolverProvider struct {
	response RunInputResolverResponse
	err      error
	requests []RunInputResolverRequest
}

func (*testRunInputResolverProvider) Validate(Action) error { return nil }
func (*testRunInputResolverProvider) Execute(context.Context, Action) (any, error) {
	return nil, errors.New("action execution is not expected")
}
func (p *testRunInputResolverProvider) ResolveRunInput(_ context.Context, request RunInputResolverRequest) (RunInputResolverResponse, error) {
	p.requests = append(p.requests, request)
	return p.response, p.err
}

func TestLoadTeamNormalizesStrictTypedInputManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: typed-team
spec:
  inputs:
    review.scope:
      type: object
      required: true
      default: {head: HEAD, count: 10, kind: last_n, history: first_parent}
      properties:
        kind: {type: string, enum: [last_n, revision_range, since]}
        count: {type: integer, minimum: 1, maximum: 100}
        history: {type: string, enum: [first_parent]}
        head: {type: string, min-length: 1, max-length: 160}
      required-properties: [kind, count, history, head]
      additional-properties: false
`
	writeTypedInputFixture(t, dir, manifest)
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if len(session.RunInputDefinitions) != 1 {
		t.Fatalf("definitions = %#v", session.RunInputDefinitions)
	}
	definition := session.RunInputDefinitions[0]
	if definition.Name != "review.scope" || definition.Schema.Type != "object" || !definition.Required {
		t.Fatalf("definition = %#v", definition)
	}
	if got, want := string(definition.Default), `{"count":10,"head":"HEAD","history":"first_parent","kind":"last_n"}`; got != want {
		t.Fatalf("canonical default = %s, want %s", got, want)
	}
}

func TestLoadTeamRejectsUnsafeOrUnknownTypedInputSchema(t *testing.T) {
	for name, input := range map[string]string{
		"unknown keyword": "type: string\n      pattern: '.*'",
		"secret":          "type: string\n      secret: true",
		"bad name":        "type: string",
		"wrong keyword":   "type: boolean\n      minimum: 1",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			inputName := "value"
			if name == "bad name" {
				inputName = "Bad Name"
			}
			manifest := "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: bad}\nspec:\n  inputs:\n    " + inputName + ":\n      " + input + "\n"
			writeTypedInputFixture(t, dir, manifest)
			if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
				t.Fatal("LoadTeam accepted invalid typed input schema")
			}
		})
	}
}

func TestResolveRunInputSnapshotCanonicalizesSourcesAndDefaults(t *testing.T) {
	definitions := []RunInputDefinition{
		{Name: "optional", Schema: RunInputSchema{Type: "boolean"}},
		{Name: "review.count", Schema: RunInputSchema{Type: "integer", Minimum: new(1.0), Maximum: new(100.0)}, Required: true, Default: json.RawMessage(`10`)},
	}
	snapshot, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{
		{Name: "review.count", RawValue: []byte("3"), Source: RunInputSourceCLI, Location: "--input[1]"},
		{Name: "review.count", RawValue: []byte("3"), Source: RunInputSourceFile, Location: "inputs.yaml:review.count"},
	}, "run-1", "invocation-1", "demo")
	if err != nil {
		t.Fatalf("ResolveRunInputSnapshot: %v", err)
	}
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		t.Fatalf("ValidateRunInputSnapshot: %v", err)
	}
	if len(snapshot.Inputs) != 1 || snapshot.Inputs[0].Name != "review.count" || string(snapshot.Inputs[0].CanonicalValue) != "3" || snapshot.Inputs[0].Source != RunInputSourceCLI || len(snapshot.Inputs[0].Evidence) != 2 {
		t.Fatalf("snapshot inputs = %#v", snapshot.Inputs)
	}
	defaultSnapshot, err := ResolveRunInputSnapshot(definitions, nil, "run-2", "invocation-2", "demo")
	if err != nil || string(defaultSnapshot.Inputs[0].CanonicalValue) != "10" || defaultSnapshot.Inputs[0].Source != RunInputSourceDefault {
		t.Fatalf("default snapshot = %#v, err=%v", defaultSnapshot, err)
	}
}

func TestResolveRunInputSnapshotFailsClosedOnConflictAndInvalidValues(t *testing.T) {
	definitions := []RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}}
	tests := []struct {
		name        string
		assignments []RunInputAssignment
		want        string
	}{
		{name: "missing", want: "input_missing"},
		{name: "float", assignments: []RunInputAssignment{{Name: "count", RawValue: []byte("10.0"), Source: RunInputSourceCLI}}, want: "input_invalid"},
		{name: "unknown", assignments: []RunInputAssignment{{Name: "other", RawValue: []byte("1"), Source: RunInputSourceCLI}}, want: "input_unknown"},
		{name: "cli file conflict", assignments: []RunInputAssignment{{Name: "count", RawValue: []byte("1"), Source: RunInputSourceCLI}, {Name: "count", RawValue: []byte("2"), Source: RunInputSourceFile}}, want: "input_explicit_conflict"},
		{name: "duplicate cli conflict", assignments: []RunInputAssignment{{Name: "count", RawValue: []byte("1"), Source: RunInputSourceCLI}, {Name: "count", RawValue: []byte("2"), Source: RunInputSourceCLI}}, want: "input_explicit_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveRunInputSnapshot(definitions, test.assignments, "run", "invocation", "demo")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
}

func TestResolveRunInputSnapshotMergesResolverWithoutSilentPrecedence(t *testing.T) {
	definitions := []RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}}
	resolver := RunInputAssignment{
		Name: "count", RawValue: []byte("3"), Source: RunInputSourceResolver,
		ResolverID: "count-v1", ResolverVersion: "1",
		Evidence: []InputEvidence{{Source: RunInputSourceResolver, Location: "invocation_prompt", Start: 7, End: 8, Kind: "count"}},
	}
	snapshot, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{
		{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI, Location: "--input[1]"}, resolver,
	}, "run", "invocation", "demo")
	if err != nil {
		t.Fatal(err)
	}
	input := snapshot.Inputs[0]
	if input.Source != RunInputSourceCLI || input.ResolverID != "count-v1" || input.ResolverVersion != "1" || len(input.Evidence) != 2 {
		t.Fatalf("merged input = %#v", input)
	}
	resolver.RawValue = []byte("4")
	if _, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{
		{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}, resolver,
	}, "run", "invocation", "demo"); err == nil || !strings.Contains(err.Error(), "input_prompt_conflict") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestRunInputResolverResponseFailsClosed(t *testing.T) {
	for name, response := range map[string]RunInputResolverResponse{
		"unknown status":     {Status: "guessed"},
		"matched no version": {Status: "matched", Value: json.RawMessage(`1`)},
		"no match value":     {Status: "no_match", Value: json.RawMessage(`1`)},
		"bad evidence":       {Status: "matched", Value: json.RawMessage(`1`), ResolverVersion: "1", Evidence: []RunInputResolverEvidence{{Source: "artifact", Start: 1, End: 2}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRunInputResolverResponse(response); err == nil {
				t.Fatalf("accepted response %#v", response)
			}
		})
	}
}

func TestCoordinatorResolverUsesPromptAndExplicitCandidateBeforeSnapshot(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	resolver := &testRunInputResolverProvider{response: RunInputResolverResponse{
		Status: "matched", Value: json.RawMessage(`3`), ResolverVersion: "1",
		Evidence: []RunInputResolverEvidence{{Source: "prompt", Start: 5, End: 6, Kind: "count"}},
	}}
	coordinator.session.RunInputDefinitions = []RunInputDefinition{{
		Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true,
		Resolver: &RunInputResolverSpec{ID: "count-v1", Capability: "resolve-count", Type: "resolve_count", Source: "invocation_prompt", SideEffect: "none", Timeout: 1},
	}}
	coordinator.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{
		"resolve-count": {Command: []string{"resolver-fixture"}},
	}
	coordinator.session.ProviderRegistry = NewProviderRegistry()
	coordinator.session.ProviderRegistry.Register("resolve-count", resolver)
	coordinator.SetRunInputAssignments([]RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}})
	state, err := newExecutionPolicyState(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.executionPolicy = state
	ctx, end := coordinator.beginInvocationExecutionRun(t.Context())
	defer end()
	if err := coordinator.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.resolveRunInputsForInvocation(ctx, "last 3 commits"); err != nil {
		t.Fatal(err)
	}
	if len(resolver.requests) != 1 || string(resolver.requests[0].ExplicitValue) != "3" || resolver.requests[0].Prompt != "last 3 commits" {
		t.Fatalf("resolver requests = %#v", resolver.requests)
	}
	snapshot := coordinator.RunInputSnapshot()
	if snapshot == nil || snapshot.Inputs[0].Source != RunInputSourceCLI || snapshot.Inputs[0].ResolverID != "count-v1" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestCoordinatorResolverAmbiguityFailsBeforeTaskOrModelBoundary(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	resolver := &testRunInputResolverProvider{response: RunInputResolverResponse{Status: "ambiguous", Diagnostic: "two incompatible scopes"}}
	coordinator.session.RunInputDefinitions = []RunInputDefinition{{
		Name: "scope", Schema: RunInputSchema{Type: "string"},
		Resolver: &RunInputResolverSpec{ID: "scope-v1", Capability: "resolve-scope", Type: "resolve_scope", Source: "invocation_prompt", SideEffect: "none", Timeout: 1},
	}}
	coordinator.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{"resolve-scope": {Command: []string{"resolver-fixture"}}}
	coordinator.session.ProviderRegistry = NewProviderRegistry()
	coordinator.session.ProviderRegistry.Register("resolve-scope", resolver)
	state, err := newExecutionPolicyState(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.executionPolicy = state
	ctx, end := coordinator.beginInvocationExecutionRun(t.Context())
	defer end()
	if err := coordinator.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.resolveRunInputsForInvocation(ctx, "ambiguous"); err == nil || !strings.Contains(err.Error(), "input_ambiguous") {
		t.Fatalf("error = %v", err)
	}
	events, err := coordinator.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if EventType(event.Type) == EventTaskCreated || EventType(event.Type) == EventTaskStarted || EventType(event.Type) == EventRunInputsResolved {
			t.Fatalf("execution/input event escaped failed resolver admission: %#v", event)
		}
	}
}

func TestInterruptedRunReusesFrozenInputsAndRejectsResumeDrift(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.SetSessionData(NewSession())
	definitions := []RunInputDefinition{{Name: "count", Schema: RunInputSchema{Type: "integer"}, Required: true}}
	coordinator.session.RunInputDefinitions = definitions
	snapshot, err := ResolveRunInputSnapshot(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}}, "run-old", "run-old:invocation", coordinator.session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.sessionData.RunInputSnapshots = []RunInputSnapshot{*snapshot}
	coordinator.sessionData.ActiveRunInputSnapshotID = snapshot.ID
	coordinator.taskTracker.TodoList().SetRunID("run-old")
	items := coordinator.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interrupted"}})
	items[0].Status = TaskInProgress

	frozen, interrupted := coordinator.interruptedRunInputSnapshot()
	if !interrupted || frozen == nil || frozen.SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("frozen=%#v interrupted=%t", frozen, interrupted)
	}
	if err := validateResumeRunInputAssignments(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: RunInputSourceCLI}}, frozen, coordinator.session.Config.Name); err != nil {
		t.Fatalf("same resume input rejected: %v", err)
	}
	if err := validateResumeRunInputAssignments(definitions, []RunInputAssignment{{Name: "count", RawValue: []byte("4"), Source: RunInputSourceCLI}}, frozen, coordinator.session.Config.Name); err == nil || !strings.Contains(err.Error(), "resume_input_conflict") {
		t.Fatalf("resume drift error = %v", err)
	}
}

func TestRunInputSchemasEnforceSupportedScalarArrayAndSizeBounds(t *testing.T) {
	tests := []struct {
		name   string
		schema RunInputSchema
		value  string
		ok     bool
	}{
		{name: "string", schema: RunInputSchema{Type: "string", MinLength: new(2), MaxLength: new(3)}, value: `"界a"`, ok: true},
		{name: "short string", schema: RunInputSchema{Type: "string", MinLength: new(2)}, value: `"a"`},
		{name: "number", schema: RunInputSchema{Type: "number", Minimum: new(0.5), Maximum: new(1.5)}, value: `1.25`, ok: true},
		{name: "number high", schema: RunInputSchema{Type: "number", Maximum: new(1.0)}, value: `1.25`},
		{name: "boolean", schema: RunInputSchema{Type: "boolean"}, value: `true`, ok: true},
		{name: "array", schema: RunInputSchema{Type: "array", Items: &RunInputSchema{Type: "integer"}, MinItems: new(1), MaxItems: new(2)}, value: `[1,2]`, ok: true},
		{name: "array long", schema: RunInputSchema{Type: "array", Items: &RunInputSchema{Type: "integer"}, MaxItems: new(1)}, value: `[1,2]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateAndCanonicalizeRunInput(test.schema, []byte(test.value))
			if (err == nil) != test.ok {
				t.Fatalf("error = %v, want ok=%t", err, test.ok)
			}
		})
	}
	oversized := `"` + strings.Repeat("x", maxRunInputValueBytes) + `"`
	if _, err := validateAndCanonicalizeRunInput(RunInputSchema{Type: "string"}, []byte(oversized)); err == nil {
		t.Fatal("oversized value was accepted")
	}
}

func TestResolveRunInputSnapshotNoInputTeamHasZeroBehaviorChange(t *testing.T) {
	snapshot, err := ResolveRunInputSnapshot(nil, nil, "run", "invocation", "legacy")
	if err != nil || snapshot != nil {
		t.Fatalf("no-input resolution = %#v, err=%v", snapshot, err)
	}
	if _, err := ResolveRunInputSnapshot(nil, []RunInputAssignment{{Name: "unexpected", RawValue: []byte("1"), Source: RunInputSourceCLI}}, "run", "invocation", "legacy"); err == nil || !strings.Contains(err.Error(), "input_unknown") {
		t.Fatalf("undeclared explicit input error = %v", err)
	}
}

func TestResolveRunInputSnapshotEnforcesAggregateSize(t *testing.T) {
	definitions := make([]RunInputDefinition, maxRunInputDefinitions)
	value := json.RawMessage(`"` + strings.Repeat("x", 5_000) + `"`)
	for index := range definitions {
		definitions[index] = RunInputDefinition{Name: "value." + strconv.Itoa(index), Schema: RunInputSchema{Type: "string"}, Default: value}
	}
	if _, err := ResolveRunInputSnapshot(definitions, nil, "run", "invocation", "demo"); err == nil || !strings.Contains(err.Error(), "snapshot exceeds") {
		t.Fatalf("aggregate snapshot error = %v", err)
	}
}

func TestRunInputObjectValidationAndHashStability(t *testing.T) {
	allowAdditional := false
	schema := RunInputSchema{Type: "object", Properties: map[string]RunInputSchema{
		"kind":  {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"last_n"`)}},
		"count": {Type: "integer"},
	}, RequiredProperties: []string{"kind", "count"}, AdditionalProperties: &allowAdditional}
	first, err := validateAndCanonicalizeRunInput(schema, []byte(`{"kind":"last_n","count":10}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := validateAndCanonicalizeRunInput(schema, []byte(`{"count":10,"kind":"last_n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || runInputHash(first) != runInputHash(second) {
		t.Fatalf("canonical values differ: %s / %s", first, second)
	}
	for _, invalid := range []string{`{"kind":"other","count":10}`, `{"kind":"last_n"}`, `{"kind":"last_n","count":10,"extra":true}`} {
		if _, err := validateAndCanonicalizeRunInput(schema, []byte(invalid)); err == nil {
			t.Fatalf("accepted invalid object %s", invalid)
		}
	}
}

func TestRunInputSnapshotCloneHasNoMutableAliases(t *testing.T) {
	original := &RunInputSnapshot{Inputs: []ResolvedRunInput{{CanonicalValue: json.RawMessage(`{"x":1}`), Evidence: []InputEvidence{{Location: "original"}}}}}
	clone := CloneRunInputSnapshot(original)
	clone.Inputs[0].CanonicalValue[2] = 'y'
	clone.Inputs[0].Evidence[0].Location = "changed"
	if string(original.Inputs[0].CanonicalValue) != `{"x":1}` || original.Inputs[0].Evidence[0].Location != "original" {
		t.Fatalf("clone mutated original: %#v", original)
	}
}

func TestRunInputsResolvedEventReplaysFrozenSnapshot(t *testing.T) {
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{Name: "value", Schema: RunInputSchema{Type: "string"}, Default: json.RawMessage(`"x"`)}}, nil, "run-1", "invocation-1", "demo")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	session := ReduceToSessionData([]RunEvent{{Type: string(EventRunInputsResolved), Payload: payload}})
	if len(session.RunInputSnapshots) != 1 || session.RunInputSnapshots[0].SnapshotHash != snapshot.SnapshotHash || session.ActiveRunInputSnapshotID != snapshot.ID {
		t.Fatalf("replayed snapshots = %#v active=%q", session.RunInputSnapshots, session.ActiveRunInputSnapshotID)
	}
}

func TestRunInputsResolvedEventRetainsChatInvocationHistory(t *testing.T) {
	definition := []RunInputDefinition{{Name: "value", Schema: RunInputSchema{Type: "integer"}, Required: true}}
	first, err := ResolveRunInputSnapshot(definition, []RunInputAssignment{{Name: "value", RawValue: []byte("1"), Source: RunInputSourceCLI}}, "run-1", "run-1:invocation", "demo")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveRunInputSnapshot(definition, []RunInputAssignment{{Name: "value", RawValue: []byte("2"), Source: RunInputSourceCLI}}, "run-2", "run-2:invocation", "demo")
	if err != nil {
		t.Fatal(err)
	}
	firstPayload, _ := json.Marshal(first)
	secondPayload, _ := json.Marshal(second)
	session := ReduceToSessionData([]RunEvent{
		{Type: string(EventRunInputsResolved), Payload: firstPayload},
		{Type: string(EventRunInputsResolved), Payload: secondPayload},
	})
	if len(session.RunInputSnapshots) != 2 || session.ActiveRunInputSnapshotID != second.ID {
		t.Fatalf("snapshots=%#v active=%q", session.RunInputSnapshots, session.ActiveRunInputSnapshotID)
	}
}

func TestExecutionPolicyConfigurationHashIncludesRunInputSchema(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	without := coordinator.ExecutionPolicySnapshot()
	coordinator.session.RunInputDefinitions = []RunInputDefinition{{Name: "value", Schema: RunInputSchema{Type: "string"}, Default: json.RawMessage(`"x"`)}}
	with, err := newExecutionPolicyState(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if with.snapshot.RunInputSchemaHash == "" || with.snapshot.ConfigurationHash == without.ConfigurationHash {
		t.Fatalf("input schema was not bound into execution policy: before=%#v after=%#v", without, with.snapshot)
	}
}

func TestRunInputSnapshotIsPersistedAfterPolicyAndBeforeProviderBoundary(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{{Name: "value", Schema: RunInputSchema{Type: "string"}, Required: true}}
	state, err := newExecutionPolicyState(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.executionPolicy = state
	coordinator.SetRunInputAssignments([]RunInputAssignment{{Name: "value", RawValue: []byte(`"chosen"`), Source: RunInputSourceCLI}})
	_, end := coordinator.beginInvocationExecutionRun(t.Context())
	defer end()
	if err := coordinator.checkRunAdmission(); err != nil {
		t.Fatalf("checkRunAdmission: %v", err)
	}
	if err := coordinator.resolveRunInputsForInvocation(t.Context(), "prompt"); err != nil {
		t.Fatalf("resolveRunInputsForInvocation: %v", err)
	}
	events, err := coordinator.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	policyIndex, inputsIndex := -1, -1
	for index, event := range events {
		switch EventType(event.Type) {
		case EventExecutionPolicySnapshot:
			policyIndex = index
		case EventRunInputsResolved:
			inputsIndex = index
			if !strings.HasPrefix(event.IdempotencyKey, "run-inputs:") {
				t.Fatalf("run_inputs_resolved idempotency key = %q", event.IdempotencyKey)
			}
		case EventTaskCreated, EventTaskStarted:
			t.Fatalf("task/provider boundary event appeared during input admission: %#v", event)
		}
	}
	if policyIndex < 0 || inputsIndex <= policyIndex {
		t.Fatalf("event order policy=%d inputs=%d events=%#v", policyIndex, inputsIndex, events)
	}
	if snapshot := coordinator.RunInputSnapshot(); snapshot == nil || snapshot.RunID == "" || snapshot.Inputs[0].Name != "value" {
		t.Fatalf("coordinator snapshot = %#v", snapshot)
	}
}

func writeTypedInputFixture(t *testing.T, dir, manifest string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: coordinator\nrole: coordinator\n---\nCoordinate.\n"
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
}
