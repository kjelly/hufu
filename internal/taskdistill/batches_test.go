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

func TestPrepareBatchesPreservesEscapedUnicodeEvidence(t *testing.T) {
	content := strings.Repeat("審查\n\"\\\x00", MaxInputBytes/10)
	sources := []Source{{ID: "trash", Content: content}}
	batches, err := PrepareBatches(sources, "policy", DefaultMaxModelCalls)
	if err != nil || len(batches) < 2 {
		t.Fatalf("batches=%d err=%v", len(batches), err)
	}
	var rebuilt strings.Builder
	for _, batch := range batches {
		if inputSize(batch, "policy") > MaxInputBytes {
			t.Fatal("encoded batch exceeds input budget")
		}
		for _, fragment := range batch {
			rebuilt.WriteString(fragment.Content)
		}
	}
	if rebuilt.String() != content {
		t.Fatal("evidence was lost or altered at fragment boundaries")
	}
}

func TestGenerateBatchedKeepsKnowledgeBeyondPerCallItemLimit(t *testing.T) {
	sources := []Source{{ID: "source", Content: strings.Repeat("e", 12*(MaxInputBytes/2))}}
	calls, extracted := 0, 0
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		calls++
		requested := requestedSources(t, prompt)
		draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
		merge := strings.HasPrefix(requested[0].ID, "fact_")
		for _, source := range requested {
			action := "retain"
			if !merge {
				action = "distill"
				for i := range 100 {
					draft.Knowledge = append(draft.Knowledge, Knowledge{fmt.Sprintf("Verified fact %d/%d.", extracted, i), []string{source.ID}})
				}
				extracted++
			}
			draft.Decisions = append(draft.Decisions, Decision{source.ID, action, "preserve the selected facts"})
		}
		raw, err := json.Marshal(draft)
		if len(raw) > MaxOutputBytes {
			t.Fatal("fixture exceeded the unchanged per-call output bound")
		}
		return string(raw), err
	}
	draft, err := GenerateBatched(t.Context(), g, sources, "preserve all facts", 100)
	if err != nil || len(draft.Knowledge) != 1200 || Validate(draft, sources) != nil {
		t.Fatalf("aggregate lost approved facts: count=%d err=%v", len(draft.Knowledge), err)
	}
	for _, knowledge := range draft.Knowledge {
		if !slices.Equal(knowledge.SourceIDs, []string{"source"}) {
			t.Fatal("aggregate lost original source provenance")
		}
	}
	raw, err := json.Marshal(draft)
	if err != nil || len(raw) > MaxDraftBytes {
		t.Fatalf("canonical byte bound: bytes=%d err=%v", len(raw), err)
	}
	if _, err := decodeModelDraft(string(raw), sources); !errors.Is(err, errModelContract) {
		t.Fatal("single model reply gained the aggregate item capacity")
	}
	before := calls
	replayed, err := GenerateBatched(t.Context(), g, sources, "preserve all facts", 1)
	if err != nil || calls != before || Digest(replayed) != Digest(draft) {
		t.Fatalf("aggregate replay resent or lost completed work: calls=%d err=%v", calls-before, err)
	}
}

func TestGenerateBatchedAggregatesProvenanceAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"merge", "retain fragment", "protected", "late failure", "discard during merge"} {
		t.Run(mode, func(t *testing.T) {
			sources := []Source{{ID: "old", Content: strings.Repeat("verified evidence ", MaxInputBytes/8), Protected: mode == "protected"}, {ID: "other", Content: "another constraint"}}
			calls, extracted := 0, 0
			generator := generatorFunc(func(_ context.Context, prompt string) (string, error) {
				calls++
				_, data, ok := strings.Cut(prompt, "\nDATA: ")
				if !ok {
					t.Fatal("missing structured input")
				}
				var input struct {
					Sources []Source `json:"sources"`
					Policy  string   `json:"policy"`
				}
				if err := json.Unmarshal([]byte(data), &input); err != nil {
					t.Fatal(err)
				}
				if len(data) > MaxInputBytes {
					t.Fatal("unbounded model input")
				}
				merge := strings.HasPrefix(input.Sources[0].ID, "fact_")
				if mode == "late failure" && calls == 2 {
					return "", errors.New("provider failed")
				}
				draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
				var ids []string
				for _, source := range input.Sources {
					action := "distill"
					if source.Protected || mode == "retain fragment" && extracted == 0 && !merge {
						action = "retain"
					}
					if merge && mode == "discard during merge" {
						action = "discard"
					}
					draft.Decisions = append(draft.Decisions, Decision{source.ID, action, "evidence"})
					if action == "distill" {
						ids = append(ids, source.ID)
					}
				}
				if len(ids) > 0 {
					text := "Merged verified rule with its constraint."
					if !merge {
						// Distinct wording forces a semantic consolidation call.
						text = fmt.Sprintf("Verified rule %d with its constraint.", extracted)
					}
					draft.Knowledge = append(draft.Knowledge, Knowledge{text, ids})
				}
				if !merge {
					extracted++
				}
				raw, err := json.Marshal(draft)
				return string(raw), err
			})
			result, err := GenerateBatched(t.Context(), generator, sources, "keep constraints", DefaultMaxModelCalls)
			if mode == "late failure" || mode == "discard during merge" {
				if err == nil || len(result.Decisions) != 0 {
					t.Fatalf("failed batch returned an applicable proposal: %+v %v", result, err)
				}
				return
			}
			if err != nil || calls < 2 || Validate(result, sources) != nil {
				t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
			}
			if mode == "merge" && (len(result.Knowledge) != 1 || len(result.Knowledge[0].SourceIDs) != 2) {
				t.Fatalf("lost merged provenance: %+v", result)
			}
			if mode == "protected" && result.Decisions[0].Action != "retain" {
				t.Fatal("protected source allowed cleanup")
			}
			if mode == "retain fragment" && (result.Decisions[0].Action != "extract_only" || !slices.Contains(result.Knowledge[0].SourceIDs, "old")) {
				t.Fatal("approved knowledge from retained source fragments was lost")
			}
		})
	}
}

func TestGenerateBatchedPreflightsTotalBudget(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bytes, calls int
	}{
		{"total bytes", MaxSourceBytes + 1, DefaultMaxModelCalls},
		{"call budget", MaxInputBytes + 1, 1},
		{"invalid budget", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := GenerateBatched(t.Context(), generatorFunc(func(context.Context, string) (string, error) { calls++; return "", nil }), []Source{{ID: "large", Content: strings.Repeat("x", tc.bytes)}}, "", tc.calls)
			if !errors.Is(err, ErrInvalid) || calls != 0 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestGenerateBatchedBudgetsOnlyValidatedUncachedBatches(t *testing.T) {
	for _, mode := range []string{"complete", "partial", "missing", "changed policy", "changed source", "corrupt", "split with draft", "split hint", "load failure"} {
		t.Run(mode, func(t *testing.T) {
			sources := []Source{{ID: "old", Content: strings.Repeat("x", MaxInputBytes*2)}}
			batches, err := PrepareBatches(sources, "policy", MaxModelCalls)
			if err != nil || len(batches) < 3 {
				t.Fatalf("fixture: batches=%d err=%v", len(batches), err)
			}
			calls := 0
			g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}, generatorFunc: func(_ context.Context, prompt string) (string, error) {
				calls++
				draft := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
				for _, source := range requestedSources(t, prompt) {
					draft.Decisions = append(draft.Decisions, Decision{source.ID, "discard", "transient evidence"})
				}
				raw, err := json.Marshal(draft)
				return string(raw), err
			}}
			seed := len(batches)
			if mode == "partial" {
				seed = 1
			} else if mode == "missing" {
				seed = 0
			}
			for batch := range seed {
				if _, err := ProbeBatch(t.Context(), g, sources, "policy", batch+1, 1); err != nil {
					t.Fatal(err)
				}
			}
			policy, budget, wantCalls := "policy", 1, 0
			want := error(ErrInvalid)
			switch mode {
			case "complete":
				want = nil
			case "partial":
				budget, wantCalls, want = len(batches), len(batches)-1, nil
			case "changed policy":
				policy = "different policy"
			case "changed source":
				sources[0].Content = "changed " + sources[0].Content
			case "load failure":
				g.loadErr = errors.New("checkpoint read failed")
				want = g.loadErr
			case "corrupt", "split with draft", "split hint":
				for key, checkpoint := range g.checkpoints {
					if mode == "corrupt" {
						checkpoint.Draft = []byte(`{}`)
					} else {
						checkpoint.Split = true
						if mode == "split hint" {
							checkpoint.Draft = nil
						}
					}
					g.checkpoints[key] = checkpoint
					break
				}
			}
			before, _ := json.Marshal(g.checkpoints, json.Deterministic(true))
			calls = 0
			draft, err := GenerateBatched(t.Context(), g, sources, policy, budget)
			if calls != wantCalls || want == nil && err != nil || want != nil && !errors.Is(err, want) {
				t.Fatalf("calls=%d want=%d err=%v", calls, wantCalls, err)
			}
			if want == nil {
				if err := Validate(draft, sources); err != nil || len(draft.Decisions) != 1 {
					t.Fatalf("invalid resumed proposal: %v", err)
				}
			} else {
				after, _ := json.Marshal(g.checkpoints, json.Deterministic(true))
				if len(draft.Decisions) != 0 || string(before) != string(after) {
					t.Fatal("budget preflight changed checkpoints or accepted a partial proposal")
				}
			}
		})
	}
}
