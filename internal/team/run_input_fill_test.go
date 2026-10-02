package team

import (
	"encoding/json"
	"testing"
)

func TestFillSingleValueRequiredProperties(t *testing.T) {
	one := func(value string) []json.RawMessage { return []json.RawMessage{json.RawMessage(value)} }
	scope := RunInputSchema{
		Type: "object",
		Properties: map[string]RunInputSchema{
			"kind":        {Type: "string", Enum: []json.RawMessage{json.RawMessage(`"last_n"`), json.RawMessage(`"since"`)}},
			"count":       {Type: "integer"},
			"history":     {Type: "string", Enum: one(`"first_parent"`)},
			"head":        {Type: "string"},
			"commit_type": {Type: "string", Enum: one(`"feat"`)},
			"window":      {Type: "object", Properties: map[string]RunInputSchema{"unit": {Type: "string", Enum: one(`"day"`)}}, RequiredProperties: []string{"unit"}},
		},
		RequiredProperties: []string{"kind", "history", "head"},
	}
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing single-value required property is filled", raw: `{"kind":"last_n","count":2,"head":"HEAD"}`, want: `{"count":2,"head":"HEAD","history":"first_parent","kind":"last_n"}`},
		{name: "present value is left for validation", raw: `{"kind":"last_n","history":"all","head":"HEAD"}`, want: `{"kind":"last_n","history":"all","head":"HEAD"}`},
		{name: "required property with several values stays missing", raw: `{"history":"first_parent","head":"HEAD"}`, want: `{"history":"first_parent","head":"HEAD"}`},
		{name: "required property without an enum stays missing", raw: `{"kind":"last_n","history":"first_parent"}`, want: `{"kind":"last_n","history":"first_parent"}`},
		{name: "optional single-value property is not added", raw: `{"kind":"last_n","history":"first_parent","head":"HEAD"}`, want: `{"kind":"last_n","history":"first_parent","head":"HEAD"}`},
		{name: "nested object is filled", raw: `{"kind":"since","history":"first_parent","head":"HEAD","window":{}}`, want: `{"head":"HEAD","history":"first_parent","kind":"since","window":{"unit":"day"}}`},
		{name: "large integer keeps its digits", raw: `{"kind":"last_n","count":12345678901234567890,"head":"HEAD"}`, want: `{"count":12345678901234567890,"head":"HEAD","history":"first_parent","kind":"last_n"}`},
		{name: "non-object value is unchanged", raw: `"last_n"`, want: `"last_n"`},
		{name: "invalid JSON is unchanged", raw: `{"kind":`, want: `{"kind":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(fillSingleValueRequiredProperties(scope, json.RawMessage(tt.raw))); got != tt.want {
				t.Fatalf("filled = %s, want %s", got, tt.want)
			}
		})
	}
}

// Reproduces the failing review run: the semantic model omits history, which
// the schema pins to first_parent, on both the first answer and the repair.
func TestSemanticRunInputFillsSingleValueRequiredProperty(t *testing.T) {
	coordinator := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	coordinator.session.RunInputDefinitions = []RunInputDefinition{semanticScopeDefinition(runInputResolverModeSemanticJSON)}
	semantic := &recordingSemanticRunInputResolver{value: json.RawMessage(`{"kind":"last_n","count":2,"head":"HEAD"}`)}
	coordinator.SetSemanticRunInputResolver(semantic)
	registerSemanticCandidateValidator(coordinator, nil)

	assignments, err := coordinator.resolveRunInputCandidates(t.Context(), "review 最近2個的 git commit", nil, "run-1", true)
	if err != nil {
		t.Fatalf("resolveRunInputCandidates: %v", err)
	}
	if len(assignments) != 1 || string(assignments[0].RawValue) != `{"count":2,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("assignments = %#v", assignments)
	}
	if len(semantic.requests) != 1 {
		t.Fatalf("semantic requests = %d, want one answer without a repair", len(semantic.requests))
	}
}
