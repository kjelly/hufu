package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/sidecar"
)

const maxToolCallHistory = 1000
const maxSequencesPerAgent = 500

// Skill generation quality control
const (
	minFrequency       = 10
	maxSkillCandidates = 5
	qualityThreshold   = 0.7
	llmTimeout         = 60 * time.Second
)

// GeneralizationScore represents LLM evaluation result
type GeneralizationScore struct {
	Score            float64  `json:"score"`
	Reason           string   `json:"reason"`
	SpecificElements []string `json:"specific_elements"`
}

// Pre-compiled regex patterns for normalizeParams (avoid recompilation on every call)
var (
	reFile           = regexp.MustCompile(`[\w./-]+\.(go|ts|js|py|md|yaml|yml|json|txt)`)
	reNum            = regexp.MustCompile(`\b\d+\b`)
	reURL            = regexp.MustCompile(`https?://[\w./-]+`)
	reHash           = regexp.MustCompile(`\b[a-f0-9]{7,40}\b`)
	reQuote          = regexp.MustCompile(`["'][^"']+["']`)
	reValidSkillName = regexp.MustCompile(`^[a-z0-9-]+$`)
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
	Tools     []string
	Params    []string // normalized parameter patterns
	Hash      string
	Count     int
	FirstSeen time.Time
	LastSeen  time.Time
	TaskDescs []string // task descriptions where this sequence was used
	Agent     string
}

// PatternCandidate represents a detected repeating pattern
type PatternCandidate struct {
	Sequence             *ToolSequence
	SimilarityScore      float64 // 0.0-1.0 from semantic analysis
	SuggestedName        string  // Rule-based fallback name
	SuggestedDesc        string
	LLMGeneratedName     string   // LLM-generated meaningful name (preferred)
	QualityScore         float64  // 0-1 quality score
	IsSingleTool         bool     // true if single tool repeated
	GeneralizationReason string   // LLM assessment reason
	SpecificElements     []string // specific values detected
}

// SkillPatternDetector detects repeating tool call patterns
type SkillPatternDetector struct {
	mu              sync.RWMutex
	toolCalls       []ToolCallRecord
	sequences       map[string]*ToolSequence // hash -> sequence
	sequenceByAgent map[string][]string      // agent -> sequence hashes
	minFrequency    int
	windowMin       int
	windowMax       int
	sidecarEnabled  bool
	sidecar         *sidecar.Sidecar         // sidecar instance for semantic analysis
	clusterCache    map[string]map[int][]int // descriptions hash -> clusters
	cacheMu         sync.RWMutex
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

// SetSidecar sets the sidecar instance for semantic analysis
func (d *SkillPatternDetector) SetSidecar(s *sidecar.Sidecar) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sidecar = s
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

	// Maintain bounded history to prevent unbounded memory growth
	if len(d.toolCalls) > maxToolCallHistory {
		d.toolCalls = d.toolCalls[len(d.toolCalls)-maxToolCallHistory:]
	}

	// Prune sequences periodically to prevent unbounded memory growth
	if len(d.toolCalls)%100 == 0 {
		d.pruneSequencesLocked()
	}

	d.analyzeAgentSequencesLocked(agent)
}

// analyzeAgentSequencesLocked analyzes tool calls for repeating sequences for a specific agent
// Must be called with lock held
func (d *SkillPatternDetector) analyzeAgentSequencesLocked(agent string) {
	if len(d.toolCalls) < d.windowMin {
		return
	}

	var calls []ToolCallRecord
	for _, call := range d.toolCalls {
		if call.Agent == agent {
			calls = append(calls, call)
		}
	}
	d.extractSequencesForAgent(agent, calls)
}

// pruneSequencesLocked removes old sequences to prevent unbounded memory growth
// Must be called with lock held
func (d *SkillPatternDetector) pruneSequencesLocked() {
	for agent, hashes := range d.sequenceByAgent {
		if len(hashes) > maxSequencesPerAgent {
			// Keep only the most recent sequences
			cutoff := len(hashes) - maxSequencesPerAgent
			for _, hash := range hashes[:cutoff] {
				delete(d.sequences, hash)
			}
			d.sequenceByAgent[agent] = hashes[cutoff:]
		}
	}
}

// extractSequencesForAgent extracts sequences from an agent's tool calls.
// Analyzes only windows ending at the latest tool call to prevent double-counting
// and avoid cumulative frequency increments on old entries.
func (d *SkillPatternDetector) extractSequencesForAgent(agent string, calls []ToolCallRecord) {
	if len(calls) < d.windowMin {
		return
	}

	lastIdx := len(calls) - 1

	for windowSize := d.windowMin; windowSize <= d.windowMax && windowSize <= len(calls); windowSize++ {
		startIndex := lastIdx - windowSize + 1
		window := calls[startIndex : lastIdx+1]
		seq := d.buildSequence(window, agent)

		if _, exists := d.sequences[seq.Hash]; !exists {
			d.sequences[seq.Hash] = seq
			d.sequenceByAgent[agent] = append(d.sequenceByAgent[agent], seq.Hash)
		} else {
			existing := d.sequences[seq.Hash]
			existing.Count++
			existing.LastSeen = window[windowSize-1].Timestamp
			existing.TaskDescs = append(existing.TaskDescs, window[windowSize-1].TaskDesc)
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
	// Replace specific values with placeholders using pre-compiled regex
	result := input

	// File paths with extensions
	result = reFile.ReplaceAllString(result, "*.$1")

	// Numbers
	result = reNum.ReplaceAllString(result, "<num>")

	// URLs
	result = reURL.ReplaceAllString(result, "<url>")

	// Hashes (git, etc)
	result = reHash.ReplaceAllString(result, "<hash>")

	// Quoted strings
	result = reQuote.ReplaceAllString(result, "<str>")

	return result
}

// hashSequence creates a unique hash for a sequence
func (d *SkillPatternDetector) hashSequence(seq *ToolSequence) string {
	data := strings.Join(seq.Tools, "|") + "||" + strings.Join(seq.Params, "|")
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// evaluateToolDiversity checks if sequence has at least 2 different tools
func (d *SkillPatternDetector) evaluateToolDiversity(seq *ToolSequence) bool {
	uniqueTools := make(map[string]bool)
	for _, tool := range seq.Tools {
		uniqueTools[tool] = true
	}
	return len(uniqueTools) >= 2
}

// isSingleToolRepeat checks if sequence is single tool repeated
func (d *SkillPatternDetector) isSingleToolRepeat(seq *ToolSequence) bool {
	return !d.evaluateToolDiversity(seq)
}

// buildParamGeneralizationPrompt builds few-shot prompt for LLM evaluation
func (d *SkillPatternDetector) buildParamGeneralizationPrompt(seq *ToolSequence) string {
	var sb strings.Builder
	sb.WriteString("You are a skill generalization analyzer. Evaluate if tool call parameters are:\n")
	sb.WriteString("- GENERIC: Reusable across contexts (e.g., file paths, variable names)\n")
	sb.WriteString("- SPECIFIC: Tied to one context (e.g., hostnames, IPs, specific service names)\n\n")

	// Few-shot examples
	sb.WriteString("## Examples\n\n")

	sb.WriteString("Example 1 (GENERIC - score=0.9):\n")
	sb.WriteString("  1. view(path=\"src/auth/login.ts\")\n")
	sb.WriteString("  2. edit(file=\"src/auth/login.ts\", old=\"...\")\n")
	sb.WriteString("  3. bash(cmd=\"npm test\")\n")
	sb.WriteString("  Analysis: Uses generic file paths and standard commands\n")
	sb.WriteString("  JSON: {\"score\": 0.9, \"reason\": \"Fully generic parameters\", \"specific_elements\": []}\n\n")

	sb.WriteString("Example 2 (SPECIFIC - score=0.2):\n")
	sb.WriteString("  1. ssh(host=\"prod-server-01.acme.com\", cmd=\"kubectl get pods\")\n")
	sb.WriteString("  2. ssh(host=\"prod-server-01.acme.com\", cmd=\"kubectl logs\")\n")
	sb.WriteString("  Analysis: Contains specific hostname and environment\n")
	sb.WriteString("  JSON: {\"score\": 0.2, \"reason\": \"Tied to production environment\", \"specific_elements\": [\"prod-server-01.acme.com\"]}\n\n")

	sb.WriteString("Example 3 (MIXED - score=0.6):\n")
	sb.WriteString("  1. bash(cmd=\"docker build -t myapp .\")\n")
	sb.WriteString("  2. bash(cmd=\"docker push registry.example.com/myapp:latest\")\n")
	sb.WriteString("  Analysis: Generic docker commands but specific registry URL\n")
	sb.WriteString("  JSON: {\"score\": 0.6, \"reason\": \"Generic commands with specific registry\", \"specific_elements\": [\"registry.example.com\"]}\n\n")

	sb.WriteString("## Task to Analyze\n\n")
	sb.WriteString("Tool calls:\n")
	for i, tool := range seq.Tools {
		param := ""
		if i < len(seq.Params) {
			param = seq.Params[i]
		}
		sb.WriteString(fmt.Sprintf("  %d. %s(%s)\n", i+1, tool, param))
	}

	sb.WriteString("\nReturn ONLY JSON: {\"score\": 0.0-1.0, \"reason\": \"explanation\", \"specific_elements\": [\"list\", \"of\", \"specific\", \"values\"]}\n")
	sb.WriteString("Scoring:\n")
	sb.WriteString("  - score=1.0: Fully generic, no specific values\n")
	sb.WriteString("  - score=0.0: Highly specific, contains hostnames/IPs/service names\n")
	sb.WriteString("  - score>=0.7: Acceptable for skill generation\n")

	return sb.String()
}

// evaluateParamGeneralization uses sidecar to evaluate parameter generalization
// Returns score (0-1), reason, and specific elements list
func (d *SkillPatternDetector) evaluateParamGeneralization(ctx context.Context, seq *ToolSequence) (float64, string, []string) {
	if !d.sidecarEnabled || d.sidecar == nil {
		return 0, "sidecar unavailable", nil
	}

	prompt := d.buildParamGeneralizationPrompt(seq)

	timeoutCtx, cancel := context.WithTimeout(ctx, llmTimeout)
	defer cancel()

	result, err := d.sidecar.Execute(timeoutCtx, prompt)
	if err != nil {
		return 0, fmt.Sprintf("sidecar error: %v", err), nil
	}

	score, reason, elements := d.parseGeneralizationScore(result)
	return score, reason, elements
}

// parseGeneralizationScore parses LLM JSON response
func (d *SkillPatternDetector) parseGeneralizationScore(result string) (float64, string, []string) {
	var score GeneralizationScore
	if err := json.Unmarshal([]byte(result), &score); err != nil {
		return 0, "parse error", nil
	}

	// Clamp score to valid range
	if score.Score < 0 {
		score.Score = 0
	}
	if score.Score > 1 {
		score.Score = 1
	}

	return score.Score, score.Reason, score.SpecificElements
}

// calculateQualityScore computes weighted quality score
// toolDiversity weight: 60%, paramGeneralization weight: 40%
func (d *SkillPatternDetector) calculateQualityScore(candidate PatternCandidate, paramScore float64) float64 {
	toolDiversity := 0.0
	if d.evaluateToolDiversity(candidate.Sequence) {
		toolDiversity = 1.0
	}

	return toolDiversity*0.6 + paramScore*0.4
}

// FindCandidates returns high-quality patterns that repeat at least minFrequency times
func (d *SkillPatternDetector) FindCandidates(ctx context.Context) []PatternCandidate {
	// Check sidecar availability - skip if unavailable (no fallback)
	if !d.sidecarEnabled || d.sidecar == nil {
		log.Printf("[INFO] Skill generation skipped: sidecar unavailable")
		return nil
	}

	d.mu.RLock()
	var candidates []PatternCandidate

	for _, seq := range d.sequences {
		if seq.Count >= minFrequency {
			// Deep copy ToolSequence to prevent data race after RUnlock
			copiedSeq := &ToolSequence{
				Tools:     append([]string{}, seq.Tools...),
				Params:    append([]string{}, seq.Params...),
				TaskDescs: append([]string{}, seq.TaskDescs...),
				Count:     seq.Count,
				FirstSeen: seq.FirstSeen,
				LastSeen:  seq.LastSeen,
				Agent:     seq.Agent,
			}
			candidate := PatternCandidate{
				Sequence:        copiedSeq,
				SimilarityScore: 1.0,
				SuggestedName:   d.generateSuggestedName(seq),
				SuggestedDesc:   d.generateSuggestedDescription(seq),
			}
			candidates = append(candidates, candidate)
		}
	}

	d.mu.RUnlock()

	if len(candidates) == 0 {
		return nil
	}

	// Filtering statistics
	var (
		highQualityCandidates []PatternCandidate
		filteredByFrequency   int
		filteredBySingleTool  int
		filteredByQuality     int
		evaluatedCount        int
	)

	for i := range candidates {
		// Frequency filter (already filtered by loop condition, but count for stats)
		if candidates[i].Sequence.Count < minFrequency {
			filteredByFrequency++
			log.Printf("[INFO] Filtered candidate (frequency %d < %d): %v",
				candidates[i].Sequence.Count, minFrequency, candidates[i].Sequence.Tools)
			continue
		}

		// Single tool filter
		candidates[i].IsSingleTool = d.isSingleToolRepeat(candidates[i].Sequence)
		if candidates[i].IsSingleTool {
			filteredBySingleTool++
			log.Printf("[INFO] Filtered candidate (single tool): %v (×%d)",
				candidates[i].Sequence.Tools, candidates[i].Sequence.Count)
			continue
		}

		// LLM evaluation (limit to maxSkillCandidates)
		if evaluatedCount >= maxSkillCandidates {
			log.Printf("[INFO] Stopped LLM evaluation: reached max %d candidates", maxSkillCandidates)
			break
		}

		paramScore, reason, elements := d.evaluateParamGeneralization(ctx, candidates[i].Sequence)
		candidates[i].GeneralizationReason = reason
		candidates[i].SpecificElements = elements

		// Quality score calculation
		candidates[i].QualityScore = d.calculateQualityScore(candidates[i], paramScore)

		// Quality filter
		if candidates[i].QualityScore < qualityThreshold {
			filteredByQuality++
			log.Printf("[INFO] Filtered candidate (quality %.2f < %.2f): %s - %s",
				candidates[i].QualityScore, qualityThreshold, candidates[i].SuggestedName, reason)
			continue
		}

		highQualityCandidates = append(highQualityCandidates, candidates[i])
		evaluatedCount++
	}

	// Filtering summary
	log.Printf("[INFO] Skill pattern filtering summary:")
	log.Printf("  Total candidates: %d", len(candidates))
	log.Printf("  Filtered by frequency (<%d): %d", minFrequency, filteredByFrequency)
	log.Printf("  Filtered by single tool: %d", filteredBySingleTool)
	log.Printf("  Filtered by quality (<%.2f): %d", qualityThreshold, filteredByQuality)
	log.Printf("  High-quality candidates: %d", len(highQualityCandidates))

	// Sort by quality score (descending)
	sort.Slice(highQualityCandidates, func(i, j int) bool {
		return highQualityCandidates[i].QualityScore > highQualityCandidates[j].QualityScore
	})

	return highQualityCandidates
}

// analyzeSemanticSimilarity uses sidecar to analyze and merge similar sequences
func (d *SkillPatternDetector) analyzeSemanticSimilarity(ctx context.Context, sidecar *sidecar.Sidecar, candidates []PatternCandidate) []PatternCandidate {
	// Collect all task descriptions
	allDescs := d.collectAllTaskDescriptions(candidates)
	if len(allDescs) < 2 {
		d.enrichLLMNames(ctx, candidates)
		return candidates
	}

	// Cluster descriptions using sidecar with context
	clusters := d.clusterDescriptions(ctx, sidecar, allDescs)
	if clusters == nil {
		d.enrichLLMNames(ctx, candidates)
		return candidates
	}

	// Merge sequences with similarity >= 0.9
	merged := d.mergeSimilarSequences(candidates, clusters, 0.9)

	// Generate LLM names for all candidates
	d.enrichLLMNames(ctx, merged)

	return merged
}

// enrichLLMNames generates LLM-based names for each candidate.
// Falls back to rule-based SuggestedName on error.
func (d *SkillPatternDetector) enrichLLMNames(ctx context.Context, candidates []PatternCandidate) {
	if !d.sidecarEnabled || d.sidecar == nil {
		return
	}
	for i := range candidates {
		llmName, err := d.generateLLMName(ctx, candidates[i].Sequence)
		if err == nil && llmName != "" {
			candidates[i].LLMGeneratedName = llmName
		}
	}
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
func (d *SkillPatternDetector) clusterDescriptions(ctx context.Context, sidecar *sidecar.Sidecar, descriptions []string) map[int][]int {
	// Check cache first
	descHash := d.hashDescriptions(descriptions)

	d.cacheMu.RLock()
	if cached, ok := d.clusterCache[descHash]; ok {
		// Deep copy cache to prevent data race
		result := make(map[int][]int, len(cached))
		for k, v := range cached {
			result[k] = append([]int{}, v...)
		}
		d.cacheMu.RUnlock()
		return result
	}
	d.cacheMu.RUnlock()

	// Build prompt for sidecar
	prompt := d.buildClusterPrompt(descriptions)

	// Call sidecar with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result, err := sidecar.Execute(timeoutCtx, prompt)
	if err != nil {
		// Don't write to stderr - return nil and let caller handle error
		return nil
	}

	// Parse JSON result
	var clusters map[int][]int
	if err := json.Unmarshal([]byte(result), &clusters); err != nil {
		// Don't write to stderr - return nil silently
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

	// Pre-compute desc → clusterID mapping for O(1) lookup
	descToCluster := d.buildDescToClusterMap(allDescs, clusters)

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
				if d.isInSameClusterFast(cand.Sequence.TaskDescs, mergedGroup[i].Sequence.TaskDescs, descToCluster) {
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

// buildDescToClusterMap creates a mapping from description string to cluster ID.
// This pre-computation avoids O(n³) repeated lookups in isInSameCluster.
func (d *SkillPatternDetector) buildDescToClusterMap(allDescs []string, clusters map[int][]int) map[string]int {
	descToCluster := make(map[string]int, len(allDescs))
	for clusterID, memberIndices := range clusters {
		for _, mIdx := range memberIndices {
			if mIdx < len(allDescs) {
				desc := strings.ToLower(strings.TrimSpace(allDescs[mIdx]))
				descToCluster[desc] = clusterID
			}
		}
	}
	return descToCluster
}

// isInSameClusterFast checks if two sets of descriptions share a cluster using pre-computed map.
// O(n*m) instead of O(n³) where n,m are the number of descriptions in each set.
func (d *SkillPatternDetector) isInSameClusterFast(descs1, descs2 []string, descToCluster map[string]int) bool {
	// Find clusters containing any description from descs1
	clusters1 := make(map[int]bool)
	for _, desc := range descs1 {
		trimmed := strings.ToLower(strings.TrimSpace(desc))
		if clusterID, ok := descToCluster[trimmed]; ok {
			clusters1[clusterID] = true
		}
	}

	// Check if any description in descs2 is in those clusters
	for _, desc := range descs2 {
		trimmed := strings.ToLower(strings.TrimSpace(desc))
		if clusterID, ok := descToCluster[trimmed]; ok {
			if clusters1[clusterID] {
				return true
			}
		}
	}

	// Fallback to keyword overlap if no cluster matched
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

// isValidSkillName validates a LLM-generated name meets the requirements:
// lowercase, hyphenated, max 10 words, no leading/trailing hyphens.
func isValidSkillName(name string) bool {
	if name == "" {
		return false
	}
	if !reValidSkillName.MatchString(name) {
		return false
	}
	words := strings.Split(name, "-")
	if len(words) > 10 {
		return false
	}
	for _, word := range words {
		if len(word) == 0 {
			return false
		}
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}
	return true
}

// buildNamingPrompt constructs the prompt for LLM-based skill naming.
func (d *SkillPatternDetector) buildNamingPrompt(seq *ToolSequence) string {
	var sb strings.Builder

	sb.WriteString("You are a skill naming assistant. Generate a concise, descriptive name for an automated skill.\n\n")
	sb.WriteString("## Tool Sequence\n")
	for i, tool := range seq.Tools {
		param := ""
		if i < len(seq.Params) {
			param = seq.Params[i]
		}
		sb.WriteString(fmt.Sprintf("%d. %s(%s)\n", i+1, tool, param))
	}

	sb.WriteString("\n## Common Task Descriptions\n")
	seen := make(map[string]bool)
	count := 0
	for _, desc := range seq.TaskDescs {
		if desc != "" && !seen[desc] {
			seen[desc] = true
			sb.WriteString(fmt.Sprintf("- %s\n", desc))
			count++
			if count >= 3 {
				break
			}
		}
	}

	sb.WriteString("\n## Requirements\n")
	sb.WriteString("- Generate a short, lowercase, hyphenated name\n")
	sb.WriteString("- Maximum 10 words (hyphen-separated)\n")
	sb.WriteString("- Should describe the skill's purpose, not the tools used\n")
	sb.WriteString("- Examples: \"fix-null-pointer\", \"refactor-auth-module\", \"setup-test-env\"\n")
	sb.WriteString("- Return ONLY the name, no other text\n")

	return sb.String()
}

// generateLLMName uses sidecar to generate a meaningful skill name.
// Returns empty string on error (caller must fallback to generateSuggestedName).
func (d *SkillPatternDetector) generateLLMName(ctx context.Context, seq *ToolSequence) (string, error) {
	if !d.sidecarEnabled || d.sidecar == nil {
		return "", fmt.Errorf("sidecar not enabled")
	}

	prompt := d.buildNamingPrompt(seq)

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	result, err := d.sidecar.Execute(timeoutCtx, prompt)
	if err != nil {
		return "", fmt.Errorf("sidecar naming failed: %w", err)
	}

	name := strings.TrimSpace(result)
	if !isValidSkillName(name) {
		return "", fmt.Errorf("invalid name generated: %q", name)
	}

	return name, nil
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

// isHighValueSequence returns true if the sequence represents a meaningful multi-step workflow.
// It filters out single-tool repetitions and sequences consisting entirely of generic tools.
func (d *SkillPatternDetector) isHighValueSequence(seq *ToolSequence) bool {
	if len(seq.Tools) == 0 {
		return false
	}

	// 1. Must have at least 2 unique tool types (filters out e.g. ssh->ssh->ssh)
	uniqueTools := make(map[string]bool)
	for _, tool := range seq.Tools {
		uniqueTools[tool] = true
	}
	if len(uniqueTools) < 2 {
		return false
	}

	// 2. Must not consist entirely of generic utility/execution/read-only tools
	genericTools := map[string]bool{
		"bash":     true,
		"ssh":      true,
		"sudo":     true,
		"ls":       true,
		"glob":     true,
		"grep":     true,
		"view":     true,
		"ask_user": true,
		"random":   true,
		"math":     true,
	}

	hasNonGeneric := false
	for _, tool := range seq.Tools {
		if !genericTools[tool] {
			hasNonGeneric = true
			break
		}
	}
	return hasNonGeneric
}
