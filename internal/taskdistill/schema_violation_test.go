package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSchemaViolationFeedbackRepairsExactFieldsWithoutLeakingValues(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	for _, tc := range []struct {
		name, raw, path, rule, detail string
	}{
		{"missing array", `{"decisions":[{"source_id":"a","action":"retain","reason":"needed"}]}`, "$", "required", "missing=knowledge"},
		{"null array", `{"decisions":[{"source_id":"a","action":"retain","reason":"needed"}],"knowledge":null}`, "knowledge", "type", "expected=array, got=null"},
		{"wrong source type", `{"decisions":[{"source_id":123,"action":"retain","reason":"needed"}],"knowledge":[]}`, "decisions[0].source_id", "type", "expected=string, got=number"},
		{"oversize fact", `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + strings.Repeat("界", 2001) + `","source_ids":["a"]}]}`, "knowledge[0].text", "maxLength", "limit=2000, got=2001"},
		{"invalid action", `{"decisions":[{"source_id":"a","action":"` + secret + `","reason":"needed"}],"knowledge":[]}`, "decisions[0].action", "enum", "use a value declared"},
		{"unknown key", `{"decisions":[{"source_id":"a","action":"retain","reason":"needed","` + secret + `":"private"}],"knowledge":[]}`, "decisions[0]", "additionalProperties", "undeclared fields"},
		{"duplicate citation", `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"verified fact","source_ids":["a","a"]}]}`, "knowledge[0].source_ids", "uniqueItems", "duplicate array entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := []Source{{ID: "a", Content: "verified evidence"}}
			_, rejected := decodeModelDraft(tc.raw, sources)
			issue, ok := errors.AsType[*OutputViolation](rejected)
			if !ok || issue.Code != "schema_violation" || issue.Path != tc.path || len(issue.SchemaIssues) != 1 {
				t.Fatalf("missing precise schema issue: %+v", rejected)
			}
			if _, ok := errors.AsType[*jsonschema.ValidationError](rejected); !ok {
				t.Fatal("underlying typed validator error was lost")
			}
			published, err := json.Marshal(issue)
			if err != nil || strings.Contains(string(published), secret) || strings.Contains(issue.Error(), secret) {
				t.Fatal("schema diagnostic leaked rejected data")
			}
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if calls == 1 {
					return tc.raw, nil
				}
				if !strings.Contains(prompt, tc.path) || !strings.Contains(prompt, tc.rule) || !strings.Contains(prompt, tc.detail) || strings.Contains(prompt, secret) {
					t.Fatal("correction lacks precise feedback or leaked rejected data")
				}
				if tc.name == "oversize fact" {
					return `{"texts":[["Preserve the verified evidence."]]}`, nil
				}
				return `{"decisions":[{"source_id":"a","action":"retain","reason":"preserve evidence"}],"knowledge":[]}`, nil
			}
			draft, err := GenerateBatched(t.Context(), g, sources, "", 2)
			wantAction := "retain"
			if tc.name == "oversize fact" {
				wantAction = "distill"
			}
			if err != nil || calls != 2 || len(g.checkpoints) != 1 || draft.Decisions[0].Action != wantAction {
				t.Fatalf("bounded correction failed: calls=%d draft=%+v err=%v", calls, draft, err)
			}
		})
	}
}

func TestSchemaViolationFeedbackIsBoundedAndInvalidReplacementsStayRejected(t *testing.T) {
	raw := `{"decisions":[{"source_id":0,"action":null,"reason":false}],"knowledge":[{"text":0,"source_ids":null}]}`
	_, err := parseDraft(raw)
	issue, ok := errors.AsType[*OutputViolation](err)
	if !ok || len(issue.SchemaIssues) != maxSchemaIssues || len(issue.Error()) > 1024 {
		t.Fatalf("unbounded schema diagnostic: %+v", err)
	}
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) {
		calls++
		return raw, nil
	}}
	result, err := GenerateBatched(t.Context(), g, []Source{{ID: "a"}}, "", 8)
	if !errors.Is(err, errModelContract) || calls != 2 || len(g.checkpoints) != 0 || len(result.Decisions) != 0 {
		t.Fatalf("invalid replacement accepted or unbounded retry: calls=%d result=%+v err=%v", calls, result, err)
	}
}
