package skill

import (
	"testing"
)

func TestHasActionTool(t *testing.T) {
	d := NewSkillPatternDetector(10, 2, 5)

	tests := []struct {
		name     string
		tools    []string
		expected bool
	}{
		// Pure read-only sequences should NOT have action tools
		{"read-only view+grep", []string{"view", "grep"}, false},
		{"read-only view+glob+ls", []string{"view", "glob", "ls"}, false},
		{"read-only grep+view+ls", []string{"grep", "view", "ls"}, false},
		{"read-only ask_user+view", []string{"ask_user", "view"}, false},

		// Sequences with mutating tools should have action tools
		{"write tool", []string{"view", "write"}, true},
		{"edit tool", []string{"view", "edit"}, true},
		{"multiedit tool", []string{"grep", "multiedit"}, true},
		{"bash tool", []string{"view", "bash"}, true},
		{"sudo tool", []string{"view", "sudo"}, true},
		{"ssh tool", []string{"view", "ssh"}, true},
		{"lua tool", []string{"view", "lua"}, true},
		{"golang tool", []string{"view", "golang"}, true},
		{"download tool", []string{"view", "download"}, true},

		// Mixed
		{"view+edit+bash", []string{"view", "edit", "bash"}, true},
		{"glob+grep+view+edit", []string{"glob", "grep", "view", "edit"}, true},

		// Empty
		{"empty", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := &ToolSequence{Tools: tt.tools}
			got := d.hasActionTool(seq)
			if got != tt.expected {
				t.Errorf("hasActionTool(%v) = %v, want %v", tt.tools, got, tt.expected)
			}
		})
	}
}

func TestDynamicMinFrequency(t *testing.T) {
	d := NewSkillPatternDetector(10, 3, 10) // base = 10

	tests := []struct {
		name     string
		seqLen   int
		expected int
	}{
		// Short sequences (len <= 4) get 2x multiplier
		{"len=3 (short)", 3, 20},
		{"len=4 (short)", 4, 20},

		// Medium sequences (len=5) use baseline
		{"len=5 (medium)", 5, 10},

		// Long sequences (len >= 6) get 60% reduction
		{"len=6 (long)", 6, 6},   // 10 * 3/5 = 6
		{"len=8 (long)", 8, 6},   // same formula
		{"len=10 (long)", 10, 6}, // same formula
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.dynamicMinFrequency(tt.seqLen)
			if got != tt.expected {
				t.Errorf("dynamicMinFrequency(%d) = %d, want %d", tt.seqLen, got, tt.expected)
			}
		})
	}
}

func TestDynamicMinFrequency_SmallBase(t *testing.T) {
	// Ensure the minimum floor of 3 for long sequences
	d := NewSkillPatternDetector(4, 3, 10) // base = 4

	// 4 * 3/5 = 2 but floor is 3
	got := d.dynamicMinFrequency(6)
	if got != 3 {
		t.Errorf("dynamicMinFrequency(6) with base=4 = %d, want 3 (floor)", got)
	}
}

func TestIsContiguousSubsequence(t *testing.T) {
	tests := []struct {
		name     string
		a        []string
		b        []string
		expected bool
	}{
		// Strict prefix (also a contiguous subsequence)
		{"prefix", []string{"view", "edit"}, []string{"view", "edit", "bash"}, true},
		// Suffix
		{"suffix", []string{"edit", "bash"}, []string{"view", "edit", "bash"}, true},
		// Middle
		{"middle", []string{"edit"}, []string{"view", "edit", "bash"}, true},
		// Not a subsequence
		{"not subseq", []string{"view", "bash"}, []string{"view", "edit", "bash"}, false},
		// Same length (not strict)
		{"same length", []string{"view", "edit"}, []string{"view", "edit"}, false},
		// Longer a
		{"a longer", []string{"view", "edit", "bash"}, []string{"view", "edit"}, false},
		// Empty a
		{"empty a", []string{}, []string{"view", "edit"}, true},
		// Single element
		{"single match", []string{"edit"}, []string{"view", "edit", "bash"}, true},
		{"single no match", []string{"write"}, []string{"view", "edit", "bash"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isContiguousSubsequence(tt.a, tt.b)
			if got != tt.expected {
				t.Errorf("isContiguousSubsequence(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

func TestDedupPrefixes_SubsequenceDedup(t *testing.T) {
	// [edit, bash] is a suffix of [view, edit, bash] — should be deduped
	cands := []PatternCandidate{
		{Sequence: &ToolSequence{Tools: []string{"view", "edit", "bash"}}},
		{Sequence: &ToolSequence{Tools: []string{"edit", "bash"}}}, // suffix — should be removed
	}

	got := dedupPrefixes(cands)
	if len(got) != 1 {
		t.Errorf("dedupPrefixes returned %d candidates, want 1", len(got))
	}
	if len(got) > 0 && len(got[0].Sequence.Tools) != 3 {
		t.Errorf("kept candidate has %d tools, want 3", len(got[0].Sequence.Tools))
	}
}

func TestDedupPrefixes_MiddleSubsequence(t *testing.T) {
	// [edit] is a middle sub-sequence of [view, edit, bash] — should be deduped
	cands := []PatternCandidate{
		{Sequence: &ToolSequence{Tools: []string{"view", "edit", "bash"}}},
		{Sequence: &ToolSequence{Tools: []string{"edit"}}},          // middle — should be removed
		{Sequence: &ToolSequence{Tools: []string{"grep", "write"}}}, // unrelated — kept
	}

	got := dedupPrefixes(cands)
	if len(got) != 2 {
		t.Errorf("dedupPrefixes returned %d candidates, want 2", len(got))
	}
}

func TestMinParamScoreConstant(t *testing.T) {
	if minParamScore != 0.5 {
		t.Errorf("minParamScore = %.2f, want 0.5", minParamScore)
	}
}
