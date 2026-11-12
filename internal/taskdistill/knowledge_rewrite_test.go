package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestKnowledgeRewritePreservesBindingsAndReusesCanonicalCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		afterGeneral bool
		multiple     bool
	}{
		{"initial length failure", false, false},
		{"length after ordinary correction", true, false},
		{"multiple overlong facts", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := []Source{{ID: "a", Content: "frozen applicability"}, {ID: "b", Content: "frozen failure evidence"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified a"}, {"b", "distill", "verified b"}}, Knowledge: []Knowledge{
				{Text: "  Keep this fact and indentation unchanged.\n", SourceIDs: []string{"a", "b"}},
				{Text: strings.Repeat("界", 2043), SourceIDs: []string{"a", "b"}},
			}}
			texts := [][]string{{strings.Repeat("界", 1000), strings.Repeat("界", 1043)}}
			if tc.multiple {
				candidate.Knowledge = append(candidate.Knowledge, Knowledge{Text: strings.Repeat("則", 2044), SourceIDs: []string{"b"}})
				texts = append(texts, []string{strings.Repeat("則", 1000), strings.Repeat("則", 1044)})
			}
			raw, _ := json.Marshal(candidate)
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if tc.afterGeneral && calls == 1 {
					return `{"decisions":[{"source_id":12,"action":"retain","reason":"bad type"}],"knowledge":[]}`, nil
				}
				if calls == 1 || tc.afterGeneral && calls == 2 {
					return string(raw), nil
				}
				_, targets, found := strings.Cut(prompt, "\nTARGETS: ")
				targets, frozen, frozenFound := strings.Cut(targets, "\nFROZEN_DATA: ")
				var actualTargets []Knowledge
				var input struct {
					Sources []Source `json:"sources"`
				}
				if !found || !frozenFound || json.Unmarshal([]byte(targets), &actualTargets) != nil || json.Unmarshal([]byte(frozen), &input) != nil || !reflect.DeepEqual(input.Sources, sources) || !reflect.DeepEqual(actualTargets, candidate.Knowledge[1:]) {
					t.Fatal("text repair lost ordered targets or frozen evidence")
				}
				response, _ := json.Marshal(struct {
					Texts [][]string `json:"texts"`
				}{texts})
				return string(response), nil
			}
			wantCalls := 2
			if tc.afterGeneral {
				wantCalls++
			}
			canonical, _ := draftPrompt(sources, "preserve all conditions")
			legacyKey := Digest(struct {
				Prompt       string
				SplitContent bool
			}{canonical, true})
			draft, err := newDraftRunner(g, wantCalls).generate(t.Context(), sources, "preserve all conditions", 0)
			if err != nil || calls != wantCalls || Validate(draft, sources) != nil || !reflect.DeepEqual(draft.Decisions, candidate.Decisions) || !reflect.DeepEqual(draft.Knowledge[0], candidate.Knowledge[0]) {
				t.Fatalf("bindings changed or repair failed: calls=%d err=%v", calls, err)
			}
			next := 1
			for index, parts := range texts {
				if strings.Join(parts, "") != candidate.Knowledge[index+1].Text {
					t.Fatal("fixture did not preserve complete original text")
				}
				for _, text := range parts {
					if draft.Knowledge[next].Text != text || !reflect.DeepEqual(draft.Knowledge[next].SourceIDs, candidate.Knowledge[index+1].SourceIDs) {
						t.Fatal("runtime changed text bytes or copied citations from another target")
					}
					next++
				}
			}
			if next != len(draft.Knowledge) || len(g.checkpoints[legacyKey].Draft) == 0 || len(g.checkpoints) != 1 {
				t.Fatal("lost facts or changed the canonical checkpoint key")
			}
			if _, err := newDraftRunner(g, 1).generate(t.Context(), sources, "preserve all conditions", 0); err != nil || calls != wantCalls {
				t.Fatalf("valid checkpoint repeated model work: %v", err)
			}
		})
	}
}

func TestKnowledgeRewriteRejectsInvalidReplacementsWithoutAnotherRepair(t *testing.T) {
	long := strings.Repeat("x", 2043)
	for _, raw := range []string{
		`{"texts":[]}`, `{"texts":[[]]}`, `{"texts":[null]}`, `{"texts":[[" "]]}`,
		`{"texts":[["` + long + `"]]}`,
		`{"texts":[["AKIAAAAAAAAAAAAAAAAA"]]}`,
		`{"texts":[["Evidence [REDACTED]"]]}`,
		`{"texts":[["ok"]],"decisions":[]}`,
		`{"texts":[[{"text":"ok","source_ids":["other"]}]]}`,
	} {
		calls := 0
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) {
			calls++
			if calls == 1 {
				return `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + long + `","source_ids":["a"]}]}`, nil
			}
			return raw, nil
		}}
		draft, err := newDraftRunner(g, 8).generate(t.Context(), []Source{{ID: "a"}}, "", 0)
		if !errors.Is(err, errModelContract) || calls != 2 || len(g.checkpoints) != 0 || len(draft.Decisions) != 0 {
			t.Fatalf("invalid text repair accepted or repeated: calls=%d err=%v", calls, err)
		}
	}
}

func TestKnowledgeRewriteEligibilityKeepsProtectionPrivacyAndReferenceGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Draft, []Source)
	}{
		{"protected", func(_ *Draft, s []Source) { s[0].Protected = true }},
		{"unknown citation", func(d *Draft, _ []Source) { d.Knowledge[0].SourceIDs = []string{"unknown"} }},
		{"retained citation", func(d *Draft, _ []Source) { d.Decisions[0].Action = "retain" }},
		{"blank rationale", func(d *Draft, _ []Source) { d.Decisions[0].Reason = " " }},
		{"secret rationale", func(d *Draft, _ []Source) { d.Decisions[0].Reason = "AKIAAAAAAAAAAAAAAAAA" }},
		{"secret beyond length limit", func(d *Draft, _ []Source) { d.Knowledge[0].Text += " AKIAAAAAAAAAAAAAAAAA" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := []Source{{ID: "a"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified"}}, Knowledge: []Knowledge{{strings.Repeat("x", 2043), []string{"a"}}}}
			tc.mutate(&candidate, sources)
			raw, _ := json.Marshal(candidate)
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				if strings.Contains(prompt, "\nTARGETS: ") {
					t.Fatal("invalid candidate gained text-only repair authority")
				}
				return string(raw), nil
			}}
			if _, err := newDraftRunner(g, 8).generate(t.Context(), sources, "", 0); err == nil || calls != 2 || len(g.checkpoints) != 0 {
				t.Fatalf("guard bypassed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestKnowledgeRewriteSharesCallBudgetWithOrdinaryCorrection(t *testing.T) {
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			return `{}`, nil
		}
		return `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + strings.Repeat("x", 2043) + `","source_ids":["a"]}]}`, nil
	}}
	if _, err := newDraftRunner(g, 2).generate(t.Context(), []Source{{ID: "a"}}, "", 0); !errors.Is(err, ErrCallBudget) || calls != 2 || len(g.checkpoints) != 0 {
		t.Fatalf("text rewrite bypassed the shared call budget: calls=%d err=%v", calls, err)
	}
}

func TestKnowledgeRewriteNeverRepairsInvalidStoredCheckpoints(t *testing.T) {
	sources := []Source{{ID: "a"}}
	canonical, _ := draftPrompt(sources, "")
	key := Digest(struct {
		Prompt       string
		SplitContent bool
	}{canonical, true})
	raw := []byte(`{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + strings.Repeat("x", 2043) + `","source_ids":["a"]}]}`)
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{key: {Draft: raw}}, generatorFunc: func(context.Context, string) (string, error) {
		t.Fatal("stored checkpoint reached model repair")
		return "", nil
	}}
	if _, err := newDraftRunner(g, 8).generate(t.Context(), sources, "", 0); !errors.Is(err, ErrInvalid) || string(g.checkpoints[key].Draft) != string(raw) {
		t.Fatalf("invalid stored checkpoint was accepted or rewritten: %v", err)
	}
}

func TestKnowledgeRewriteDoesNotAuthorizeDiscardDuringConsolidation(t *testing.T) {
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "\nTARGETS: ") {
			t.Fatal("merge discard gained text-only repair authority")
		}
		return `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"},{"source_id":"b","action":"discard","reason":"forget selected fact"}],"knowledge":[{"text":"` + strings.Repeat("x", 2043) + `","source_ids":["a"]}]}`, nil
	}}
	runner := newDraftRunner(g, 8)
	runner.splitContent = false
	if _, err := runner.generate(t.Context(), []Source{{ID: "a"}, {ID: "b"}}, "", 0); err == nil || calls != 2 || len(g.checkpoints) != 0 {
		t.Fatalf("merge discard was accepted: calls=%d err=%v", calls, err)
	}
}
