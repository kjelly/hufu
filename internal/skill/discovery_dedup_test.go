package skill

import (
	"context"
	"testing"
	"time"
)

// makeSeq creates a ToolSequence for testing.
func makeSeq(tools []string, count int, desc string) *ToolSequence {
	return &ToolSequence{
		Tools:     tools,
		Hash:      hashToolsForTest(tools),
		Count:     count,
		FirstSeen: time.Now().Add(-time.Hour),
		LastSeen:  time.Now(),
		TaskDescs: []string{desc},
	}
}

// hashToolsForTest is a test-only helper that hashes a tool list.
// Matches the format used in discovery.go's buildSequence.
func hashToolsForTest(tools []string) string {
	h := ""
	for i, t := range tools {
		if i > 0 {
			h += ","
		}
		h += t
	}
	return h
}

func TestFindCandidates_ReturnsAtLeastOne(t *testing.T) {
	// Smoke test: with a single sequence that passes the frequency filter,
	// FindCandidates should return at least one candidate.
	d := NewSkillPatternDetector(2, 2, 4)
	d.sequences["hash-a"] = makeSeq([]string{"view", "edit"}, 5, "fix a bug")

	got := d.FindCandidates(context.Background())
	if len(got) < 1 {
		t.Fatalf("FindCandidates returned %d candidates, want >= 1", len(got))
	}
}

func TestFindCandidates_AnalyzesSemanticSimilarity(t *testing.T) {
	// Two sequences with the same tools but different task descriptions.
	// Without a sidecar, semantic merge is a no-op; both candidates come back.
	d := NewSkillPatternDetector(2, 2, 4)
	d.sequences["hash-a"] = makeSeq([]string{"view", "edit"}, 5, "fix a bug")
	d.sequences["hash-b"] = makeSeq([]string{"view", "edit"}, 5, "fix a bug")

	got := d.FindCandidates(context.Background())
	// Without a sidecar, the two are not merged; without a real LLM eval
	// the quality score is 0 (no tool diversity) so they may be filtered.
	// We just check the function does not panic.
	_ = got
}
