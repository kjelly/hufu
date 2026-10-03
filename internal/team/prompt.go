package team

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

type PromptSegmentType string

const (
	SegmentSwitchTeam  PromptSegmentType = "switch_team"
	SegmentInvokeAgent PromptSegmentType = "invoke_agent"
	SegmentText        PromptSegmentType = "text"
)

type PromptSegment struct {
	Type    PromptSegmentType
	Name    string
	Content string
	IsPiped bool
}

var atNamePattern = regexp.MustCompile(`\B@([\w][\w-]*)`)

func HasAtName(s string) bool {
	return atNamePattern.MatchString(s)
}

func ParsePromptWithLazyAgents(rawPrompt string, registry *TeamRegistry, defaultTeam string) ([]PromptSegment, error) {
	if defaultTeam == "" && !atNamePattern.MatchString(rawPrompt) {
		return nil, fmt.Errorf("no team specified. Use --agent-team or @team-name in the prompt (available: %s)", strings.Join(registry.ListTeams(), ", "))
	}

	if defaultTeam != "" {
		return []PromptSegment{{Type: SegmentSwitchTeam, Name: strings.ToLower(defaultTeam), Content: rawPrompt}}, nil
	}

	var locs []struct{ start, end, nameStart, nameEnd int }
	for _, loc := range atNamePattern.FindAllStringSubmatchIndex(rawPrompt, -1) {
		locs = append(locs, struct{ start, end, nameStart, nameEnd int }{loc[0], loc[1], loc[2], loc[3]})
	}
	for _, loc := range locs {
		name := strings.ToLower(rawPrompt[loc.nameStart:loc.nameEnd])
		if registry.HasTeam(name) {
			content := strings.TrimSpace(rawPrompt[loc.end:])
			return []PromptSegment{{Type: SegmentSwitchTeam, Name: name, Content: content}}, nil
		}

		// Try fuzzy team matching for typo correction
		bestMatch := ""
		bestScore := 0.0
		for _, tName := range registry.ListTeams() {
			score := similarityScore(name, tName)
			if score > bestScore {
				bestScore = score
				bestMatch = tName
			}
		}
		if bestScore >= 0.75 && bestMatch != "" && bestMatch != name {
			fmt.Fprintf(os.Stderr, "Note: Corrected typo @%s to @%s (similarity: %.0f%%)\n", name, bestMatch, bestScore*100)
			content := strings.TrimSpace(rawPrompt[loc.end:])
			return []PromptSegment{{Type: SegmentSwitchTeam, Name: bestMatch, Content: content}}, nil
		}
	}

	return nil, fmt.Errorf("no team found in prompt. Available teams: %s", strings.Join(registry.ListTeams(), ", "))
}

// SplitSegmentByAgents splits a team prompt at mentions of a team or agent.
// Only a mention that resolves to one, directly or by typo correction,
// starts a segment; any other @word, such as the host in
// "https://user:[REDACTED]@example.invalid" or a handle in prose, stays in
// the text. Splitting at those dropped the rest of the prompt from the run.
func SplitSegmentByAgents(segment PromptSegment, registry *TeamRegistry, currentAgents []*agent.AgentDef) ([]PromptSegment, error) {
	if segment.Type != SegmentSwitchTeam || segment.Content == "" {
		return []PromptSegment{segment}, nil
	}

	content := segment.Content
	type mention struct {
		start, end int
		name       string
	}
	var mentions []mention
	for _, loc := range atNamePattern.FindAllStringSubmatchIndex(content, -1) {
		if name, ok := resolveMentionName(strings.ToLower(content[loc[2]:loc[3]]), registry, currentAgents); ok {
			mentions = append(mentions, mention{start: loc[0], end: loc[1], name: name})
		}
	}
	if len(mentions) == 0 {
		return []PromptSegment{segment}, nil
	}

	teamName := segment.Name
	// The team segment carries any text before the first mention; it stays
	// even when empty so the mentioned steps run under the current team.
	segments := []PromptSegment{{Type: SegmentSwitchTeam, Name: teamName, Content: strings.TrimSpace(content[:mentions[0].start])}}
	for i, m := range mentions {
		taskEnd := len(content)
		if i+1 < len(mentions) {
			taskEnd = mentions[i+1].start
		}
		segmentType := SegmentInvokeAgent
		if registry.HasTeam(m.name) {
			segmentType = SegmentSwitchTeam
		}
		segments = append(segments, PromptSegment{Type: segmentType, Name: m.name, Content: strings.TrimSpace(content[m.end:taskEnd])})
	}
	return segments, nil
}

// resolveMentionName returns the team or agent a mention names: an exact team,
// then an exact agent, then the closest team or agent when the name looks
// like a typo of it. Coordinators are never mention targets.
func resolveMentionName(name string, registry *TeamRegistry, currentAgents []*agent.AgentDef) (string, bool) {
	if registry.HasTeam(name) || isAgentInList(name, currentAgents) {
		return name, true
	}
	bestMatch := ""
	bestScore := 0.0
	for _, tName := range registry.ListTeams() {
		if score := similarityScore(name, tName); score > bestScore {
			bestScore, bestMatch = score, tName
		}
	}
	for _, ag := range currentAgents {
		if ag.Role == "orchestrator" || ag.Role == "coordinator" {
			continue
		}
		if score := similarityScore(name, strings.ToLower(ag.Name)); score > bestScore {
			bestScore, bestMatch = score, strings.ToLower(ag.Name)
		}
		if ag.FileAlias != "" {
			if score := similarityScore(name, strings.ToLower(ag.FileAlias)); score > bestScore {
				bestScore, bestMatch = score, strings.ToLower(ag.FileAlias)
			}
		}
	}
	if bestScore >= 0.75 && bestMatch != "" && bestMatch != name {
		fmt.Fprintf(os.Stderr, "Note: Corrected typo @%s to @%s (similarity: %.0f%%)\n", name, bestMatch, bestScore*100)
		return bestMatch, true
	}
	return "", false
}

func isAgentInList(name string, agents []*agent.AgentDef) bool {
	nameLower := strings.ToLower(name)
	for _, def := range agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		// Exact case-insensitive match against Name
		if strings.ToLower(def.Name) == nameLower {
			return true
		}
		// Word-level match on Name
		for _, word := range strings.Fields(def.Name) {
			if strings.ToLower(word) == nameLower {
				return true
			}
		}
		// Segment-level match on FileAlias
		if def.FileAlias != "" {
			// Exact case-insensitive match on full FileAlias
			if strings.ToLower(def.FileAlias) == nameLower {
				return true
			}
			for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
				if seg != "" && seg == nameLower {
					return true
				}
			}
		}
	}
	return false
}

func similarityScore(s, t string) float64 {
	d := levenshteinDistance(s, t)
	maxLen := len(s)
	if len(t) > maxLen {
		maxLen = len(t)
	}
	if maxLen == 0 {
		return 1.0
	}
	return 1.0 - float64(d)/float64(maxLen)
}

func levenshteinDistance(s, t string) int {
	d := make([][]int, len(s)+1)
	for i := range d {
		d[i] = make([]int, len(t)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(s); i++ {
		for j := 1; j <= len(t); j++ {
			cost := 1
			if s[i-1] == t[j-1] {
				cost = 0
			}
			d[i][j] = minInt(
				d[i-1][j]+1,
				minInt(d[i][j-1]+1, d[i-1][j-1]+cost),
			)
		}
	}
	return d[len(s)][len(t)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func SplitSegmentsByPipe(segments []PromptSegment) []PromptSegment {
	var result []PromptSegment
	for _, seg := range segments {
		if seg.Content == "" {
			result = append(result, seg)
			continue
		}

		parts := splitOnChainPipe(seg.Content)
		for i, part := range parts {
			part = strings.TrimSpace(part)

			isPiped := false
			if i > 0 {
				isPiped = true
			} else {
				isPiped = seg.IsPiped
			}

			newSeg := seg
			newSeg.Content = part
			newSeg.IsPiped = isPiped

			result = append(result, newSeg)
		}
	}
	return result
}

// splitOnChainPipe splits content on " | " only where the delimiter is
// immediately followed by an @mention. Prompt chaining is meant to hand off
// to another agent/team step, so this avoids misinterpreting a literal
// " | " inside task text (e.g. a shell pipeline the user is describing) as
// a chain boundary.
func splitOnChainPipe(content string) []string {
	const delim = " | "
	var parts []string
	start := 0
	for {
		idx := strings.Index(content[start:], delim)
		if idx < 0 {
			break
		}
		splitAt := start + idx
		after := content[splitAt+len(delim):]
		trimmedAfter := strings.TrimLeft(after, " \t")
		if loc := atNamePattern.FindStringIndex(trimmedAfter); loc != nil && loc[0] == 0 {
			parts = append(parts, content[:splitAt])
			content = after
			start = 0
			continue
		}
		start = splitAt + len(delim)
	}
	parts = append(parts, content)
	return parts
}
