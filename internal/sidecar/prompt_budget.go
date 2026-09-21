package sidecar

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PromptSection is one structurally delimited block in a bounded sidecar
// prompt. Header and Footer are always preserved; only Content is shortened.
// Weight controls the relative share of the remaining content budget, while
// MaxRunes optionally caps the section before weighted allocation.
type PromptSection struct {
	Header   string
	Content  string
	Footer   string
	Weight   int
	MaxRunes int
}

type preparedPromptSection struct {
	PromptSection
	runes      []rune
	desired    int
	allocation int
}

// BuildBoundedPrompt renders a prompt that never exceeds maxRunes. It keeps
// structural delimiters and the final instruction intact, distributes the
// available evidence budget deterministically, and annotates omitted content.
func BuildBoundedPrompt(prefix string, sections []PromptSection, suffix string, maxRunes int) (string, error) {
	if maxRunes <= 0 {
		return "", fmt.Errorf("bounded prompt requires a positive rune limit")
	}

	fixedRunes := utf8.RuneCountInString(prefix) + utf8.RuneCountInString(suffix)
	prepared := make([]preparedPromptSection, 0, len(sections))
	for _, section := range sections {
		section.Content = strings.TrimSpace(section.Content)
		if section.Content == "" {
			continue
		}
		sectionRunes := []rune(section.Content)
		desired := len(sectionRunes)
		if section.MaxRunes > 0 {
			desired = min(desired, section.MaxRunes)
		}
		section.Weight = max(section.Weight, 1)
		fixedRunes += utf8.RuneCountInString(section.Header) + utf8.RuneCountInString(section.Footer)
		prepared = append(prepared, preparedPromptSection{
			PromptSection: section,
			runes:         sectionRunes,
			desired:       desired,
		})
	}
	if fixedRunes > maxRunes {
		return "", fmt.Errorf("bounded prompt structure exceeds input limit: %d runes > %d", fixedRunes, maxRunes)
	}

	remaining := maxRunes - fixedRunes
	for remaining > 0 {
		progressed := false
		for i := range prepared {
			section := &prepared[i]
			for range section.Weight {
				if remaining == 0 || section.allocation >= section.desired {
					break
				}
				section.allocation++
				remaining--
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}

	var prompt strings.Builder
	prompt.Grow(min(maxRunes, len(prefix)+len(suffix)+fixedRunes))
	prompt.WriteString(prefix)
	for _, section := range prepared {
		prompt.WriteString(section.Header)
		prompt.WriteString(renderBoundedPromptContent(section.runes, section.allocation))
		prompt.WriteString(section.Footer)
	}
	prompt.WriteString(suffix)
	result := prompt.String()
	if resultRunes := utf8.RuneCountInString(result); resultRunes > maxRunes {
		return "", fmt.Errorf("bounded prompt renderer exceeded input limit: %d runes > %d", resultRunes, maxRunes)
	}
	return result, nil
}

func renderBoundedPromptContent(content []rune, limit int) string {
	if len(content) <= limit {
		return string(content)
	}
	if limit <= 0 {
		return ""
	}

	prefixRunes := limit
	for {
		omitted := len(content) - prefixRunes
		marker := []rune(fmt.Sprintf("\n... [%d runes omitted]", omitted))
		if len(marker) >= limit {
			return string(marker[:limit])
		}
		nextPrefixRunes := limit - len(marker)
		if nextPrefixRunes == prefixRunes {
			return string(content[:prefixRunes]) + string(marker)
		}
		prefixRunes = nextPrefixRunes
	}
}
