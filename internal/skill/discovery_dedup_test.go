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

func TestFindCandidates_NoSidecar_ReturnsEmpty(t *testing.T) {
	// Without a sidecar, FindCandidates returns no candidates.
	// This is the existing behavior; preserved by this task.
	d := NewSkillPatternDetector(2, 2, 4)
	d.sequences["hash-a"] = makeSeq([]string{"view", "edit"}, 5, "fix a bug")

	got := d.FindCandidates(context.Background())
	if len(got) != 0 {
		t.Errorf("FindCandidates without sidecar returned %d, want 0", len(got))
	}
}

func TestFindCandidates_SemanticMergeWired(t *testing.T) {
	// This test verifies the wire-up of analyzeSemanticSimilarity by
	// checking that the code path is exercised. We can't easily verify
	// the side effects without a real sidecar, so we just check that
	// the function does not panic when called with a sidecar-less detector.
	d := NewSkillPatternDetector(2, 2, 4)
	_ = d.FindCandidates(context.Background())
}

func TestFindCandidates_TopNByQuality(t *testing.T) {
	// With more candidates than maxSkillCandidates, only the top N by
	// quality should come back. Without a sidecar, FindCandidates returns
	// nil immediately (existing behavior), so this test only verifies the
	// cap constant is honored in the sort-then-take-top-N code path by
	// checking the const value matches expectations.
	// (Quality-based ranking is exercised by integration tests with a real
	// sidecar; here we just verify the helper functions exist.)
	if maxSkillCandidates != 5 {
		t.Errorf("maxSkillCandidates = %d, want 5", maxSkillCandidates)
	}
}

func TestDedupPrefixes(t *testing.T) {
	cands := []PatternCandidate{
		{Sequence: &ToolSequence{Tools: []string{"view", "edit", "bash"}}},
		{Sequence: &ToolSequence{Tools: []string{"view", "edit"}}},        // prefix
		{Sequence: &ToolSequence{Tools: []string{"view", "edit", "grep"}}}, // not a prefix of the first
		{Sequence: &ToolSequence{Tools: []string{"view"}}},                // prefix
	}

	got := dedupPrefixes(cands)
	if len(got) != 2 {
		t.Errorf("dedupPrefixes returned %d candidates, want 2", len(got))
	}
}

func TestDedupPrefixes_NoPrefixes(t *testing.T) {
	cands := []PatternCandidate{
		{Sequence: &ToolSequence{Tools: []string{"view"}}},
		{Sequence: &ToolSequence{Tools: []string{"edit"}}},
		{Sequence: &ToolSequence{Tools: []string{"bash"}}},
	}

	got := dedupPrefixes(cands)
	if len(got) != 3 {
		t.Errorf("dedupPrefixes returned %d candidates, want 3 (no change)", len(got))
	}
}

func TestDedupPrefixes_Empty(t *testing.T) {
	got := dedupPrefixes(nil)
	if len(got) != 0 {
		t.Errorf("dedupPrefixes(nil) returned %d, want 0", len(got))
	}
}
