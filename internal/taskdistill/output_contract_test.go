package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestModelKnowledgeCoalescesOnlyIdenticalTextAndPreservesEveryCitation(t *testing.T) {
	sources := []Source{{ID: "a"}, {ID: "b"}}
	draft := Draft{
		Decisions: []Decision{{"a", "distill", "verified"}, {"b", "distill", "verified"}},
		Knowledge: []Knowledge{
			{"timeout is 50ms", []string{"a"}}, {"timeout is 50ms", []string{"b"}},
			{"timeout is 500ms", []string{"b"}},
			{"    preserve_code_indentation()\n", []string{"a"}},
			{"preserve_code_indentation()\n", []string{"b"}},
		},
	}
	raw, _ := json.Marshal(draft)
	if err := Validate(draft, sources); err == nil {
		t.Fatal("canonical storage accepted duplicate knowledge")
	}
	calls := 0
	result, err := GenerateBatched(t.Context(), generatorFunc(func(context.Context, string) (string, error) {
		calls++
		return string(raw), nil
	}), sources, "", 1)
	if err != nil || calls != 1 || len(result.Knowledge) != 4 || !reflect.DeepEqual(result.Knowledge[0].SourceIDs, []string{"a", "b"}) {
		t.Fatalf("lossless normalization failed: %+v calls=%d err=%v", result, calls, err)
	}
	for i, text := range []string{"timeout is 50ms", "timeout is 500ms", "    preserve_code_indentation()\n", "preserve_code_indentation()\n"} {
		if result.Knowledge[i].Text != text {
			t.Fatalf("fact changed at %d: %q", i, result.Knowledge[i].Text)
		}
	}
	input := []Knowledge{{Text: "same fact", SourceIDs: []string{"z", "a"}}, {Text: "same fact", SourceIDs: []string{"b"}}}
	before, _ := json.Marshal(input)
	_ = mergeExactKnowledge(input)
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("normalization mutated frozen input citations")
	}
}

func TestModelContractViolationsUseOneSharedCorrectionAndKeepAllGuards(t *testing.T) {
	fakeSecret := "AKIA" + strings.Repeat("A", 16)
	base := Draft{Decisions: []Decision{{"a", "distill", "verified"}, {"b", "distill", "verified"}}, Knowledge: []Knowledge{{"public verified rule", []string{"a", "b"}}}}
	for _, tc := range []struct {
		name, code string
		mutate     func(*Draft, []Source)
	}{
		{"blank fact", "blank_knowledge", func(d *Draft, _ []Source) { d.Knowledge[0].Text = " \n" }},
		{"redacted fact", "redacted_knowledge", func(d *Draft, _ []Source) { d.Knowledge[0].Text = "Evidence [REDACTED]" }},
		{"sensitive fact", "secret_knowledge", func(d *Draft, _ []Source) { d.Knowledge[0].Text = fakeSecret }},
		{"protected source", "protected_source", func(_ *Draft, s []Source) { s[0].Protected = true }},
		{"retained citation", "citation_not_distilled", func(d *Draft, _ []Source) { d.Decisions[0].Action = "retain" }},
		{"missing coverage", "uncovered_distill_source", func(d *Draft, _ []Source) { d.Knowledge = []Knowledge{} }},
		{"duplicate hides unknown citation", "unknown_citation", func(d *Draft, _ []Source) {
			d.Knowledge = append(d.Knowledge, Knowledge{d.Knowledge[0].Text, []string{"unknown"}})
		}},
	} {
		for _, corrected := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "rejected", true: "corrected"}[corrected], func(t *testing.T) {
				sources := []Source{{ID: "a", Content: "public evidence"}, {ID: "b", Content: "public evidence"}}
				baseRaw, _ := json.Marshal(base)
				var candidate Draft
				_ = json.Unmarshal(baseRaw, &candidate)
				tc.mutate(&candidate, sources)
				raw, _ := json.Marshal(candidate)
				calls := 0
				g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
				g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
					calls++
					if strings.Contains(prompt, fakeSecret) {
						t.Fatal("rejected secret was echoed into the corrective request")
					}
					if calls == 1 || !corrected {
						return string(raw), nil
					}
					if !strings.Contains(prompt, tc.code) {
						t.Fatalf("missing precise correction code %q", tc.code)
					}
					return `{"decisions":[{"source_id":"a","action":"retain","reason":"preserve evidence"},{"source_id":"b","action":"retain","reason":"preserve evidence"}],"knowledge":[]}`, nil
				}
				result, err := GenerateBatched(t.Context(), g, sources, "", 8)
				if calls != 2 {
					t.Fatalf("correction was not bounded: calls=%d err=%v", calls, err)
				}
				if corrected {
					if err != nil || Validate(result, sources) != nil || len(g.checkpoints) != 1 {
						t.Fatalf("valid replacement rejected: %+v %v", result, err)
					}
					return
				}
				issue, ok := errors.AsType[*OutputViolation](err)
				if !ok || issue.Code != tc.code || len(result.Decisions) != 0 || len(g.checkpoints) != 0 || strings.Contains(err.Error(), fakeSecret) {
					t.Fatalf("bad candidate accepted, hidden, or persisted: %+v %v", result, err)
				}
			})
		}
	}
}

func TestSchemaViolationsReceiveCorrectionWithoutParsingOrCoercingTheirMeaning(t *testing.T) {
	for _, raw := range []string{
		"```json\n{}\n```", `{"decisions":[],"knowledge":[]}`, `{"decisions":[],"knowledge":null}`,
		`{"decisions":[{"source_id":123,"action":"discard","reason":"transient"}],"knowledge":[]}`,
		`{"decisions":[{"source_id":"a","action":"DISCARD","reason":"transient"}],"knowledge":[]}`,
	} {
		calls := 0
		g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
			calls++
			if calls == 1 {
				return raw, nil
			}
			return discardedReply(t, requestedSources(t, prompt)), nil
		})
		result, err := GenerateBatched(t.Context(), g, []Source{{ID: "a"}}, "", 2)
		if err != nil || calls != 2 || len(result.Decisions) != 1 {
			t.Fatalf("schema correction failed: calls=%d result=%+v err=%v", calls, result, err)
		}
	}
}

func TestMergeNeverCachesDiscardAndCanReuseValidLegacyWork(t *testing.T) {
	sources := []Source{{ID: "fact_0", Content: "Preserve the entire selected fact."}}
	canonical, _ := draftPrompt(sources, "")
	legacy := Digest(struct {
		Prompt       string
		SplitContent bool
	}{canonical, false})
	for _, action := range []string{"retain", "discard"} {
		raw, _ := json.Marshal(Draft{Decisions: []Decision{{"fact_0", action, "legacy proposal"}}, Knowledge: []Knowledge{}})
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{legacy: {Draft: raw}}}
		calls := 0
		g.generatorFunc = func(context.Context, string) (string, error) {
			calls++
			return `{"decisions":[{"source_id":"fact_0","action":"retain","reason":"preserve selected fact"}],"knowledge":[]}`, nil
		}
		runner := newDraftRunner(g, 2)
		runner.splitContent = false
		draft, err := runner.generate(t.Context(), sources, "", 0)
		wantCalls := 0
		if action == "discard" {
			wantCalls = 1
		}
		if err != nil || draft.Decisions[0].Action != "retain" || calls != wantCalls || string(g.checkpoints[legacy].Draft) != string(raw) {
			t.Fatalf("legacy merge was overwritten or rejected valid work: calls=%d draft=%+v err=%v", calls, draft, err)
		}
	}
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) { return discardedReply(t, sources), nil }}
	runner := newDraftRunner(g, 8)
	runner.splitContent = false
	if _, err := runner.generate(t.Context(), sources, "", 0); err == nil || len(g.checkpoints) != 0 {
		t.Fatalf("discard of selected facts was cached: %v", err)
	}
}
