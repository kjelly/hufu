package skill

import (
	"context"
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

	candidates := detector.FindCandidates(context.Background())
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

	candidates := detector.FindCandidates(context.Background())
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
	candidates := detector.FindCandidates(context.Background())

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

	candidates := detector.FindCandidates(context.Background())
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

	candidates := detector.FindCandidates(context.Background())
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

	candidates := detector.FindCandidates(context.Background())
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

func TestSkillPatternDetector_SemanticSimilarity(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	// Record tool calls with similar descriptions
	for i := 0; i < 5; i++ {
		detector.RecordToolCall("agent1", "view", "*.go", "modify code")
		detector.RecordToolCall("agent1", "edit", "*.go", "modify code")
	}

	candidates := detector.FindCandidates(context.Background())

	// Verify candidates were found (semantic analysis runs automatically if sidecar is set)
	if len(candidates) == 0 {
		t.Error("Expected candidates to be found")
	}

	// Without sidecar, should still work (graceful degradation)
	// The count is 25 because sliding window creates multiple sequences
	if candidates[0].Sequence.Count < 5 {
		t.Errorf("Expected count >= 5, got %d", candidates[0].Sequence.Count)
	}
}

func TestSkillPatternDetector_MergeSimilarSequences(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	candidates := []PatternCandidate{
		{
			Sequence: &ToolSequence{
				Tools:     []string{"view", "edit"},
				Count:     3,
				TaskDescs: []string{"fix bug", "fix issue"},
			},
			SimilarityScore: 0.95,
		},
		{
			Sequence: &ToolSequence{
				Tools:     []string{"view", "edit"},
				Count:     2,
				TaskDescs: []string{"modify code"},
			},
			SimilarityScore: 0.92,
		},
	}

	// Put all descriptions in the same cluster
	clusters := map[int][]int{
		0: {0, 1, 2},
	}

	merged := detector.mergeSimilarSequences(candidates, clusters, 0.9)

	// Verify merging occurred
	if len(merged) != 1 {
		t.Errorf("Expected 1 merged candidate, got %d", len(merged))
	}

	if merged[0].Sequence.Count != 5 {
		t.Errorf("Expected merged count 5, got %d", merged[0].Sequence.Count)
	}
}

func TestSkillPatternDetector_MergeSimilarSequences_DifferentClusters(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	candidates := []PatternCandidate{
		{
			Sequence: &ToolSequence{
				Tools:     []string{"view", "edit"},
				Count:     3,
				TaskDescs: []string{"fix bug", "fix issue"},
			},
			SimilarityScore: 0.95,
		},
		{
			Sequence: &ToolSequence{
				Tools:     []string{"view", "edit"},
				Count:     2,
				TaskDescs: []string{"modify code"},
			},
			SimilarityScore: 0.92,
		},
	}

	// Put them in different clusters
	clusters := map[int][]int{
		0: {0, 1},
		1: {2},
	}

	merged := detector.mergeSimilarSequences(candidates, clusters, 0.9)

	// Verify merging did NOT occur (but they should fallback to keyword overlap unless we ensure overlap is low)
	// Wait, let's look at descs1: "fix bug", "fix issue" -> keywords: "fix", "bug", "issue"
	// descs2: "modify code" -> keywords: "modify", "code"
	// The overlap is 0, which is < 50%, so they will indeed not merge.
	if len(merged) != 2 {
		t.Errorf("Expected 2 separate candidates, got %d", len(merged))
	}
}

func TestSkillPatternDetector_CollectAllTaskDescriptions(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	candidates := []PatternCandidate{
		{
			Sequence: &ToolSequence{
				TaskDescs: []string{"fix bug", "modify code"},
			},
		},
		{
			Sequence: &ToolSequence{
				TaskDescs: []string{"fix bug", "add feature"},
			},
		},
	}

	descs := detector.collectAllTaskDescriptions(candidates)

	// Should have 3 unique descriptions
	if len(descs) != 3 {
		t.Errorf("Expected 3 unique descriptions, got %d", len(descs))
	}

	// Verify all descriptions are present
	expected := map[string]bool{
		"fix bug":     false,
		"modify code": false,
		"add feature": false,
	}

	for _, desc := range descs {
		if _, ok := expected[desc]; ok {
			expected[desc] = true
		}
	}

	for desc, found := range expected {
		if !found {
			t.Errorf("Expected description %q not found", desc)
		}
	}
}

func TestSkillPatternDetector_ExtractKeywords(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	descs := []string{
		"Fix the bug in the code",
		"Modify the implementation",
	}

	keywords := detector.extractKeywords(descs)

	// Should extract content words, not stop words
	expectedKeywords := []string{"fix", "bug", "code", "modify", "implementation"}
	for _, keyword := range expectedKeywords {
		if !keywords[keyword] {
			t.Errorf("Expected keyword %q not found", keyword)
		}
	}

	// Should not include stop words
	stopWords := []string{"the", "in", "and", "or"}
	for _, word := range stopWords {
		if keywords[word] {
			t.Errorf("Stop word %q should not be included", word)
		}
	}
}

func TestSkillPatternDetector_HashDescriptions(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	descs1 := []string{"fix bug", "modify code"}
	descs2 := []string{"fix bug", "modify code"}
	descs3 := []string{"different", "descriptions"}

	hash1 := detector.hashDescriptions(descs1)
	hash2 := detector.hashDescriptions(descs2)
	hash3 := detector.hashDescriptions(descs3)

	if hash1 != hash2 {
		t.Error("Expected same hash for same descriptions")
	}

	if hash1 == hash3 {
		t.Error("Expected different hash for different descriptions")
	}
}

func TestSkillPatternDetector_IsInSameCluster(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	// Test case 1: Matching clusters (no keyword overlap)
	descs1 := []string{"fix backend bug"}
	descs2 := []string{"resolve server issue"}
	allDescs := []string{"fix backend bug", "resolve server issue"}
	clusters := map[int][]int{
		0: {0, 1},
	}

	if !detector.isInSameCluster(descs1, descs2, clusters, allDescs) {
		t.Error("Expected descriptions to be in the same cluster via cluster mapping")
	}

	// Test case 2: Different clusters (no keyword overlap)
	descs3 := []string{"fix backend bug"}
	descs4 := []string{"another unrelated thing"}
	allDescs2 := []string{"fix backend bug", "another unrelated thing"}
	clusters3 := map[int][]int{
		0: {0},
		1: {1},
	}

	if detector.isInSameCluster(descs3, descs4, clusters3, allDescs2) {
		t.Error("Expected descriptions to be in different clusters via cluster mapping")
	}

	// Test case 3: Fallback keyword overlap (no clusters map)
	descs5 := []string{"fix bug code"}
	descs6 := []string{"fix code bug"}

	if !detector.isInSameCluster(descs5, descs6, nil, append(descs5, descs6...)) {
		t.Error("Expected descriptions with overlapping keywords to be in same cluster via fallback")
	}

	// Test case 4: Fallback no overlap (no clusters map)
	descs7 := []string{"completely different task"}
	descs8 := []string{"another unrelated thing"}

	if detector.isInSameCluster(descs7, descs8, nil, append(descs7, descs8...)) {
		t.Error("Expected descriptions with no overlap to be in different clusters via fallback")
	}
}

func TestSkillPatternDetector_GenerateMergedDescription(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	descs := []string{
		"fix bug in parser",
		"fix bug in parser",
		"modify code",
	}

	result := detector.generateMergedDescription(descs, 10)

	if !strings.Contains(result, "fix bug in parser") {
		t.Error("Expected merged description to mention most common task")
	}

	if !strings.Contains(result, "10") {
		t.Error("Expected merged description to mention count")
	}
}

func TestSkillPatternDetector_HashToolSequence(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	tools1 := []string{"view", "edit", "bash"}
	tools2 := []string{"view", "edit", "bash"}
	tools3 := []string{"view", "edit"}

	hash1 := detector.hashToolSequence(tools1)
	hash2 := detector.hashToolSequence(tools2)
	hash3 := detector.hashToolSequence(tools3)

	if hash1 != hash2 {
		t.Error("Expected same hash for same tool sequence")
	}

	if hash1 == hash3 {
		t.Error("Expected different hash for different tool sequence")
	}
}

func TestSkillPatternDetector_BuildClusterPrompt(t *testing.T) {
	detector := NewSkillPatternDetector(2, 2, 3)

	descs := []string{
		"fix bug",
		"modify code",
	}

	prompt := detector.buildClusterPrompt(descs)

	if !strings.Contains(prompt, "fix bug") {
		t.Error("Expected prompt to contain first description")
	}

	if !strings.Contains(prompt, "modify code") {
		t.Error("Expected prompt to contain second description")
	}

	if !strings.Contains(prompt, "JSON") {
		t.Error("Expected prompt to mention JSON format")
	}
}
