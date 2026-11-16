package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func refinementFixture(n int) (Draft, []Source) {
	sources := []Source{{ID: "old-a", Content: "Frozen verification evidence A."}, {ID: "old-b", Content: "Frozen verification evidence B."}}
	parent := Draft{Decisions: []Decision{{"old-a", "distill", "useful"}, {"old-b", "distill", "useful"}}, Knowledge: []Knowledge{}}
	for i := range n {
		parent.Knowledge = append(parent.Knowledge, Knowledge{Text: fmt.Sprintf("Reusable rule %d; applies only to verified receipts.", i), SourceIDs: []string{sources[i%2].ID}})
	}
	return parent, sources
}

func refinementReply(t *testing.T, sources []Source, filter bool) string {
	t.Helper()
	draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
	var ids []string
	for _, source := range sources {
		action := "distill"
		if filter && source.ID == "fact_0" {
			action = "discard"
		}
		if filter && source.ID == "fact_1" {
			action = "retain"
		}
		draft.Decisions = append(draft.Decisions, Decision{source.ID, action, "bounded semantic judgment"})
		if action == "distill" {
			ids = append(ids, source.ID)
		}
	}
	if len(ids) > 0 {
		draft.Knowledge = append(draft.Knowledge, Knowledge{Text: "Require verified receipts; preserve scope and uncertainty.", SourceIDs: ids})
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRefinementFiltersMergesAndBindsOriginalProvenance(t *testing.T) {
	parent, sources := refinementFixture(4)
	before := Digest(parent)
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		calls++
		return refinementReply(t, requestedSources(t, prompt), strings.Contains(prompt, "Plan refinement v1")), nil
	}
	evidence, draft, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 5)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(draft.Knowledge) != 1 || len(evidence.Batches) != 1 || before != Digest(parent) {
		t.Fatalf("calls=%d draft=%+v evidence=%+v", calls, draft, evidence)
	}
	if !slices.Equal(draft.Knowledge[0].SourceIDs, []string{"old-a", "old-b"}) || draft.Decisions[0].Action != ActionExtractOnly || draft.Decisions[1].Action != "distill" {
		t.Fatalf("lost provenance or expanded cleanup authority: %+v", draft)
	}
	if !slices.Equal(evidence.Knowledge[0].SourceIDs, []string{"fact_1", "fact_2", "fact_3"}) {
		t.Fatalf("parent-fact lineage=%v", evidence.Knowledge)
	}
	calls = 0
	replayedEvidence, replayed, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 1)
	if err != nil || calls != 0 || Digest(replayed) != Digest(draft) || Digest(replayedEvidence) != Digest(evidence) {
		t.Fatalf("checkpoint replay calls=%d err=%v", calls, err)
	}
}

func TestRefinementPreservesProgressAcrossInterruptions(t *testing.T) {
	for _, failure := range []string{"transport", "cancel", "budget", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			parent, sources := refinementFixture(33)
			calls := 0
			fail := true
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
				calls++
				if calls == 2 && fail {
					switch failure {
					case "transport":
						return "", NewTransportError(errors.New("offline transport failure"))
					case "cancel":
						cancel()
						return "", ctx.Err()
					case "malformed":
						return `{"decisions":[`, nil
					}
				}
				return refinementReply(t, requestedSources(t, prompt), strings.Contains(prompt, "Plan refinement v1")), nil
			}
			budget := 2
			if failure == "budget" {
				budget = 1
			}
			_, partial, err := Refine(ctx, g, parent, sources, DefaultRefinementPolicy, budget)
			if err == nil || len(partial.Decisions) != 0 || calls > budget || len(g.checkpoints) == 0 {
				t.Fatalf("partial plan or lost progress: calls=%d err=%v", calls, err)
			}
			if failure == "budget" && !errors.Is(err, ErrCallBudget) {
				t.Fatal(err)
			}
			fail = false
			calls = 0
			_, complete, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 4)
			if err != nil || calls != 2 || Validate(complete, sources) != nil {
				t.Fatalf("resume replayed accepted first batch: calls=%d err=%v", calls, err)
			}
			calls = 0
			_, _, err = Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 1)
			if err != nil || calls != 0 {
				t.Fatalf("complete replay: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRefinementRejectsForgedReferencesAndMissingCoverage(t *testing.T) {
	parent, sources := refinementFixture(4)
	g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
		return refinementReply(t, requestedSources(t, prompt), strings.Contains(prompt, "Plan refinement v1")), nil
	})
	evidence, _, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"foreign", "discarded", "duplicate", "missing", "inventory", "runtime action", "privacy"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(evidence)
			var changed Refinement
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign":
				changed.Knowledge[0].SourceIDs = append(changed.Knowledge[0].SourceIDs, "old-a")
			case "discarded":
				changed.Knowledge[0].SourceIDs = append(changed.Knowledge[0].SourceIDs, "fact_0")
			case "duplicate":
				changed.Knowledge[0].SourceIDs = append(changed.Knowledge[0].SourceIDs, "fact_1")
			case "missing":
				changed.Knowledge[0].SourceIDs = []string{"fact_1"}
			case "inventory":
				changed.Batches = nil
			case "runtime action":
				changed.Batches[0].Decisions[2].Action = ActionExtractOnly
			case "privacy":
				changed.Knowledge[0].Text = "Invented credential [REDACTED]"
			}
			if _, err := RebuildRefinement(parent, sources, DefaultRefinementPolicy, changed); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted invalid %s: %v", mode, err)
			}
		})
	}
}

func TestRefinementEmptySelectionRetainsSourcesAndNeverSplitsFacts(t *testing.T) {
	parent, sources := refinementFixture(2)
	g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
		return discardedReply(t, requestedSources(t, prompt)), nil
	})
	_, draft, err := Refine(t.Context(), g, parent, sources, "Discard all transient reports.", 1)
	if err != nil || len(draft.Knowledge) != 0 || draft.Decisions[0].Action != "retain" || draft.Decisions[1].Action != "retain" {
		t.Fatalf("empty selection permits cleanup: %+v %v", draft, err)
	}
	parent, sources = refinementFixture(1)
	parent.Decisions[1].Action = "retain"
	parent.Knowledge[0].Text = strings.Repeat("條", 1500)
	calls := 0
	g = func(context.Context, string) (string, error) {
		calls++
		return "", ErrIncompleteOutput
	}
	_, _, err = Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 8)
	if !errors.Is(err, ErrIncompleteOutput) || calls != 1 {
		t.Fatalf("whole fact subdivided: calls=%d err=%v", calls, err)
	}
}

func TestRefinementRepairsInvalidCitationsWithinSharedBudget(t *testing.T) {
	for _, budget := range []int{1, 2} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			parent, sources := refinementFixture(2)
			calls := 0
			g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
				calls++
				valid := refinementReply(t, requestedSources(t, prompt), false)
				if calls == 1 {
					return strings.Replace(valid, `"source_ids":["fact_0","fact_1"]`, `"source_ids":["foreign"]`, 1), nil
				}
				return valid, nil
			})
			_, draft, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, budget)
			if calls != budget {
				t.Fatalf("calls=%d budget=%d", calls, budget)
			}
			if budget == 1 {
				if !errors.Is(err, ErrCallBudget) || len(draft.Decisions) != 0 {
					t.Fatalf("repair exceeded budget or produced partial draft: %v", err)
				}
			} else if err != nil || len(draft.Knowledge) != 1 || !slices.Equal(draft.Knowledge[0].SourceIDs, []string{"old-a", "old-b"}) {
				t.Fatalf("repair lost original provenance: %+v %v", draft, err)
			}
		})
	}
}

func TestRefinementLargeParentReplaysAllBatches(t *testing.T) {
	parent, sources := refinementFixture(1200)
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		calls++
		return refinementReply(t, requestedSources(t, prompt), false), nil
	}
	evidence, draft, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 64)
	if err != nil || len(evidence.Batches) != 38 || len(draft.Knowledge) != 1 || len(evidence.Knowledge[0].SourceIDs) != 1200 {
		t.Fatalf("large parent lost facts: calls=%d err=%v", calls, err)
	}
	calls = 0
	_, replayed, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 1)
	if err != nil || calls != 0 || Digest(replayed) != Digest(draft) {
		t.Fatalf("large parent replay calls=%d err=%v", calls, err)
	}
	changed := parent
	changed.Knowledge = slices.Clone(parent.Knowledge)
	changed.Knowledge[0].Text += " Updated applicability."
	if _, _, err := Refine(t.Context(), g, changed, sources, DefaultRefinementPolicy, 64); err != nil || calls == 0 {
		t.Fatalf("changed parent reused old filtering judgments: calls=%d err=%v", calls, err)
	}
}

func TestRefinementReferenceSubdivisionPreservesWholeFactsAndResumes(t *testing.T) {
	for _, fault := range []string{"unknown_citation", "citation_not_distilled", "uncovered_distill_source", "invalid_json", "schema_violation"} {
		for _, interruption := range []string{"transport", "cancel", "budget"} {
			t.Run(fault+"/"+interruption, func(t *testing.T) {
				parent, sources := refinementFixture(4)
				before := Digest(parent)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stopped := NewTransportError(errors.New("fixture interrupted"))
				failRight, calls := true, 0
				g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
				g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
					calls++
					requested := requestedSources(t, prompt)
					if !strings.Contains(prompt, "Plan refinement v1") {
						return refinementReply(t, requested, false), nil
					}
					for _, source := range requested {
						if !slices.ContainsFunc(parent.Knowledge, func(item Knowledge) bool {
							raw, _ := json.Marshal(item)
							return source.Content == string(raw)
						}) {
							t.Fatal("refinement fragmented or changed a parent fact")
						}
					}
					if len(requested) == 2 && requested[0].ID == "fact_2" && failRight {
						if interruption == "cancel" {
							cancel()
							return "", ctx.Err()
						}
						return "", stopped
					}
					return mergeReferenceReply(t, requested, fault, len(requested) > 2), nil
				}
				budget, wantCalls, wantErr := 8, 5, error(stopped)
				if interruption == "budget" {
					budget, wantCalls, wantErr = 3, 3, ErrCallBudget
				} else if interruption == "cancel" {
					wantCalls, wantErr = 4, context.Canceled
				}
				_, partial, err := Refine(ctx, g, parent, sources, DefaultRefinementPolicy, budget)
				if !errors.Is(err, wantErr) || len(partial.Decisions) != 0 || calls != wantCalls {
					t.Fatalf("partial=%v calls=%d err=%v", partial, calls, err)
				}
				failRight, calls = false, 0
				evidence, complete, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 2)
				if err != nil || calls != 2 || Validate(complete, sources) != nil || Digest(parent) != before || len(complete.Knowledge) != 1 {
					t.Fatalf("resume calls=%d draft=%v err=%v", calls, complete, err)
				}
				if !slices.Equal(evidence.Knowledge[0].SourceIDs, []string{"fact_0", "fact_1", "fact_2", "fact_3"}) ||
					!slices.Equal(complete.Knowledge[0].SourceIDs, []string{"old-a", "old-b"}) {
					t.Fatal("refinement lost fact coverage or original provenance")
				}
				calls = 0
				_, replay, err := Refine(t.Context(), g, parent, sources, DefaultRefinementPolicy, 1)
				if err != nil || calls != 0 || Digest(replay) != Digest(complete) {
					t.Fatalf("replay calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
