package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolCallRecord represents a single tool call with context
type ToolCallRecord struct {
	Timestamp time.Time
	Agent     string
	Tool      string
	Input     string
	TaskDesc  string
}

// ToolSequence represents a sequence of tool calls
type ToolSequence struct {
	Tools      []string
	Params     []string // normalized parameter patterns
	Hash       string
	Count      int
	FirstSeen  time.Time
	LastSeen   time.Time
	TaskDescs  []string // task descriptions where this sequence was used
	Agent      string
}

// PatternCandidate represents a detected repeating pattern
type PatternCandidate struct {
	Sequence       *ToolSequence
	SimilarityScore float64 // 0.0-1.0 from semantic analysis
	SuggestedName   string
	SuggestedDesc   string
}

// SkillPatternDetector detects repeating tool call patterns
type SkillPatternDetector struct {
	mu                sync.RWMutex
	toolCalls         []ToolCallRecord
	sequences         map[string]*ToolSequence // hash -> sequence
	sequenceByAgent   map[string][]string      // agent -> sequence hashes
	minFrequency      int
	windowMin         int
	windowMax         int
	sidecarEnabled    bool
}

// NewSkillPatternDetector creates a new pattern detector
func NewSkillPatternDetector(minFrequency, windowMin, windowMax int) *SkillPatternDetector {
	return &SkillPatternDetector{
		sequences:       make(map[string]*ToolSequence),
		sequenceByAgent: make(map[string][]string),
		minFrequency:    minFrequency,
		windowMin:       windowMin,
		windowMax:       windowMax,
		sidecarEnabled:  false,
	}
}

// EnableSidecar enables semantic similarity analysis
func (d *SkillPatternDetector) EnableSidecar() {
	d.sidecarEnabled = true
}

// RecordToolCall records a tool call for pattern analysis
func (d *SkillPatternDetector) RecordToolCall(agent, tool, input, taskDesc string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	record := ToolCallRecord{
		Timestamp: time.Now(),
		Agent:     agent,
		Tool:      tool,
		Input:     input,
		TaskDesc:  taskDesc,
	}

	d.toolCalls = append(d.toolCalls, record)
	d.analyzeSequencesLocked()
}

// analyzeSequencesLocked analyzes tool calls for repeating sequences
// Must be called with lock held
func (d *SkillPatternDetector) analyzeSequencesLocked() {
	if len(d.toolCalls) < d.windowMin {
		return
	}

	// Analyze per agent
	agentCalls := make(map[string][]ToolCallRecord)
	for _, call := range d.toolCalls {
		agentCalls[call.Agent] = append(agentCalls[call.Agent], call)
	}

	for agent, calls := range agentCalls {
		d.extractSequencesForAgent(agent, calls)
	}
}

// extractSequencesForAgent extracts sequences from an agent's tool calls
func (d *SkillPatternDetector) extractSequencesForAgent(agent string, calls []ToolCallRecord) {
	if len(calls) < d.windowMin {
		return
	}

	// Sliding window extraction
	for windowSize := d.windowMin; windowSize <= d.windowMax && windowSize <= len(calls); windowSize++ {
		for i := 0; i <= len(calls)-windowSize; i++ {
			window := calls[i : i+windowSize]
			seq := d.buildSequence(window, agent)
			
			if _, exists := d.sequences[seq.Hash]; !exists {
				d.sequences[seq.Hash] = seq
				d.sequenceByAgent[agent] = append(d.sequenceByAgent[agent], seq.Hash)
			} else {
				// Update existing sequence
				existing := d.sequences[seq.Hash]
				existing.Count++
				existing.LastSeen = window[len(window)-1].Timestamp
				existing.TaskDescs = append(existing.TaskDescs, window[len(window)-1].TaskDesc)
			}
		}
	}
}

// buildSequence builds a ToolSequence from a window of tool calls
func (d *SkillPatternDetector) buildSequence(calls []ToolCallRecord, agent string) *ToolSequence {
	tools := make([]string, len(calls))
	params := make([]string, len(calls))

	for i, call := range calls {
		tools[i] = call.Tool
		params[i] = d.normalizeParams(call.Tool, call.Input)
	}

	seq := &ToolSequence{
		Tools:     tools,
		Params:    params,
		Count:     1,
		FirstSeen: calls[0].Timestamp,
		LastSeen:  calls[len(calls)-1].Timestamp,
		TaskDescs: []string{calls[len(calls)-1].TaskDesc},
		Agent:     agent,
	}
	seq.Hash = d.hashSequence(seq)

	return seq
}

// normalizeParams normalizes tool input to a pattern
func (d *SkillPatternDetector) normalizeParams(tool, input string) string {
	// Replace specific values with placeholders
	result := input

	// File paths with extensions
	reFile := regexp.MustCompile(`[\w./-]+\.(go|ts|js|py|md|yaml|yml|json|txt)`)
	result = reFile.ReplaceAllString(result, "*.$1")

	// Numbers
	reNum := regexp.MustCompile(`\b\d+\b`)
	result = reNum.ReplaceAllString(result, "<num>")

	// URLs
	reURL := regexp.MustCompile(`https?://[\w./-]+`)
	result = reURL.ReplaceAllString(result, "<url>")

	// Hashes (git, etc)
	reHash := regexp.MustCompile(`\b[a-f0-9]{7,40}\b`)
	result = reHash.ReplaceAllString(result, "<hash>")

	// Quoted strings
	reQuote := regexp.MustCompile(`["'][^"']+["']`)
	result = reQuote.ReplaceAllString(result, "<str>")

	return result
}

// hashSequence creates a unique hash for a sequence
func (d *SkillPatternDetector) hashSequence(seq *ToolSequence) string {
	data := strings.Join(seq.Tools, "|") + "||" + strings.Join(seq.Params, "|")
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// FindCandidates returns patterns that repeat at least minFrequency times
func (d *SkillPatternDetector) FindCandidates() []PatternCandidate {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var candidates []PatternCandidate

	for _, seq := range d.sequences {
		if seq.Count >= d.minFrequency {
			candidate := PatternCandidate{
				Sequence:        seq,
				SimilarityScore: 1.0, // Will be refined by sidecar
				SuggestedName:   d.generateSuggestedName(seq),
				SuggestedDesc:   d.generateSuggestedDescription(seq),
			}
			candidates = append(candidates, candidate)
		}
	}

	// Sort by frequency (descending)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Sequence.Count > candidates[j].Sequence.Count
	})

	return candidates
}

// generateSuggestedName creates a name from the tool sequence
func (d *SkillPatternDetector) generateSuggestedName(seq *ToolSequence) string {
	// Use first 3 tools for name
	tools := seq.Tools
	if len(tools) > 3 {
		tools = tools[:3]
	}

	name := strings.Join(tools, "-")
	name = strings.ReplaceAll(name, "_", "-")

	// Truncate if too long
	if len(name) > 50 {
		name = name[:50]
	}

	return "draft-" + name
}

// generateSuggestedDescription creates a description from task descriptions
func (d *SkillPatternDetector) generateSuggestedDescription(seq *ToolSequence) string {
	if len(seq.TaskDescs) == 0 {
		return "Auto-generated skill from repeated tool usage pattern"
	}

	// Use most common task description
	descFreq := make(map[string]int)
	for _, desc := range seq.TaskDescs {
		if desc != "" {
			descFreq[strings.ToLower(strings.TrimSpace(desc))]++
		}
	}

	// Find most frequent
	maxCount := 0
	mostCommon := ""
	for desc, count := range descFreq {
		if count > maxCount {
			maxCount = count
			mostCommon = desc
		}
	}

	if mostCommon != "" {
		return fmt.Sprintf("Use when %s (detected from %d similar executions)", mostCommon, seq.Count)
	}

	return fmt.Sprintf("Auto-generated skill from %d executions of: %s",
		seq.Count, strings.Join(seq.Tools, " → "))
}

// GetSequenceCount returns the number of detected sequences
func (d *SkillPatternDetector) GetSequenceCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.sequences)
}

// GetToolCallCount returns the total number of recorded tool calls
func (d *SkillPatternDetector) GetToolCallCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.toolCalls)
}

// Clear clears all recorded data
func (d *SkillPatternDetector) Clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.toolCalls = nil
	d.sequences = make(map[string]*ToolSequence)
	d.sequenceByAgent = make(map[string][]string)
}

// GetSequencesByAgent returns sequences for a specific agent
func (d *SkillPatternDetector) GetSequencesByAgent(agent string) []*ToolSequence {
	d.mu.RLock()
	defer d.mu.RUnlock()

	hashes := d.sequenceByAgent[agent]
	var sequences []*ToolSequence

	for _, hash := range hashes {
		if seq, exists := d.sequences[hash]; exists {
			sequences = append(sequences, seq)
		}
	}

	return sequences
}

// GetAllSequences returns all detected sequences
func (d *SkillPatternDetector) GetAllSequences() []*ToolSequence {
	d.mu.RLock()
	defer d.mu.RUnlock()

	sequences := make([]*ToolSequence, 0, len(d.sequences))
	for _, seq := range d.sequences {
		sequences = append(sequences, seq)
	}

	return sequences
}
