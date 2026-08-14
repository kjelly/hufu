package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/yamlutil"
)

type SkillDef struct {
	Name         string
	Description  string
	AllowedTools string
	Content      string
	Path         string
	Summary      string
	Draft        bool
	CreatedAt    time.Time
}

func parseSkillFile(path string) *SkillDef {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	def, err := parseSkillBytes(raw)
	if err != nil {
		return nil
	}
	def.Path = path
	if info, err := os.Stat(path); err == nil && def.CreatedAt.IsZero() {
		def.CreatedAt = info.ModTime()
	}
	return def
}

// ValidateSkillDraft parses a complete SKILL.md without touching the filesystem.
func ValidateSkillDraft(raw []byte) (*SkillDef, error) {
	def, err := parseSkillBytes(raw)
	if err != nil {
		return nil, err
	}
	if def.Description == "" {
		return nil, fmt.Errorf("skill draft is missing required description")
	}
	if def.Content == "" {
		return nil, fmt.Errorf("skill draft body is empty")
	}
	return def, nil
}

func parseSkillBytes(raw []byte) (*SkillDef, error) {
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("skill draft must start with YAML frontmatter")
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return nil, fmt.Errorf("skill draft has malformed frontmatter")
	}
	fm := parseSkillYAML(rest[:idx])
	body := strings.TrimSpace(rest[idx+5:])

	if fm["name"] == "" {
		return nil, fmt.Errorf("skill draft is missing required name")
	}
	def := &SkillDef{
		Name:         fm["name"],
		Description:  fm["description"],
		AllowedTools: fm["allowed-tools"],
		Content:      body,
	}

	def.Summary = buildSummary(def)

	if ts := fm["created_at"]; ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			def.CreatedAt = t
		}
	}
	return def, nil
}

func buildSummary(def *SkillDef) string {
	if def.Description != "" {
		desc := def.Description
		if len(desc) > 200 {
			desc = desc[:200] + "..."
		}
		return desc
	}
	firstLine := ""
	lines := strings.Split(def.Content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			firstLine = line
			break
		}
	}
	if len(firstLine) > 200 {
		firstLine = firstLine[:200] + "..."
	}
	return firstLine
}

type skillFrontmatter struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	AllowedTools string    `yaml:"allowed-tools"`
	CreatedAt    time.Time `yaml:"created_at"`
}

func parseSkillYAML(data string) map[string]string {
	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(data), &fm); err == nil {
		result := map[string]string{}
		if fm.Name != "" {
			result["name"] = fm.Name
		}
		if fm.Description != "" {
			result["description"] = fm.Description
		}
		if fm.AllowedTools != "" {
			result["allowed-tools"] = fm.AllowedTools
		}
		if !fm.CreatedAt.IsZero() {
			result["created_at"] = fm.CreatedAt.Format(time.RFC3339)
		}
		return result
	}
	return yamlutil.ParseSimpleYAML(data)
}

// DiscoverSkills scans the given directories for SKILL.md files.
// When includeDrafts is true, the `drafts/` subdirectory of each dir is
// also scanned; discovered drafts are marked with SkillDef.Draft=true.
// When false (the default for the LLM-facing skill pool), drafts are
// excluded entirely.
func DiscoverSkills(dirs []string, includeDrafts bool) []*SkillDef {
	seen := map[string]bool{}
	var skills []*SkillDef

	for _, dir := range dirs {
		scanSkillDir(dir, false, seen, &skills)
		if includeDrafts {
			scanSkillDir(filepath.Join(dir, "drafts"), true, seen, &skills)
		}
	}
	return skills
}

// scanSkillDir scans a single directory for skill subdirectories and
// appends discovered skills to the slice.
func scanSkillDir(dir string, draft bool, seen map[string]bool, skills *[]*SkillDef) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(dir, entry.Name(), "SKILL.md")
		def := parseSkillFile(skillPath)
		if def == nil {
			continue
		}
		def.Draft = draft
		nameLower := strings.ToLower(def.Name)
		if seen[nameLower] {
			continue
		}
		seen[nameLower] = true
		*skills = append(*skills, def)
	}
}

func FilterSkills(skills []*SkillDef, include, exclude []string) []*SkillDef {
	includeMap := map[string]bool{}
	for _, s := range include {
		includeMap[strings.ToLower(strings.TrimSpace(s))] = true
	}
	excludeMap := map[string]bool{}
	for _, s := range exclude {
		excludeMap[strings.ToLower(strings.TrimSpace(s))] = true
	}

	if len(includeMap) == 0 && len(excludeMap) == 0 {
		return skills
	}

	var filtered []*SkillDef
	for _, s := range skills {
		nameLower := strings.ToLower(s.Name)
		if excludeMap[nameLower] {
			continue
		}
		if len(includeMap) > 0 && !includeMap[nameLower] {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func SkillsByName(skills []*SkillDef, names []string) []*SkillDef {
	nameMap := map[string]bool{}
	for _, n := range names {
		nameMap[strings.ToLower(strings.TrimSpace(n))] = true
	}
	var result []*SkillDef
	for _, s := range skills {
		if nameMap[strings.ToLower(s.Name)] {
			result = append(result, s)
		}
	}
	return result
}

func ParseSkillList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}
