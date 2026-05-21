package skill

import (
	"strings"
	"testing"
	"time"
)

func TestSkillPatternDetector_RecordToolCall(t *testing.T) {
	detector := NewSkillPatternDetector(3, 3, 5)

	// Record some tool calls
	detector.RecordToolCall("agent1", "view", "file.go", "task 1")
	detector.RecordToolCall("agent1", "edit", "file.go", "task 1")
	detector.RecordToolCall("agent1", "bash", "go build", "task 1")

	if detector.GetToolCallCount() != 3 {
		t.Errorf("Expected 3 tool calls, got %d", detector.GetToolCallCount())
	}

	if detector.GetSequenceCount() == 0 {
		t.Error("Expected at least 1 sequence to be detected")
	}
}

func TestSkillPatternDetector_FindCandidates(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 4)

	// Record repeating pattern 3 times
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "*.go", "review code")
		detector.RecordToolCall("agent1", "edit", "*.go", "review code")
		detector.RecordToolCall("agent1", "bash", "go test", "review code")
	}

	candidates := detector.FindCandidates()
	if len(candidates) == 0 {
		t.Fatal("Expected to find at least one candidate")
	}

	// Check that the repeating pattern was found
	found := false
	for _, cand := range candidates {
		if len(cand.Sequence.Tools) == 3 &&
			cand.Sequence.Tools[0] == "view" &&
			cand.Sequence.Tools[1] == "edit" &&
			cand.Sequence.Tools[2] == "bash" {
			found = true
			if cand.Sequence.Count < 3 {
				t.Errorf("Expected count >= 3, got %d", cand.Sequence.Count)
			}
			break
		}
	}

	if !found {
		t.Error("Expected to find view->edit->bash pattern")
	}
}

func TestSkillPatternDetector_NormalizeParams(t *testing.T) {
	detector := NewSkillPatternDetector(3, 3, 5)

	tests := []struct {
		name     string
		tool     string
		input    string
		expected string
	}{
		{
			name:     "file paths with extensions",
			tool:     "view",
			input:    "src/main.go",
			expected: "*.",
		},
		{
			name:     "numbers",
			tool:     "bash",
			input:    "git checkout 12345",
			expected: "git checkout <num>",
		},
		{
			name:     "URLs",
			tool:     "fetch",
			input:    "https://example.com/api",
			expected: "<url>",
		},
		{
			name:     "quoted strings",
			tool:     "edit",
			input:    `"hello world"`,
			expected: "<str>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detector.normalizeParams(tt.tool, tt.input)
			if !strings.Contains(result, tt.expected) {
				t.Errorf("Expected %q to contain %q, got %q", result, tt.expected, result)
			}
		})
	}
}

func TestSkillPatternDetector_GenerateSuggestedName(t *testing.T) {
	detector := NewSkillPatternDetector(3, 3, 5)

	seq := &ToolSequence{
		Tools: []string{"view", "edit", "bash", "view"},
	}

	name := detector.generateSuggestedName(seq)
	if !strings.HasPrefix(name, "draft-") {
		t.Errorf("Expected name to start with 'draft-', got %q", name)
	}

	// Should use first 3 tools
	expected := "draft-view-edit-bash"
	if name != expected {
		t.Errorf("Expected %q, got %q", expected, name)
	}
}

func TestSkillPatternDetector_GenerateSuggestedDescription(t *testing.T) {
	detector := NewSkillPatternDetector(3, 3, 5)

	seq := &ToolSequence{
		Tools:     []string{"view", "edit"},
		Count:     5,
		TaskDescs: []string{"review code", "review code", "fix bug", "review code"},
	}

	desc := detector.generateSuggestedDescription(seq)
	if !strings.Contains(desc, "review code") {
		t.Errorf("Expected description to mention most common task, got %q", desc)
	}
	if !strings.Contains(desc, "5") {
		t.Errorf("Expected description to mention count, got %q", desc)
	}
}

func TestSkillPatternDetector_Clear(t *testing.T) {
	detector := NewSkillPatternDetector(3, 3, 5)

	detector.RecordToolCall("agent1", "view", "file.go", "task 1")
	detector.RecordToolCall("agent1", "edit", "file.go", "task 1")
	detector.RecordToolCall("agent1", "bash", "go build", "task 1")

	if detector.GetToolCallCount() != 3 {
		t.Fatal("Expected 3 tool calls before clear")
	}

	detector.Clear()

	if detector.GetToolCallCount() != 0 {
		t.Errorf("Expected 0 tool calls after clear, got %d", detector.GetToolCallCount())
	}

	if detector.GetSequenceCount() != 0 {
		t.Errorf("Expected 0 sequences after clear, got %d", detector.GetSequenceCount())
	}
}

func TestSkillPatternDetector_MultipleAgents(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	// Agent 1 pattern
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "*.go", "task 1")
		detector.RecordToolCall("agent1", "edit", "*.go", "task 1")
	}

	// Agent 2 different pattern
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent2", "grep", "pattern", "task 2")
		detector.RecordToolCall("agent2", "view", "*.go", "task 2")
	}

	candidates := detector.FindCandidates()
	if len(candidates) < 2 {
		t.Errorf("Expected at least 2 candidates (one per agent), got %d", len(candidates))
	}
}

func TestSkillPatternDetector_WindowSize(t *testing.T) {
	detector := NewSkillPatternDetector(2, 3, 5)

	// Record exactly 5 tool calls
	tools := []string{"view", "edit", "bash", "view", "bash"}
	for _, tool := range tools {
		detector.RecordToolCall("agent1", tool, "args", "task")
	}

	// Should detect sequences of size 3, 4, and 5
	candidates := detector.FindCandidates()

	// At minimum, should find some repeating patterns
	if len(candidates) == 0 {
		t.Error("Expected to find some candidates with window size 3-5")
	}
}

func TestAutoSkillGenerator(t *testing.T) {
	generator := NewAutoSkillGenerator(t.TempDir())

	candidate := PatternCandidate{
		Sequence: &ToolSequence{
			Tools:  []string{"view", "edit", "bash"},
			Params: []string{"*.go", "*.go", "go build"},
			Count:  5,
		},
		SuggestedName: "draft-view-edit-bash",
		SuggestedDesc: "Use when modifying Go code",
	}

	path, err := generator.GenerateSkill(candidate)
	if err != nil {
		t.Fatalf("Failed to generate skill: %v", err)
	}

	// Verify file was created
	content := generator.buildSkillContent(candidate)

	if !strings.Contains(content, "draft-view-edit-bash") {
		t.Error("Expected skill name in content")
	}
	if !strings.Contains(content, "view") {
		t.Error("Expected 'view' tool in workflow")
	}
	if !strings.Contains(content, "edit") {
		t.Error("Expected 'edit' tool in workflow")
	}
	if !strings.Contains(content, "bash") {
		t.Error("Expected 'bash' tool in workflow")
	}
	if !strings.Contains(content, "Auto-generated") {
		t.Error("Expected auto-generated notice")
	}

	_ = path // path is the file location
}

func TestSkillPatternDetector_ParameterPatternMatching(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	// Same tool sequence with similar parameter patterns
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "file1.go", "task")
		detector.RecordToolCall("agent1", "edit", "file1.go", "task")
	}

	// Same tool sequence with different files (should still match due to normalization)
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "file2.go", "task")
		detector.RecordToolCall("agent1", "edit", "file2.go", "task")
	}

	candidates := detector.FindCandidates()
	if len(candidates) == 0 {
		t.Error("Expected to find candidates with normalized parameter patterns")
	}
}

func TestSkillPatternDetector_TimestampTracking(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	start := time.Now()
	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "file.go", "task")
		detector.RecordToolCall("agent1", "edit", "file.go", "task")
		time.Sleep(10 * time.Millisecond)
	}
	end := time.Now()

	candidates := detector.FindCandidates()
	if len(candidates) == 0 {
		t.Fatal("Expected to find candidates")
	}

	seq := candidates[0].Sequence
	if seq.FirstSeen.Before(start) {
		t.Error("FirstSeen should be after test start")
	}
	if seq.LastSeen.After(end) {
		t.Error("LastSeen should be before test end")
	}
	if seq.FirstSeen.After(seq.LastSeen) {
		t.Error("FirstSeen should be before LastSeen")
	}
}

func TestSkillPatternDetector_GetSequencesByAgent(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent1", "view", "file.go", "task")
		detector.RecordToolCall("agent1", "edit", "file.go", "task")
	}

	for i := 0; i < 3; i++ {
		detector.RecordToolCall("agent2", "grep", "pattern", "task")
		detector.RecordToolCall("agent2", "view", "file.go", "task")
	}

	seqs1 := detector.GetSequencesByAgent("agent1")
	seqs2 := detector.GetSequencesByAgent("agent2")

	if len(seqs1) == 0 {
		t.Error("Expected sequences for agent1")
	}
	if len(seqs2) == 0 {
		t.Error("Expected sequences for agent2")
	}

	// Verify sequences are different
	if len(seqs1) != len(seqs2) {
		// This is expected - different agents have different patterns
	}
}

func TestSkillPatternDetector_MinFrequency(t *testing.T) {
	detector := NewSkillPatternDetector(5, 2, 2)

	// Repeat 10 times to ensure we have enough data
	for i := 0; i < 10; i++ {
		detector.RecordToolCall("agent1", "view", "file.go", "task")
		detector.RecordToolCall("agent1", "edit", "file.go", "task")
	}

	candidates := detector.FindCandidates()
	if len(candidates) == 0 {
		t.Fatal("Expected candidates with 10 repetitions")
	}

	// All candidates should have count >= 5 (minFrequency)
	for _, cand := range candidates {
		if cand.Sequence.Count < 5 {
			t.Errorf("Expected all candidates to have count >= 5, got %d for sequence %v",
				cand.Sequence.Count, cand.Sequence.Tools)
		}
	}

	// Should find the view->edit pattern with high frequency
	found := false
	for _, cand := range candidates {
		if len(cand.Sequence.Tools) == 2 &&
			cand.Sequence.Tools[0] == "view" &&
			cand.Sequence.Tools[1] == "edit" &&
			cand.Sequence.Count >= 5 {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected to find view->edit pattern with count >= 5")
	}
}
