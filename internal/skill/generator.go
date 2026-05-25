package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"unicode"
)

// AutoSkillGenerator generates skill files from detected patterns
type AutoSkillGenerator struct {
	baseDir string
}

// NewAutoSkillGenerator creates a new generator
func NewAutoSkillGenerator(baseDir string) *AutoSkillGenerator {
	return &AutoSkillGenerator{
		baseDir: baseDir,
	}
}

// GenerateSkill creates a SKILL.md file from a pattern candidate.
// Prefers LLM-generated name when available, falls back to rule-based SuggestedName.
func (g *AutoSkillGenerator) GenerateSkill(candidate PatternCandidate) (string, error) {
	skillName := candidate.SuggestedName
	if candidate.LLMGeneratedName != "" {
		skillName = candidate.LLMGeneratedName
	}

	skillDir := filepath.Join(g.baseDir, skillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create skill directory: %w", err)
	}

	content := g.buildSkillContent(candidate, skillName)
	skillPath := filepath.Join(skillDir, "SKILL.md")

	if err := os.WriteFile(skillPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write skill file: %w", err)
	}

	return skillPath, nil
}

// buildSkillContent generates the SKILL.md content
func (g *AutoSkillGenerator) buildSkillContent(candidate PatternCandidate, skillName string) string {
	var sb strings.Builder

	// Frontmatter
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", skillName))
	sb.WriteString(fmt.Sprintf("description: %s\n", candidate.SuggestedDesc))
	sb.WriteString("---\n\n")

	// Title
	sb.WriteString(fmt.Sprintf("# %s\n\n", titleCase(strings.ReplaceAll(skillName, "-", " "))))

	// Overview
	sb.WriteString("## Overview\n\n")
	sb.WriteString(fmt.Sprintf("Auto-generated skill detected from **%d** similar executions.\n\n", candidate.Sequence.Count))
	sb.WriteString(fmt.Sprintf("**First seen:** %s\n\n", candidate.Sequence.FirstSeen.Format("2006-01-02 15:04")))
	sb.WriteString(fmt.Sprintf("**Last seen:** %s\n\n", candidate.Sequence.LastSeen.Format("2006-01-02 15:04")))

	// Workflow
	sb.WriteString("## Workflow\n\n")
	sb.WriteString("This skill automates the following tool sequence:\n\n")
	for i, tool := range candidate.Sequence.Tools {
		param := ""
		if i < len(candidate.Sequence.Params) {
			param = candidate.Sequence.Params[i]
		}
		sb.WriteString(fmt.Sprintf("%d. **%s** - `%s`\n", i+1, tool, param))
	}
	sb.WriteString("\n")

	// Example
	sb.WriteString("## Example Execution\n\n")
	sb.WriteString("Observed pattern:\n\n")
	sb.WriteString("```bash\n")
	for i, tool := range candidate.Sequence.Tools {
		param := ""
		if i < len(candidate.Sequence.Params) {
			param = candidate.Sequence.Params[i]
		}
		sb.WriteString(fmt.Sprintf("# Step %d: %s\n", i+1, tool))
		if param != "" {
			sb.WriteString(fmt.Sprintf("%s %s\n", tool, param))
		}
	}
	sb.WriteString("```\n\n")

	// Task descriptions
	if len(candidate.Sequence.TaskDescs) > 0 {
		sb.WriteString("## Common Use Cases\n\n")
		sb.WriteString("This skill was used in the following contexts:\n\n")
		
		// Count task description frequency
		descFreq := make(map[string]int)
		for _, desc := range candidate.Sequence.TaskDescs {
			if desc != "" {
				descFreq[desc]++
			}
		}

		// Show top 5
		type kv struct {
			Key   string
			Value int
		}
		var sorted []kv
		for k, v := range descFreq {
			sorted = append(sorted, kv{k, v})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Value > sorted[j].Value
		})

		count := 0
		for _, item := range sorted {
			if count >= 5 {
				break
			}
			sb.WriteString(fmt.Sprintf("- %s (×%d)\n", item.Key, item.Value))
			count++
		}
		sb.WriteString("\n")
	}

	// Notes
	sb.WriteString("## Notes\n\n")
	sb.WriteString("**This skill was auto-generated.** Please review and refine:\n\n")
	sb.WriteString("- Verify the tool sequence is correct\n")
	sb.WriteString("- Add error handling if needed\n")
	sb.WriteString("- Improve parameter patterns\n")
	sb.WriteString("- Add edge cases and gotchas\n\n")

	// Template for skill generation
	tmpl := `
// Template for generating similar skills
func ApplyWorkflow(ctx context.Context, params map[string]string) error {
	// TODO: Implement workflow logic
	{{range $i, $tool := .Sequence.Tools}}
	// Step {{add $i 1}}: {{$tool}}
	{{end}}
	return nil
}
`
	sb.WriteString("## Implementation Template\n\n")
	sb.WriteString("```go\n")
	t := template.Must(template.New("impl").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}).Parse(tmpl))
	t.Execute(&sb, candidate)
	sb.WriteString("```\n")

	return sb.String()
}

// titleCase returns the string with the first letter of each word capitalized.
func titleCase(s string) string {
	runes := []rune(s)
	inWord := false
	for i, r := range runes {
		if unicode.IsSpace(r) {
			inWord = false
		} else if !inWord {
			runes[i] = unicode.ToUpper(r)
			inWord = true
		}
	}
	return string(runes)
}
