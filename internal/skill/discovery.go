package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/sidecar"
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
	sidecar           *sidecar.Sidecar // sidecar instance for semantic analysis
	ctx               context.Context  // context for sidecar calls
	clusterCache      map[string]map[int][]int // descriptions hash -> clusters
	cacheMu           sync.RWMutex
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
		clusterCache:    make(map[string]map[int][]int),
	}
}

// SetSidecar sets the sidecar instance and context for semantic analysis
func (d *SkillPatternDetector) SetSidecar(ctx context.Context, s *sidecar.Sidecar) {
	d.sidecar = s
	d.ctx = ctx
	d.sidecarEnabled = s != nil
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
				SimilarityScore: 1.0,
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

	// Apply semantic similarity analysis if sidecar is enabled
	if d.sidecarEnabled && d.sidecar != nil && len(candidates) > 0 {
		candidates = d.analyzeSemanticSimilarity(candidates)
	}

	return candidates
}

// analyzeSemanticSimilarity uses sidecar to analyze and merge similar sequences
func (d *SkillPatternDetector) analyzeSemanticSimilarity(candidates []PatternCandidate) []PatternCandidate {
	// Collect all task descriptions
	allDescs := d.collectAllTaskDescriptions(candidates)
	if len(allDescs) < 2 {
		return candidates
	}

	// Cluster descriptions using sidecar
	clusters := d.clusterDescriptions(allDescs)
	if clusters == nil {
		return candidates
	}

	// Merge sequences with similarity >= 0.9
	merged := d.mergeSimilarSequences(candidates, clusters, 0.9)

	return merged
}

// collectAllTaskDescriptions collects all unique task descriptions from candidates
func (d *SkillPatternDetector) collectAllTaskDescriptions(candidates []PatternCandidate) []string {
	seen := make(map[string]bool)
	var descs []string

	for _, cand := range candidates {
		for _, desc := range cand.Sequence.TaskDescs {
			if desc != "" && !seen[desc] {
				seen[desc] = true
				descs = append(descs, desc)
			}
		}
	}

	return descs
}

// clusterDescriptions uses sidecar to cluster similar task descriptions
func (d *SkillPatternDetector) clusterDescriptions(descriptions []string) map[int][]int {
	// Check cache first
	descHash := d.hashDescriptions(descriptions)
	
	d.cacheMu.RLock()
	if cached, ok := d.clusterCache[descHash]; ok {
		d.cacheMu.RUnlock()
		return cached
	}
	d.cacheMu.RUnlock()

	// Build prompt for sidecar
	prompt := d.buildClusterPrompt(descriptions)

	// Call sidecar with timeout
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()

	result, err := d.sidecar.Execute(ctx, prompt)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "warning: sidecar cluster timeout\n")
		} else {
			fmt.Fprintf(os.Stderr, "warning: sidecar cluster failed: %v\n", err)
		}
		return nil
	}

	// Parse JSON result
	var clusters map[int][]int
	if err := json.Unmarshal([]byte(result), &clusters); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to parse cluster JSON: %v\n", err)
		return nil
	}

	// Cache the result
	d.cacheMu.Lock()
	d.clusterCache[descHash] = clusters
	d.cacheMu.Unlock()

	return clusters
}

// hashDescriptions creates a hash of the descriptions list for caching
func (d *SkillPatternDetector) hashDescriptions(descriptions []string) string {
	data := strings.Join(descriptions, "|")
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// buildClusterPrompt builds the prompt for clustering task descriptions
func (d *SkillPatternDetector) buildClusterPrompt(descriptions []string) string {
	var descList strings.Builder
	for i, desc := range descriptions {
		descList.WriteString(fmt.Sprintf("%d. %s\n", i, desc))
	}

	return fmt.Sprintf(`You are a task similarity analyzer. Group task descriptions that describe essentially the same work.

Return a JSON object where:
- Keys are cluster IDs (integers starting from 0)
- Values are arrays of 0-based indices of descriptions in that cluster

Rules:
- Group descriptions that accomplish the same goal, even if worded differently
- Keep different workflows in separate clusters
- A description can only belong to one cluster

Task descriptions:
%s

Return ONLY the JSON object, no other text.
Example format: {"0": [0, 2, 5], "1": [1, 3, 4]}`, descList.String())
}

// mergeSimilarSequences merges sequences based on cluster analysis
func (d *SkillPatternDetector) mergeSimilarSequences(candidates []PatternCandidate, clusters map[int][]int, threshold float64) []PatternCandidate {
	allDescs := d.collectAllTaskDescriptions(candidates)

	// Group candidates by tool sequence
	groupMap := make(map[string][]PatternCandidate) // tool sequence hash -> candidates

	for _, cand := range candidates {
		toolHash := d.hashToolSequence(cand.Sequence.Tools)
		groupMap[toolHash] = append(groupMap[toolHash], cand)
	}

	// Merge groups based on semantic similarity
	var merged []PatternCandidate
	for _, group := range groupMap {
		var mergedGroup []PatternCandidate
		for _, cand := range group {
			placed := false
			for i := range mergedGroup {
				if d.isInSameCluster(cand.Sequence.TaskDescs, mergedGroup[i].Sequence.TaskDescs, clusters, allDescs) {
					mergedGroup[i] = d.mergeCandidateGroup([]PatternCandidate{mergedGroup[i], cand})
					placed = true
					break
				}
			}
			if !placed {
				mergedGroup = append(mergedGroup, cand)
			}
		}
		merged = append(merged, mergedGroup...)
	}

	return merged
}

// hashToolSequence creates a hash of the tool sequence
func (d *SkillPatternDetector) hashToolSequence(tools []string) string {
	data := strings.Join(tools, "|")
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:8])
}

// isInSameCluster checks if two sets of descriptions share a cluster
func (d *SkillPatternDetector) isInSameCluster(descs1, descs2 []string, clusters map[int][]int, allDescs []string) bool {
	// Build map from description string to index in allDescs
	descIndices := make(map[string]int)
	for i, desc := range allDescs {
		descIndices[strings.ToLower(strings.TrimSpace(desc))] = i
	}

	// Find clusters containing any description from descs1
	clusters1 := make(map[int]bool)
	for _, desc := range descs1 {
		trimmed := strings.ToLower(strings.TrimSpace(desc))
		if idx, ok := descIndices[trimmed]; ok {
			for clusterID, memberIndices := range clusters {
				for _, mIdx := range memberIndices {
					if mIdx == idx {
						clusters1[clusterID] = true
					}
				}
			}
		}
	}

	// Check if any description in descs2 is in those clusters
	for _, desc := range descs2 {
		trimmed := strings.ToLower(strings.TrimSpace(desc))
		if idx, ok := descIndices[trimmed]; ok {
			for clusterID, memberIndices := range clusters {
				for _, mIdx := range memberIndices {
					if mIdx == idx {
						if clusters1[clusterID] {
							return true
						}
					}
				}
			}
		}
	}

	// Fallback to keyword overlap if clusters map is empty or no cluster matched
	keywords1 := d.extractKeywords(descs1)
	keywords2 := d.extractKeywords(descs2)

	overlap := 0
	for word := range keywords1 {
		if keywords2[word] {
			overlap++
		}
	}

	total := len(keywords1) + len(keywords2) - overlap
	if total == 0 {
		return false
	}

	return float64(overlap)/float64(total) >= 0.5
}

// mergeCandidateGroup merges a group of similar candidates
func (d *SkillPatternDetector) mergeCandidateGroup(group []PatternCandidate) PatternCandidate {
	totalCount := 0
	allTaskDescs := []string{}
	var firstSeen, lastSeen time.Time

	for _, cand := range group {
		totalCount += cand.Sequence.Count
		allTaskDescs = append(allTaskDescs, cand.Sequence.TaskDescs...)

		if firstSeen.IsZero() || cand.Sequence.FirstSeen.Before(firstSeen) {
			firstSeen = cand.Sequence.FirstSeen
		}
		if lastSeen.IsZero() || cand.Sequence.LastSeen.After(lastSeen) {
			lastSeen = cand.Sequence.LastSeen
		}
	}

	// Use first candidate's sequence as representative
	representative := group[0]

	return PatternCandidate{
		Sequence: &ToolSequence{
			Tools:     representative.Sequence.Tools,
			Params:    representative.Sequence.Params,
			Hash:      representative.Sequence.Hash,
			Count:     totalCount,
			FirstSeen: firstSeen,
			LastSeen:  lastSeen,
			TaskDescs: allTaskDescs,
			Agent:     representative.Sequence.Agent,
		},
		SimilarityScore: 0.9,
		SuggestedName:   representative.SuggestedName,
		SuggestedDesc:   d.generateMergedDescription(allTaskDescs, totalCount),
	}
}

// generateMergedDescription generates a description for a merged candidate
func (d *SkillPatternDetector) generateMergedDescription(descs []string, count int) string {
	if len(descs) == 0 {
		return fmt.Sprintf("Auto-generated skill from %d executions", count)
	}

	descFreq := make(map[string]int)
	for _, desc := range descs {
		if desc != "" {
			descFreq[strings.ToLower(strings.TrimSpace(desc))]++
		}
	}

	maxCount := 0
	mostCommon := ""
	for desc, c := range descFreq {
		if c > maxCount {
			maxCount = c
			mostCommon = desc
		}
	}

	if mostCommon != "" {
		return fmt.Sprintf("Use when %s (detected from %d similar executions)", mostCommon, count)
	}

	return fmt.Sprintf("Auto-generated skill from %d executions", count)
}

// extractKeywords extracts keywords from descriptions
func (d *SkillPatternDetector) extractKeywords(descs []string) map[string]bool {
	keywords := make(map[string]bool)
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"to": true, "of": true, "in": true, "for": true, "on": true,
		"with": true, "and": true, "or": true, "but": true,
	}

	for _, desc := range descs {
		words := strings.Fields(strings.ToLower(desc))
		for _, word := range words {
			word = strings.Trim(word, ".,!?;:")
			if len(word) > 2 && !stopWords[word] {
				keywords[word] = true
			}
		}
	}

	return keywords
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
