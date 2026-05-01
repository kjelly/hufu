package team

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/anomalyco/hufu/internal/agent"
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
}

var atNamePattern = regexp.MustCompile(`\B@([\w][\w-]*)`)

func HasAtName(s string) bool {
	return atNamePattern.MatchString(s)
}

func ParsePrompt(prompt string, registry *TeamRegistry, currentTeam string, currentAgents []*agent.AgentDef) ([]PromptSegment, error) {
	locs := atNamePattern.FindAllStringSubmatchIndex(prompt, -1)
	if len(locs) == 0 {
		if currentTeam == "" {
			return nil, fmt.Errorf("no team specified. Use --agent-team or @team-name in the prompt (available: %s)", strings.Join(registry.ListTeams(), ", "))
		}
		return []PromptSegment{{Type: SegmentText, Content: strings.TrimSpace(prompt)}}, nil
	}

	var segments []PromptSegment
	prevEnd := 0
	foundTeamOrAgent := false

	for _, loc := range locs {
		fullStart := loc[0]
		nameStart := loc[2]
		nameEnd := loc[3]
		name := prompt[nameStart:nameEnd]
		nameLower := strings.ToLower(name)

		textBefore := strings.TrimSpace(prompt[prevEnd:fullStart])

		restAfter := prompt[loc[1]:]
		taskContent := extractUntilNextAt(restAfter)
		consumedLen := len(taskContent)

		if currentTeam != "" && isAgentInList(nameLower, currentAgents) {
			foundTeamOrAgent = true
			if textBefore != "" {
				segments = append(segments, PromptSegment{Type: SegmentText, Content: textBefore})
			}
			segments = append(segments, PromptSegment{
				Type:    SegmentInvokeAgent,
				Name:    nameLower,
				Content: strings.TrimSpace(taskContent),
			})
		} else if registry != nil && registry.HasTeam(nameLower) {
			foundTeamOrAgent = true
			if textBefore != "" && currentTeam != "" {
				segments = append(segments, PromptSegment{Type: SegmentText, Content: textBefore})
			} else if textBefore != "" && currentTeam == "" {
				return nil, fmt.Errorf("text before @%s with no active team — specify a team first", name)
			}
			segments = append(segments, PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    nameLower,
				Content: strings.TrimSpace(taskContent),
			})
			currentTeam = nameLower
			currentAgents = nil
		} else {
			segments = append(segments, PromptSegment{Type: SegmentText, Content: "@" + name + extractUntilNextAt(prompt[loc[1]:])})
			prevEnd = loc[1] + consumedLen
			continue
		}

		prevEnd = loc[1] + consumedLen
	}

	textAfter := strings.TrimSpace(prompt[prevEnd:])
	if textAfter != "" {
		if currentTeam == "" && foundTeamOrAgent {
			return nil, fmt.Errorf("text with no active team — specify a team first")
		}
		if currentTeam != "" {
			segments = append(segments, PromptSegment{Type: SegmentText, Content: textAfter})
		} else if len(locs) == 0 {
			return []PromptSegment{{Type: SegmentText, Content: strings.TrimSpace(prompt)}}, nil
		}
	}

	return segments, nil
}

func ParsePromptWithLazyAgents(rawPrompt string, registry *TeamRegistry, defaultTeam string) ([]PromptSegment, error) {
	if defaultTeam == "" && !atNamePattern.MatchString(rawPrompt) {
		return nil, fmt.Errorf("no team specified. Use --agent-team or @team-name in the prompt (available: %s)", strings.Join(registry.ListTeams(), ", "))
	}

	if defaultTeam != "" {
		return []PromptSegment{{Type: SegmentSwitchTeam, Name: strings.ToLower(defaultTeam), Content: rawPrompt}}, nil
	}

	locs := atNamePattern.FindAllStringSubmatchIndex(rawPrompt, -1)
	for _, loc := range locs {
		nameStart := loc[2]
		nameEnd := loc[3]
		name := strings.ToLower(rawPrompt[nameStart:nameEnd])
		if registry.HasTeam(name) {
			return []PromptSegment{{Type: SegmentSwitchTeam, Name: name, Content: rawPrompt}}, nil
		}
	}

	return nil, fmt.Errorf("no team found in prompt. Available teams: %s", strings.Join(registry.ListTeams(), ", "))
}

func SplitSegmentByAgents(segment PromptSegment, registry *TeamRegistry, currentAgents []*agent.AgentDef) ([]PromptSegment, error) {
	if segment.Type != SegmentSwitchTeam || segment.Content == "" {
		return []PromptSegment{segment}, nil
	}

	content := segment.Content
	locs := atNamePattern.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return []PromptSegment{segment}, nil
	}

	teamName := segment.Name

	var segments []PromptSegment
	prevEnd := 0
	needsTeamHeader := true
	textBeforeFirstAt := strings.TrimSpace(content[:locs[0][0]])
	if textBeforeFirstAt != "" {
		segments = append(segments, PromptSegment{Type: SegmentSwitchTeam, Name: teamName, Content: textBeforeFirstAt})
		needsTeamHeader = false
		prevEnd = locs[0][0]
	}

	for _, loc := range locs {
		fullStart := loc[0]
		nameStart := loc[2]
		nameEnd := loc[3]
		name := strings.ToLower(content[nameStart:nameEnd])

		textBefore := strings.TrimSpace(content[prevEnd:fullStart])
		restAfter := content[loc[1]:]
		taskContent := extractUntilNextAt(restAfter)
		consumedLen := len(taskContent)

		if isAgentInList(name, currentAgents) {
			if needsTeamHeader {
				segments = append(segments, PromptSegment{Type: SegmentSwitchTeam, Name: teamName, Content: ""})
				needsTeamHeader = false
			}
			if textBefore != "" {
				segments = append(segments, PromptSegment{Type: SegmentText, Content: textBefore})
			}
			segments = append(segments, PromptSegment{
				Type:    SegmentInvokeAgent,
				Name:    name,
				Content: strings.TrimSpace(taskContent),
			})
		} else if registry.HasTeam(name) {
			if needsTeamHeader {
				segments = append(segments, PromptSegment{Type: SegmentSwitchTeam, Name: teamName, Content: ""})
				needsTeamHeader = false
			}
			if textBefore != "" {
				segments = append(segments, PromptSegment{Type: SegmentText, Content: textBefore})
			}
			segments = append(segments, PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    name,
				Content: strings.TrimSpace(taskContent),
			})
		} else {
			segments = append(segments, PromptSegment{Type: SegmentText, Content: "@" + name + extractUntilNextAt(content[loc[1]:])})
			prevEnd = loc[1] + consumedLen
			continue
		}

		prevEnd = loc[1] + consumedLen
	}

	textAfter := strings.TrimSpace(content[prevEnd:])
	if textAfter != "" {
		if needsTeamHeader {
			segments = append(segments, PromptSegment{Type: SegmentSwitchTeam, Name: teamName, Content: ""})
			needsTeamHeader = false
		}
		segments = append(segments, PromptSegment{Type: SegmentText, Content: textAfter})
	}

	if len(segments) == 0 {
		return []PromptSegment{segment}, nil
	}

	return segments, nil
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

func extractUntilNextAt(rest string) string {
	nextAt := atNamePattern.FindStringIndex(rest)
	if nextAt == nil {
		return rest
	}
	return rest[:nextAt[0]]
}
