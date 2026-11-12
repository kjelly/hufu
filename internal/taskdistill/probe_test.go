package taskdistill

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProbeReusesExactlyTheNormalExtractionRequestWithoutWholeRunBudget(t *testing.T) {
	sources := []Source{{ID: "old", Content: strings.Repeat("e", MaxInputBytes*2)}}
	calls := []string{}
	g := &checkpointGenerator{checkpoints: map[string]Checkpoint{}}
	g.generatorFunc = func(_ context.Context, prompt string) (string, error) {
		requested := requestedSources(t, prompt)
		calls = append(calls, requested[0].ID)
		return discardedReply(t, requested), nil
	}
	if _, err := GenerateBatched(t.Context(), g, sources, "", 1); err == nil || len(calls) != 0 {
		t.Fatal("normal preview bypassed full-run preflight")
	}
	if draft, err := ProbeBatch(t.Context(), g, sources, "", 2, 1); err != nil || len(calls) != 1 || draft.Decisions[0].SourceID != "fragment_0_1" {
		t.Fatalf("probe: %+v %v %v", draft, calls, err)
	}
	calls = nil
	if _, err := GenerateBatched(t.Context(), g, sources, "", 8); err != nil || len(calls) != 3 {
		t.Fatalf("probe checkpoint not reused: %v %v", calls, err)
	}
	for _, id := range calls {
		if id == "fragment_0_1" {
			t.Fatal("probed batch repeated")
		}
	}
	calls = nil
	for _, batch := range []int{0, -1, 100} {
		if _, err := ProbeBatch(t.Context(), g, sources, "", batch, 1); !errors.Is(err, ErrInvalid) || len(calls) != 0 {
			t.Fatalf("bad selector called model: %d %v", batch, err)
		}
	}
}
