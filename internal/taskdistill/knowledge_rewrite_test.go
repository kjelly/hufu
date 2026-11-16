package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
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

func TestKnowledgeRewriteRejectsInvalidReplacementsWithinRepairBound(t *testing.T) {
	long := strings.Repeat("x", 2043)
	for _, tc := range []struct {
		raw   string
		calls int
	}{
		{`{"texts":[]}`, 3}, {`{"texts":[[]]}`, 3}, {`{"texts":[null]}`, 2}, {`{"texts":[[" "]]}`, 2},
		{`{"texts":[""]}`, 3}, {`{"texts":[" "]}`, 2}, {`{"texts":[123]}`, 3},
		{`{"texts":["` + long + `"]}`, 3},
		{`{"texts":["AKIAAAAAAAAAAAAAAAAA"]}`, 2},
		{`{"texts":["Evidence [REDACTED]"]}`, 2},
		{`{"texts":[["` + long + `"]]}`, 3},
		{`{"texts":[["AKIAAAAAAAAAAAAAAAAA"]]}`, 2},
		{`{"texts":[["Evidence [REDACTED]"]]}`, 2},
		{`{"texts":[["ok"]],"decisions":[]}`, 3},
		{`{"texts":[[{"text":"ok","source_ids":["other"]}]]}`, 3},
		{`{"texts":["one","two","three","four","five","six"]}`, 3},
		{`{"texts":[`, 3},
	} {
		calls := 0
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) {
			calls++
			if calls == 1 {
				return `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + long + `","source_ids":["a"]}]}`, nil
			}
			return tc.raw, nil
		}}
		draft, err := newDraftRunner(g, 8).generate(t.Context(), []Source{{ID: "a"}}, "", 0)
		if !errors.Is(err, errModelContract) || calls != tc.calls || len(g.checkpoints) != 0 || len(draft.Decisions) != 0 {
			t.Fatalf("invalid text repair accepted or repeated: calls=%d want=%d err=%v", calls, tc.calls, err)
		}
	}
}

func TestKnowledgeRewriteAcceptsSingletonTextWithoutChangingBindings(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint("mixed=", mixed), func(t *testing.T) {
			sources := []Source{{ID: "a"}, {ID: "b"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified a"}, {"b", "distill", "verified b"}}, Knowledge: []Knowledge{
				{Text: "Unchanged fact.", SourceIDs: []string{"b"}},
				{Text: strings.Repeat("界", 2043), SourceIDs: []string{"a", "b"}},
			}}
			texts := []any{"  Preserve the constraint and its exception.\n"}
			want := []Knowledge{candidate.Knowledge[0], {Text: texts[0].(string), SourceIDs: []string{"a", "b"}}}
			if mixed {
				candidate.Knowledge = append(candidate.Knowledge, Knowledge{Text: strings.Repeat("則", 2043), SourceIDs: []string{"b"}})
				texts = append(texts, []string{"First supported condition.", "Second supported condition."})
				want = append(want, Knowledge{Text: "First supported condition.", SourceIDs: []string{"b"}}, Knowledge{Text: "Second supported condition.", SourceIDs: []string{"b"}})
			}
			raw, _ := json.Marshal(candidate)
			response, _ := json.Marshal(struct {
				Texts []any `json:"texts"`
			}{texts})
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(context.Context, string) (string, error) {
				calls++
				if calls == 1 {
					return string(raw), nil
				}
				return string(response), nil
			}}
			draft, err := newDraftRunner(g, 2).generate(t.Context(), sources, "", 0)
			if err != nil || calls != 2 || !reflect.DeepEqual(draft.Decisions, candidate.Decisions) || !reflect.DeepEqual(draft.Knowledge, want) {
				t.Fatalf("singleton rewrite lost text or bindings: calls=%d err=%v", calls, err)
			}
			if _, err := newDraftRunner(g, 1).generate(t.Context(), sources, "", 0); err != nil || calls != 2 {
				t.Fatalf("canonical checkpoint was not reused: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestKnowledgeRewriteCorrectsOverlongReplacementWithinSharedBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		budget     int
		persistent bool
		cancelled  bool
		wantCalls  int
		wantError  error
	}{
		{"corrected", 3, false, false, 3, nil},
		{"persistent length failure", 8, true, false, 3, errModelContract},
		{"budget exhausted", 2, false, false, 2, ErrCallBudget},
		{"cancelled after rejection", 3, false, true, 2, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sources := []Source{{ID: "a", Content: "frozen evidence"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified"}}, Knowledge: []Knowledge{
				{Text: "Unaffected constraint.", SourceIDs: []string{"a"}},
				{Text: strings.Repeat("界", 2200), SourceIDs: []string{"a"}},
			}}
			raw, _ := json.Marshal(candidate)
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				if calls == 1 {
					return string(raw), nil
				}
				if calls == 3 {
					if !strings.Contains(prompt, "maxLength: limit=2000, got=2431") || !strings.Contains(prompt, strings.Repeat("界", 2200)) || strings.Contains(prompt, strings.Repeat("則", 2431)) {
						t.Fatal("length correction lost its precise feedback or used rejected text as evidence")
					}
					if !tc.persistent {
						return `{"texts":[["First supported condition.","Its applicability and exception."]]}`, nil
					}
				}
				if tc.cancelled {
					cancel()
				}
				return `{"texts":["` + strings.Repeat("則", 2431) + `"]}`, nil
			}}
			draft, err := newDraftRunner(g, tc.budget).generate(ctx, sources, "preserve conditions", 0)
			if calls != tc.wantCalls || tc.wantError != nil && !errors.Is(err, tc.wantError) || tc.wantError == nil && err != nil {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
			if tc.wantError != nil {
				if len(g.checkpoints) != 0 || len(draft.Decisions) != 0 {
					t.Fatal("invalid repair was accepted or checkpointed")
				}
				return
			}
			want := []Knowledge{candidate.Knowledge[0], {Text: "First supported condition.", SourceIDs: []string{"a"}}, {Text: "Its applicability and exception.", SourceIDs: []string{"a"}}}
			if !reflect.DeepEqual(draft.Decisions, candidate.Decisions) || !reflect.DeepEqual(draft.Knowledge, want) {
				t.Fatal("length correction changed frozen bindings or unaffected facts")
			}
			if _, err := newDraftRunner(g, 1).generate(t.Context(), sources, "preserve conditions", 0); err != nil || calls != tc.wantCalls {
				t.Fatalf("corrected checkpoint did not survive restart: %v", err)
			}
		})
	}
}

func TestKnowledgeRewriteCorrectsRejectedSchemaWithoutChangingScope(t *testing.T) {
	for _, tc := range []struct{ name, raw, diagnostic string }{
		{"flattened split", `{"texts":["REJECTED_MARKER","two","three","four","five","six"]}`, "maxItems: limit=1, got=6"},
		{"wrong container", `{"texts":"REJECTED_MARKER"}`, "type: expected=array, got=string"},
		{"missing texts", `{}`, "missing=texts"},
		{"malformed JSON", `{"texts":["REJECTED_MARKER"`, "invalid_json"},
		{"unknown metadata", `{"texts":["ok"],"REJECTED_MARKER":"AKIAAAAAAAAAAAAAAAAA"}`, "undeclared fields are forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := []Source{{ID: "a", Content: "frozen condition"}, {ID: "b", Content: "frozen exception"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified a"}, {"b", "distill", "verified b"}}, Knowledge: []Knowledge{
				{Text: strings.Repeat("界", 2200), SourceIDs: []string{"a"}},
				{Text: "Unaffected constraint.", SourceIDs: []string{"b"}},
			}}
			raw, _ := json.Marshal(candidate)
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				switch calls {
				case 1:
					return string(raw), nil
				case 2:
					return tc.raw, nil
				case 3:
					if !strings.Contains(prompt, tc.diagnostic) || strings.Contains(prompt, "REJECTED_MARKER") || strings.Contains(prompt, "AKIAAAAAAAAAAAAAAAAA") {
						t.Fatal("missing safe diagnostic or rejected response leaked into repair")
					}
					_, targets, _ := strings.Cut(prompt, "\nTARGETS: ")
					targets, frozen, _ := strings.Cut(targets, "\nFROZEN_DATA: ")
					var actualTargets []Knowledge
					var input struct {
						Sources []Source `json:"sources"`
					}
					if json.Unmarshal([]byte(targets), &actualTargets) != nil || !reflect.DeepEqual(actualTargets, candidate.Knowledge[:1]) || json.Unmarshal([]byte(frozen), &input) != nil || !reflect.DeepEqual(input.Sources, sources) {
						t.Fatal("schema correction changed the frozen targets or evidence")
					}
					return `{"texts":[["Supported condition.","Its exception and scope."]]}`, nil
				default:
					t.Fatal("unbounded schema correction")
					return "", nil
				}
			}}
			draft, err := newDraftRunner(g, 3).generate(t.Context(), sources, "", 0)
			want := []Knowledge{{Text: "Supported condition.", SourceIDs: []string{"a"}}, {Text: "Its exception and scope.", SourceIDs: []string{"a"}}, candidate.Knowledge[1]}
			if err != nil || calls != 3 || !reflect.DeepEqual(draft.Decisions, candidate.Decisions) || !reflect.DeepEqual(draft.Knowledge, want) {
				t.Fatalf("schema correction failed or changed bindings: calls=%d err=%v", calls, err)
			}
			if _, err := newDraftRunner(g, 1).generate(t.Context(), sources, "", 0); err != nil || calls != 3 {
				t.Fatalf("schema-corrected checkpoint did not survive restart: %v", err)
			}
		})
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

func TestRepeatedOverlongKnowledgeRepairSubdividesFrozenSourcesAndResumes(t *testing.T) {
	for _, interruption := range []string{"transport", "cancel", "budget"} {
		t.Run(interruption, func(t *testing.T) {
			sources := []Source{{ID: "a", Content: "First complete source."}, {ID: "b", Content: "Second complete source."}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stopped := NewTransportError(errors.New("fixture transport interruption"))
			failRight, calls := true, 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if strings.Contains(prompt, "\nTARGETS: ") {
					return `{"texts":["` + strings.Repeat("x", 2750) + `"]}`, nil
				}
				requested := requestedSources(t, prompt)
				if len(requested) == 1 && requested[0] != sources[0] && requested[0] != sources[1] {
					t.Fatal("subdivision changed complete source identity or content")
				}
				if len(requested) == 1 && requested[0].ID == "b" && failRight {
					if interruption == "cancel" {
						cancel()
						return "", ctx.Err()
					}
					return "", stopped
				}
				draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
				var ids []string
				for _, source := range requested {
					ids = append(ids, source.ID)
					draft.Decisions = append(draft.Decisions, Decision{source.ID, "distill", "verified source"})
				}
				text := "Verified constraint from " + requested[0].ID
				if len(requested) > 1 {
					text = strings.Repeat("x", 2200)
				}
				draft.Knowledge = append(draft.Knowledge, Knowledge{text, ids})
				raw, err := json.Marshal(draft)
				return string(raw), err
			}
			budget, wantCalls, wantErr := 10, 6, error(stopped)
			if interruption == "cancel" {
				wantCalls, wantErr = 5, context.Canceled
			} else if interruption == "budget" {
				budget, wantCalls, wantErr = 4, 4, ErrCallBudget
			}
			_, err := newDraftRunner(g, budget).generate(ctx, sources, "keep every verified claim", 0)
			if !errors.Is(err, wantErr) || calls != wantCalls || len(g.checkpoints) == 0 {
				t.Fatalf("calls=%d checkpoints=%d err=%v", calls, len(g.checkpoints), err)
			}
			for _, checkpoint := range g.checkpoints {
				if strings.Contains(string(checkpoint.Draft), strings.Repeat("x", 2200)) {
					t.Fatal("checkpoint accepted rejected overlong text")
				}
			}
			failRight, calls = false, 0
			draft, err := newDraftRunner(g, 1).generate(t.Context(), sources, "keep every verified claim", 0)
			if err != nil || calls != 1 || len(draft.Knowledge) != 2 || Validate(draft, sources) != nil {
				t.Fatalf("resume calls=%d draft=%v err=%v", calls, draft, err)
			}
			if !reflect.DeepEqual(draft.Knowledge, []Knowledge{{"Verified constraint from a", []string{"a"}}, {"Verified constraint from b", []string{"b"}}}) {
				t.Fatal("source subdivision lost a claim or its provenance")
			}
			calls = 0
			_, err = newDraftRunner(g, 1).generate(t.Context(), sources, "keep every verified claim", 0)
			if err != nil || calls != 0 {
				t.Fatalf("replay calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRepeatedOverlongKnowledgeRepairPreservesOtherRejectionGates(t *testing.T) {
	long := strings.Repeat("x", 2750)
	for _, raw := range []string{
		`{"texts":["` + long + `"],"decisions":[]}`,
		`{"texts":["` + long + ` AKIAAAAAAAAAAAAAAAAA"]}`,
		`{"texts":[["` + long + ` [REDACTED]"]]}`,
		`{"texts":["` + strings.Repeat(" ", 2750) + `"]}`,
		`{"texts":["` + long + `","extra target"]}`,
	} {
		calls := 0
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
			calls++
			if strings.Contains(prompt, "\nTARGETS: ") {
				return raw, nil
			}
			return `{"decisions":[{"source_id":"a","action":"distill","reason":"verified"},{"source_id":"b","action":"distill","reason":"verified"}],"knowledge":[{"text":"` + strings.Repeat("x", 2200) + `","source_ids":["a","b"]}]}`, nil
		}}
		_, err := newDraftRunner(g, 10).generate(t.Context(), []Source{{ID: "a"}, {ID: "b"}}, "", 0)
		if err == nil || calls != 3 || len(g.checkpoints) != 0 {
			t.Fatalf("non-length violation triggered subdivision: calls=%d checkpoints=%d err=%v", calls, len(g.checkpoints), err)
		}
	}
}

func TestRepeatedOverlongKnowledgeRepairSplitsOriginalUTF8Evidence(t *testing.T) {
	source := Source{ID: "source", Content: strings.Repeat("界", 3000) + "complete condition and exception"}
	var fragments []Source
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
		calls++
		if strings.Contains(prompt, "\nTARGETS: ") {
			return `{"texts":[["` + strings.Repeat("x", 2750) + `"]]}`, nil
		}
		requested := requestedSources(t, prompt)
		if len(requested) != 1 {
			t.Fatal("unexpected source inventory")
		}
		item := requested[0]
		text := strings.Repeat("x", 2200)
		if item.ID != source.ID {
			fragments = append(fragments, item)
			text = "Scoped verified constraint from " + item.ID
		}
		raw, err := json.Marshal(Draft{Decisions: []Decision{{item.ID, "distill", "verified fragment"}}, Knowledge: []Knowledge{{text, []string{item.ID}}}})
		return string(raw), err
	}}
	draft, err := newDraftRunner(g, 5).generate(t.Context(), []Source{source}, "preserve complete evidence", 0)
	if err != nil || calls != 5 || len(fragments) != 2 || len(draft.Knowledge) != 2 {
		t.Fatalf("calls=%d fragments=%v draft=%v err=%v", calls, fragments, draft, err)
	}
	if fragments[0].ID != "source/0" || fragments[1].ID != "source/1" || fragments[0].Content+fragments[1].Content != source.Content {
		t.Fatal("subdivision changed or truncated the frozen UTF-8 evidence")
	}
	for _, knowledge := range draft.Knowledge {
		if !reflect.DeepEqual(knowledge.SourceIDs, []string{source.ID}) || strings.Contains(knowledge.Text, strings.Repeat("x", 2200)) {
			t.Fatal("rejected replacement became accepted evidence")
		}
	}
	if err := Validate(draft, []Source{source}); err != nil {
		t.Fatal(err)
	}
}

func TestOverlongProtocolCandidatePreservesPrivacyAndMergeStageGuards(t *testing.T) {
	for _, scenario := range []struct{ name, suffix, action, code string }{
		{"secret", " AKIAAAAAAAAAAAAAAAAA", "retain", "secret_knowledge"},
		{"redacted", " [REDACTED]", "retain", "redacted_knowledge"},
		{"discard", "", "discard", "merge_discard"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			raw, err := json.Marshal(Draft{
				Decisions: []Decision{{"a", "distill", "verified"}, {"b", scenario.action, "frozen decision"}},
				Knowledge: []Knowledge{{strings.Repeat("x", 2043) + scenario.suffix, []string{"a"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				if strings.Contains(prompt, "\nTARGETS: ") {
					t.Fatal("non-length violation gained text-rewrite authority")
				}
				return string(raw), nil
			}}
			runner := newDraftRunner(g, 8)
			runner.splitContent = false
			_, err = runner.generate(t.Context(), []Source{{ID: "a"}, {ID: "b"}}, "", 0)
			issue, ok := errors.AsType[*OutputViolation](err)
			if !ok || issue.Code != scenario.code || calls != 2 || len(g.checkpoints) != 0 {
				t.Fatalf("guard changed or protocol subdivision started: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWholeFactTextRepairKeepsPrivacyAndAbstentionTerminal(t *testing.T) {
	long := strings.Repeat("x", 2750)
	for _, scenario := range []struct{ name, raw, code string }{
		{"secret", `{"texts":["` + long + ` AKIAAAAAAAAAAAAAAAAA","supported second condition"]}`, "secret_knowledge"},
		{"redacted", `{"texts":[["` + long + ` [REDACTED]"],"supported second condition"]}`, "redacted_knowledge"},
		{"blank", `{"texts":["` + strings.Repeat(" ", 2750) + `","supported second condition"]}`, "blank_knowledge"},
		{"abstention", `{"texts":[null,"` + long + `"]}`, "knowledge_rewrite_abstained"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			sources := []Source{{ID: "a"}, {ID: "b"}}
			candidate := Draft{Decisions: []Decision{{"a", "distill", "verified"}, {"b", "distill", "verified"}}, Knowledge: []Knowledge{{strings.Repeat("a", 2200), []string{"a"}}, {strings.Repeat("b", 2200), []string{"b"}}}}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				if strings.Contains(prompt, "\nTARGETS: ") {
					return scenario.raw, nil
				}
				return string(raw), nil
			}}
			runner := newDraftRunner(g, 12)
			runner.splitContent = false
			_, err = runner.generate(t.Context(), sources, "", 0)
			issue, ok := errors.AsType[*OutputViolation](err)
			if !ok || issue.Code != scenario.code || calls != 3 || len(g.checkpoints) != 0 {
				t.Fatalf("semantic repair rejection became protocol subdivision: calls=%d err=%v", calls, err)
			}
		})
	}
}
