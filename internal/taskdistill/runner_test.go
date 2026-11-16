package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type checkpointGenerator struct {
	generatorFunc
	checkpoints      map[string]Checkpoint
	loadErr, saveErr error
}

func (g *checkpointGenerator) LoadDraftCheckpoint(_ context.Context, key string) (Checkpoint, bool, error) {
	c, ok := g.checkpoints[key]
	return c, ok, g.loadErr
}

func (g *checkpointGenerator) SaveDraftCheckpoint(_ context.Context, key string, checkpoint Checkpoint) error {
	if g.saveErr != nil {
		return g.saveErr
	}
	g.checkpoints[key] = checkpoint
	return nil
}

func requestedSources(t *testing.T, prompt string) []Source {
	t.Helper()
	_, raw, found := strings.Cut(prompt, "\nDATA: ")
	if !found {
		t.Fatal("missing input envelope")
	}
	var request struct {
		Sources []Source `json:"sources"`
	}
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	return request.Sources
}

func discardedReply(t *testing.T, sources []Source) string {
	t.Helper()
	draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
	for _, source := range sources {
		draft.Decisions = append(draft.Decisions, Decision{source.ID, "discard", "transient"})
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestIncompleteDraftSplittingRetainsCompletedCheckpointsAcrossRuns(t *testing.T) {
	sources := []Source{{ID: "one", Content: strings.Repeat("證據", 1500)}}
	calls := []string{}
	failRight := true
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		requested := requestedSources(t, prompt)
		id := requested[0].ID
		calls = append(calls, id)
		if id == "one" {
			return `{"decisions":[`, nil
		}
		if id == "one/1" && failRight {
			return "", errors.New("provider unavailable")
		}
		return discardedReply(t, requested), nil
	}
	if _, err := GenerateBatched(t.Context(), g, sources, "", 3); err == nil {
		t.Fatal("partial draft accepted")
	}
	if strings.Join(calls, ",") != "one,one/0,one/1" {
		t.Fatalf("calls=%v", calls)
	}
	failRight = false
	calls = nil
	draft, err := GenerateBatched(t.Context(), g, sources, "", 3)
	if err != nil || Validate(draft, sources) != nil || strings.Join(calls, ",") != "one/1" {
		t.Fatalf("draft=%+v calls=%v err=%v", draft, calls, err)
	}
	calls = nil
	if _, err = GenerateBatched(t.Context(), g, sources, "", 3); err != nil || len(calls) != 0 {
		t.Fatalf("completed checkpoint resent: calls=%v err=%v", calls, err)
	}
}

func TestIncompleteDraftBudgetAndCheckpointFailuresFailClosed(t *testing.T) {
	for _, mode := range []string{"call budget", "too small", "bad references", "load failure", "save failure"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			source := Source{ID: "one", Content: strings.Repeat("x", 4096)}
			if mode == "too small" {
				source.Content = "small"
			}
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
			g.generatorFunc = func(context.Context, string) (string, error) {
				calls++
				if mode == "bad references" {
					return `{"decisions":[{"source_id":"forged","action":"discard","reason":"invalid"}],"knowledge":[]}`, nil
				}
				return `{"decisions":[`, nil
			}
			if mode == "load failure" {
				g.loadErr = errors.New("read failed")
			}
			if mode == "save failure" {
				g.saveErr = errors.New("write failed")
			}
			draft, err := GenerateBatched(t.Context(), g, []Source{source}, "", 1)
			if err == nil || len(draft.Decisions) != 0 || calls > 1 {
				t.Fatalf("draft=%+v calls=%d err=%v", draft, calls, err)
			}
			if mode == "call budget" && !errors.Is(err, ErrCallBudget) {
				t.Fatalf("budget classification: %v", err)
			}
			if mode == "too small" && (!errors.Is(err, ErrIncompleteOutput) || !errors.Is(err, io.ErrUnexpectedEOF)) {
				t.Fatalf("EOF classification: %v", err)
			}
		})
	}
}

func TestIncompleteDraftSubdivisionIsBoundedAndKeepsSelectedFactsIntact(t *testing.T) {
	for _, merge := range []bool{false, true} {
		calls := 0
		generator := generatorFunc(func(context.Context, string) (string, error) {
			calls++
			return "", ErrIncompleteOutput
		})
		runner := newDraftRunner(generator, 32)
		var err error
		if merge {
			_, err = consolidateKnowledge(t.Context(), runner, []Knowledge{
				{Text: strings.Repeat("a", 4096), SourceIDs: []string{"first"}},
				{Text: strings.Repeat("b", 4096), SourceIDs: []string{"second"}},
			}, "preserve every selected fact")
		} else {
			_, err = runner.generate(t.Context(), []Source{{ID: "source", Content: strings.Repeat("x", MaxInputBytes/2)}}, "", 0)
		}
		wantCalls := 4 // Root and one child at each of the three allowed levels.
		if merge {
			wantCalls = 2 // Root group, then a complete indivisible fact.
		}
		if !errors.Is(err, ErrIncompleteOutput) || calls != wantCalls {
			t.Fatalf("merge=%v calls=%d want=%d err=%v", merge, calls, wantCalls, err)
		}
	}
}

func TestSplitDraftPreservesKnowledgeProvenanceAndRetention(t *testing.T) {
	sources := []Source{{ID: "one", Content: strings.Repeat("x", 4096)}}
	for _, retain := range []bool{false, true} {
		calls := 0
		g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
		g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
			calls++
			requested := requestedSources(t, prompt)
			if requested[0].ID == "one" {
				return "", ErrIncompleteOutput
			}
			action := "distill"
			if retain && strings.HasSuffix(requested[0].ID, "/1") {
				action = "retain"
			}
			draft := Draft{Decisions: []Decision{{requested[0].ID, action, "evidence"}}, Knowledge: []Knowledge{}}
			if action == "distill" {
				draft.Knowledge = append(draft.Knowledge, Knowledge{"Preserve the verified applicability constraint.", []string{requested[0].ID}})
			}
			raw, err := json.Marshal(draft)
			return string(raw), err
		}
		draft, err := GenerateBatched(t.Context(), g, sources, "", 3)
		if err != nil {
			t.Fatal(err)
		}
		if retain {
			if draft.Decisions[0].Action != "extract_only" || len(draft.Knowledge) != 1 {
				t.Fatalf("approved knowledge from a retained source was lost: %+v", draft)
			}
			prompt, _ := draftPrompt(sources, "")
			legacy, _ := draftCheckpointKeys(prompt, true)
			if checkpoint := g.checkpoints[legacy]; !checkpoint.Split || len(checkpoint.Draft) != 0 {
				t.Fatal("runtime metadata overwrote an older reader's legacy checkpoint")
			}
		} else if len(draft.Knowledge) != 1 || len(draft.Knowledge[0].SourceIDs) != 1 || draft.Knowledge[0].SourceIDs[0] != "one" {
			t.Fatalf("provenance lost: %+v", draft)
		}
		before := calls
		if replay, err := GenerateBatched(t.Context(), g, sources, "", 1); err != nil || calls != before || Digest(replay) != Digest(draft) {
			t.Fatalf("retained knowledge did not survive checkpoint replay: calls=%d err=%v", calls, err)
		}

	}
}

func TestMergeReferenceSubdivisionPreservesFactsAndResumes(t *testing.T) {
	items := []Knowledge{
		{Text: "First verified constraint.", SourceIDs: []string{"first"}},
		{Text: "Second conflicting constraint.", SourceIDs: []string{"second"}},
		{Text: "Third verified failure.", SourceIDs: []string{"third"}},
		{Text: "Fourth applicability limit.", SourceIDs: []string{"fourth"}},
	}
	for _, fault := range []string{"unknown_citation", "citation_not_distilled", "uncovered_distill_source", "invalid_json", "schema_violation"} {
		for _, interruption := range []string{"provider", "cancellation", "budget"} {
			t.Run(fault+"/"+interruption, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stopped := errors.New("fixture interrupted")
				failRight, calls := true, 0
				g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
				g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
					calls++
					sources := requestedSources(t, prompt)
					if len(sources) == 1 && sources[0].ID == "fact_2" && failRight {
						if interruption == "cancellation" {
							cancel()
							return "", context.Canceled
						}
						return "", stopped
					}
					return mergeReferenceReply(t, sources, fault, len(sources) > 1), nil
				}
				budget, wantCalls, wantErr := 16, 9, stopped
				if interruption == "budget" {
					budget, wantCalls, wantErr = 8, 8, ErrCallBudget
				} else if interruption == "cancellation" {
					wantErr = context.Canceled
				}
				result, err := consolidateKnowledge(ctx, newDraftRunner(g, budget), items, "keep every fact")
				if !errors.Is(err, wantErr) || len(result) != 0 || calls != wantCalls {
					t.Fatalf("partial result=%v calls=%d err=%v", result, calls, err)
				}
				failRight, calls = false, 0
				result, err = consolidateKnowledge(t.Context(), newDraftRunner(g, 2), items, "keep every fact")
				if err != nil || calls != 2 || Digest(result) != Digest(items) {
					t.Fatalf("resume lost facts/provenance or resent completed work: calls=%d err=%v result=%v", calls, err, result)
				}
				calls = 0
				result, err = consolidateKnowledge(t.Context(), newDraftRunner(g, 1), items, "keep every fact")
				if err != nil || calls != 0 || Digest(result) != Digest(items) {
					t.Fatalf("replay calls=%d err=%v result=%v", calls, err, result)
				}
			})
		}
	}
}

func mergeReferenceReply(t *testing.T, sources []Source, fault string, reject bool) string {
	t.Helper()
	if reject && fault == "invalid_json" {
		return "not a JSON response"
	}
	if reject && fault == "schema_violation" {
		return `{"decisions":[],"knowledge":[{"content":"wrong field","citations":[]}]}`
	}
	draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
	for _, source := range sources {
		action := "retain"
		if reject && fault != "citation_not_distilled" {
			action = "distill"
		}
		draft.Decisions = append(draft.Decisions, Decision{source.ID, action, "keep the verified evidence"})
	}
	if reject && fault != "uncovered_distill_source" {
		id := sources[0].ID
		if fault == "unknown_citation" {
			id = "unknown"
		}
		text := "Rejected candidate must never become accepted evidence."
		if fault == "redacted_knowledge" {
			text = "[REDACTED]"
		}
		draft.Knowledge = append(draft.Knowledge, Knowledge{text, []string{id}})
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMergeReferenceSubdivisionBoundsAndFailureClasses(t *testing.T) {
	for _, test := range []struct {
		name            string
		count, budget   int
		wantCalls       int
		extract, shape  bool
		privacy, filter bool
		malformed       bool
		wantErr         error
	}{
		{name: "depth", count: 16, budget: 32, wantCalls: 8, wantErr: ErrInvalid},
		{name: "indivisible fact", count: 1, budget: 32, wantCalls: 2, wantErr: ErrInvalid},
		{name: "shared budget", count: 16, budget: 3, wantCalls: 3, wantErr: ErrCallBudget},
		{name: "extraction unchanged", count: 4, budget: 32, wantCalls: 2, extract: true, wantErr: ErrInvalid},
		{name: "schema failure", count: 4, budget: 32, wantCalls: 6, shape: true, wantErr: ErrInvalid},
		{name: "privacy failure", count: 4, budget: 32, wantCalls: 2, privacy: true, wantErr: ErrInvalid},
		{name: "filter depth", count: 16, budget: 32, wantCalls: 8, filter: true, wantErr: ErrInvalid},
		{name: "filter indivisible fact", count: 1, budget: 32, wantCalls: 2, filter: true, wantErr: ErrInvalid},
		{name: "filter shared budget", count: 16, budget: 3, wantCalls: 3, filter: true, wantErr: ErrCallBudget},
		{name: "filter schema failure", count: 4, budget: 32, wantCalls: 6, filter: true, shape: true, wantErr: ErrInvalid},
		{name: "filter privacy failure", count: 4, budget: 32, wantCalls: 2, filter: true, privacy: true, wantErr: ErrInvalid},
		{name: "malformed reply", count: 4, budget: 32, wantCalls: 6, malformed: true, wantErr: ErrInvalid},
		{name: "filter malformed reply", count: 4, budget: 32, wantCalls: 6, malformed: true, filter: true, wantErr: ErrInvalid},
		{name: "indivisible malformed fact", count: 1, budget: 32, wantCalls: 2, malformed: true, wantErr: ErrInvalid},
		{name: "indivisible schema fact", count: 1, budget: 32, wantCalls: 2, shape: true, wantErr: ErrInvalid},
		{name: "extraction schema unchanged", count: 4, budget: 32, wantCalls: 2, extract: true, shape: true, wantErr: ErrInvalid},
		{name: "extraction malformed unchanged", count: 4, budget: 32, wantCalls: 2, extract: true, malformed: true, wantErr: ErrInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sources []Source
			for i := range test.count {
				sources = append(sources, Source{ID: fmt.Sprintf("fact_%d", i), Content: "Complete selected fact."})
			}
			calls := 0
			g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
				calls++
				requested := requestedSources(t, prompt)
				for _, source := range requested {
					if source.Content != sources[0].Content || strings.Contains(source.ID, "/") {
						t.Fatal("selected fact text was fragmented")
					}
				}
				if test.malformed {
					return "not a JSON response", nil
				}
				if test.shape {
					return `{"decisions":[],"knowledge":[]}`, nil
				}
				fault := "uncovered_distill_source"
				if test.privacy {
					fault = "redacted_knowledge"
				}
				return mergeReferenceReply(t, requested, fault, true), nil
			})
			runner := newDraftRunner(g, test.budget)
			runner.splitContent = test.extract || test.filter
			runner.wholeSources = test.filter
			draft, err := runner.generate(t.Context(), sources, "keep selected facts", 0)
			if !errors.Is(err, test.wantErr) || len(draft.Decisions) != 0 || calls != test.wantCalls {
				t.Fatalf("draft=%v calls=%d err=%v", draft, calls, err)
			}
		})
	}
}

func TestLargeSplitAggregateCheckpointReplaysWithoutChangingLegacyRecords(t *testing.T) {
	sources := []Source{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	calls := 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		calls++
		requested := requestedSources(t, prompt)
		if len(requested) > 1 {
			return "", ErrIncompleteOutput
		}
		id := requested[0].ID
		draft := Draft{Decisions: []Decision{{id, "distill", "approved"}}, Knowledge: []Knowledge{}}
		for i := range 300 {
			draft.Knowledge = append(draft.Knowledge, Knowledge{fmt.Sprintf("%s%d", id, i), []string{id}})
		}
		raw, err := json.Marshal(draft)
		if len(raw) > MaxOutputBytes {
			t.Fatal("fixture exceeded the per-reply byte limit")
		}
		return string(raw), err
	}
	runner := newDraftRunner(g, 7)
	runner.splitContent = false
	draft, err := runner.generate(t.Context(), sources, "preserve every selected fact", 0)
	if err != nil || len(draft.Knowledge) != 1200 || calls != 7 {
		t.Fatalf("aggregate count=%d calls=%d err=%v", len(draft.Knowledge), calls, err)
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	for _, legacySchema := range []string{modelSchema, retainedCheckpointSchema} {
		if _, err := decodeCheckpointDraft(string(raw), sources, legacySchema); !errors.Is(err, ErrInvalid) {
			t.Fatal("legacy checkpoint contract gained aggregate item capacity")
		}
	}
	before, err := json.Marshal(g.checkpoints, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	runner = newDraftRunner(g, 1)
	runner.splitContent = false
	replayed, err := runner.generate(t.Context(), sources, "preserve every selected fact", 0)
	if err != nil || calls != 7 || Digest(replayed) != Digest(draft) {
		t.Fatalf("large aggregate replay calls=%d err=%v", calls, err)
	}
	after, err := json.Marshal(g.checkpoints, json.Deterministic(true))
	if err != nil || string(before) != string(after) {
		t.Fatal("checkpoint replay changed stored records")
	}
	prompt, _ := draftPrompt(sources, "preserve every selected fact")
	key, _ := draftCheckpointKeys(prompt, false)
	if !g.checkpoints[key].Split || len(g.checkpoints[key].Draft) != 0 {
		t.Fatal("new aggregate overwrote the immutable legacy split hint")
	}
}
