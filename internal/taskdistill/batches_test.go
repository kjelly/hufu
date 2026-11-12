package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
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
			if (mode == "retain fragment" || mode == "protected") && result.Decisions[0].Action != "retain" {
				t.Fatal("retained fragment allowed whole-source cleanup")
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
