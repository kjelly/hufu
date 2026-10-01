package tools

import (
	"testing"

	"charm.land/fantasy"
)

func TestCanonicalToolArgumentCase(t *testing.T) {
	info := fantasy.ToolInfo{Name: "probe", Parameters: map[string]any{
		"status":  map[string]any{"type": "string", "enum": []string{"success", "blocked", "completed_with_gaps"}},
		"summary": map[string]any{"type": "string"},
		"mode":    map[string]any{"type": "string", "enum": []any{"Fast", "fast"}},
		"kind":    map[string]any{"oneOf": []any{map[string]any{"type": "string", "enum": []string{"read-only"}}, map[string]any{"type": "string", "enum": []string{"workspace-write"}}}},
		"findings": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "blocker"}},
			"detail":   map[string]any{"type": "string"},
		}}},
		"count": map[string]any{"type": "integer"},
	}}
	tests := []struct {
		name    string
		input   string
		want    string
		changed bool
	}{
		{name: "upper-case status", input: `{"status":"BLOCKED","summary":"BLOCKED by guard"}`, want: `{"status":"blocked","summary":"BLOCKED by guard"}`, changed: true},
		{name: "spaces and mixed case", input: `{"status":" Completed_With_Gaps "}`, want: `{"status":"completed_with_gaps"}`, changed: true},
		{name: "nested array enum", input: `{"findings":[{"severity":"WARNING","detail":"High & <low>"},{"severity":"info"}]}`, want: `{"findings":[{"detail":"High & <low>","severity":"warning"},{"severity":"info"}]}`, changed: true},
		{name: "oneOf enum", input: `{"kind":"Workspace-Write"}`, want: `{"kind":"workspace-write"}`, changed: true},
		{name: "numbers survive re-encoding", input: `{"status":"Success","count":12345678901234567890}`, want: `{"count":12345678901234567890,"status":"success"}`, changed: true},
		{name: "exact value untouched", input: `{"status":"blocked","summary":"x"}`, changed: false},
		{name: "status with a reason is not guessed", input: `{"status":"BLOCKED: guard denied"}`, changed: false},
		{name: "unknown value", input: `{"status":"done"}`, changed: false},
		{name: "ambiguous fold", input: `{"mode":"FAST"}`, changed: false},
		{name: "exact member of case-distinct enum", input: `{"mode":"Fast"}`, changed: false},
		{name: "free text never touched", input: `{"summary":"SUCCESS"}`, changed: false},
		{name: "invalid JSON", input: `{"status":`, changed: false},
		{name: "empty", input: ``, changed: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, changed := CanonicalToolArgumentCase(test.input, info)
			if changed != test.changed {
				t.Fatalf("changed = %v, want %v (got %s)", changed, test.changed, got)
			}
			want := test.want
			if !test.changed {
				want = test.input
			}
			if got != want {
				t.Fatalf("got %s, want %s", got, want)
			}
		})
	}
}
