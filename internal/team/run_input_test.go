package team

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
	if session.RunInputSnapshot == nil || session.RunInputSnapshot.SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("replayed snapshot = %#v", session.RunInputSnapshot)
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
	if err := coordinator.resolveRunInputsForInvocation("prompt"); err != nil {
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
