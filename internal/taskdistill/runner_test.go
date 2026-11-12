package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
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
		g := generatorFunc(func(_ context.Context, prompt string) (string, error) {
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
		})
		draft, err := GenerateBatched(t.Context(), g, sources, "", 3)
		if err != nil {
			t.Fatal(err)
		}
		if retain {
			if draft.Decisions[0].Action != "retain" || len(draft.Knowledge) != 0 {
				t.Fatalf("retained split was removed: %+v", draft)
			}
		} else if len(draft.Knowledge) != 1 || len(draft.Knowledge[0].SourceIDs) != 1 || draft.Knowledge[0].SourceIDs[0] != "one" {
			t.Fatalf("provenance lost: %+v", draft)
		}
	}
}
