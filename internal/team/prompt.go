package team

import (
	"fmt"
	"regexp"
	"strings"
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

func ParsePrompt(prompt string, registry *TeamRegistry, currentTeam string, currentAgents []string) ([]PromptSegment, error) {
	locs := atNamePattern.FindAllStringSubmatchIndex(prompt, -1)
	if len(locs) == 0 {
		if currentTeam == "" {
			return nil, fmt.Errorf("no team specified. Use --agent-team or @team-name in the prompt (available: %s)", strings.Join(registry.ListTeams(), ", "))
		}
		return []PromptSegment{{Type: SegmentText, Content: strings.TrimSpace(prompt)}}, nil
	}

	var segments []PromptSegment
	prevEnd := 0

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

		if registry != nil && registry.HasTeam(nameLower) {
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
		} else if currentTeam != "" && isAgentInList(nameLower, currentAgents) {
			if textBefore != "" {
				segments = append(segments, PromptSegment{Type: SegmentText, Content: textBefore})
			}
			segments = append(segments, PromptSegment{
				Type:    SegmentInvokeAgent,
				Name:    nameLower,
				Content: strings.TrimSpace(taskContent),
			})
		} else if currentTeam == "" {
			return nil, fmt.Errorf("@%s — no active team. Specify a team with --agent-team or @team-name first (teams: %s)", name, strings.Join(registry.ListTeams(), ", "))
		} else {
			availableTeams := strings.Join(registry.ListTeams(), ", ")
			agentList := strings.Join(currentAgents, ", ")
			return nil, fmt.Errorf("@%s not found as team or agent (teams: [%s], current team %q agents: [%s])", name, availableTeams, currentTeam, agentList)
		}

		prevEnd = loc[1] + consumedLen
	}

	textAfter := strings.TrimSpace(prompt[prevEnd:])
	if textAfter != "" {
		if currentTeam == "" {
			return nil, fmt.Errorf("text with no active team — specify a team first")
		}
		segments = append(segments, PromptSegment{Type: SegmentText, Content: textAfter})
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

func SplitSegmentByAgents(segment PromptSegment, registry *TeamRegistry, currentAgents []string) ([]PromptSegment, error) {
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

		if registry.HasTeam(name) {
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
		} else if isAgentInList(name, currentAgents) {
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
		} else {
			return nil, fmt.Errorf("@%s not found as team or agent in current context (agents: [%s])", name, strings.Join(currentAgents, ", "))
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

func isAgentInList(name string, agents []string) bool {
	for _, a := range agents {
		if strings.ToLower(a) == name {
			return true
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