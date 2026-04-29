package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSkillFile tests the parseSkillFile function with various inputs
func TestParseSkillFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *SkillDef
		wantErr bool
	}{
		{
			name: "valid skill file with all fields",
			content: `---
name: test-skill
description: A test skill for validation
allowed-tools: bash,grep
---
This is the skill content.
It can have multiple lines.
`,
			want: &SkillDef{
				Name:         "test-skill",
				Description:  "A test skill for validation",
				AllowedTools: "bash,grep",
				Content:      "This is the skill content.\nIt can have multiple lines.",
			},
			wantErr: false,
		},
		{
			name: "valid skill file with minimal fields",
			content: `---
name: minimal-skill
---
Minimal skill content.
`,
			want: &SkillDef{
				Name:    "minimal-skill",
				Content: "Minimal skill content.",
			},
			wantErr: false,
		},
		{
			name:    "missing frontmatter",
			content: "This is not a skill file.\nNo frontmatter here.",
			want:    nil,
			wantErr: true,
		},
		{
			name:    "missing name field",
			content: `---
description: No name here
---
Content.
`,
			want:    nil,
			wantErr: true,
		},
		{
			name:    "malformed frontmatter",
			content: `---
name: test
description
---
Content.
`,
			want: &SkillDef{
				Name:        "test",
				Description: "",
				Content:     "Content.",
			},
			wantErr: false, // Parser continues gracefully, skipping malformed lines
		},
		{
			name:    "quoted values",
			content: `---
name: "quoted-name"
description: 'single quoted'
---
Content.
`,
			want: &SkillDef{
				Name:        "quoted-name",
				Description: "single quoted",
				Content:     "Content.",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			skillPath := filepath.Join(tmpDir, "SKILL.md")

			if err := os.WriteFile(skillPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("failed to create test file: %v", err)
			}

			got := parseSkillFile(skillPath)
			if (got == nil) != tt.wantErr {
				t.Errorf("parseSkillFile() error = %v, wantErr %v", got == nil, tt.wantErr)
				return
			}

			if got == nil {
				return
			}

			if got.Name != tt.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, tt.want.Name)
			}
			if got.Description != tt.want.Description {
				t.Errorf("Description = %q, want %q", got.Description, tt.want.Description)
			}
			if got.AllowedTools != tt.want.AllowedTools {
				t.Errorf("AllowedTools = %q, want %q", got.AllowedTools, tt.want.AllowedTools)
			}
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.Path != skillPath {
				t.Errorf("Path = %q, want %q", got.Path, skillPath)
			}
		})
	}
}

// TestBuildSummary tests the buildSummary function
func TestBuildSummary(t *testing.T) {
	tests := []struct {
		name     string
		def      *SkillDef
		expected string
	}{
		{
			name: "description takes precedence",
			def: &SkillDef{
				Name:        "test",
				Description: "This is a detailed description that should be used as summary.",
				Content:     "This is the actual content that should be ignored for summary.",
			},
			expected: "This is a detailed description that should be used as summary.",
		},
		{
			name: "description truncated to 200 chars",
			def: &SkillDef{
				Name:        "test",
				Description: strings.Repeat("a", 250),
				Content:     "Content",
			},
			expected: strings.Repeat("a", 200) + "...",
		},
		{
			name: "first non-comment line from content",
			def: &SkillDef{
				Name:    "test",
				Content: "# Comment line\n\nThis is the first real line.\nAnother line.",
			},
			expected: "This is the first real line.",
		},
		{
			name: "first line truncated to 200 chars",
			def: &SkillDef{
				Name:    "test",
				Content: strings.Repeat("a", 250),
			},
			expected: strings.Repeat("a", 200) + "...",
		},
		{
			name: "empty content returns empty string",
			def: &SkillDef{
				Name:    "test",
				Content: "",
			},
			expected: "",
		},
		{
			name: "only comment lines returns empty string",
			def: &SkillDef{
				Name:    "test",
				Content: "# Comment 1\n# Comment 2\n",
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSummary(tt.def)
			if got != tt.expected {
				t.Errorf("buildSummary() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestParseSkillYAML tests the parseSkillYAML function
func TestParseSkillYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name: "parse simple key-value pairs",
			input: `name: test-skill
description: A test skill
allowed-tools: bash,grep`,
			expected: map[string]string{
				"name":          "test-skill",
				"description":   "A test skill",
				"allowed-tools": "bash,grep",
			},
		},
		{
			name: "skip comment lines",
			input: `# This is a comment
name: test-skill
# Another comment`,
			expected: map[string]string{
				"name": "test-skill",
			},
		},
		{
			name: "skip empty lines",
			input: `name: test-skill

description: A test skill`,
			expected: map[string]string{
				"name":        "test-skill",
				"description": "A test skill",
			},
		},
		{
			name: "handle quoted values",
			input: `name: "quoted-name"
description: 'single quoted'`,
			expected: map[string]string{
				"name":        "quoted-name",
				"description": "single quoted",
			},
		},
		{
			name:     "empty input returns empty map",
			input:    "",
			expected: map[string]string{},
		},
		{
			name: "skip malformed lines",
			input: `name: test-skill
malformed line without colon
description: A test skill`,
			expected: map[string]string{
				"name":        "test-skill",
				"description": "A test skill",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSkillYAML(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("parseSkillYAML() length = %d, want %d", len(got), len(tt.expected))
			}
			for k, v := range tt.expected {
				if got[k] != v {
					t.Errorf("parseSkillYAML()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestDiscoverSkills tests the DiscoverSkills function
func TestDiscoverSkills(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test skill directories
	skillDirs := []string{
		filepath.Join(tmpDir, "skill-a"),
		filepath.Join(tmpDir, "skill-b"),
		filepath.Join(tmpDir, "skill-c"),
	}

	for _, dir := range skillDirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("failed to create skill directory: %v", err)
		}

		skillPath := filepath.Join(dir, "SKILL.md")
		skillName := strings.TrimPrefix(dir, tmpDir+"/")
		content := `---
name: ` + skillName + `
description: Test skill ` + skillName + `
---
Content for ` + skillName + `
`
		if err := os.WriteFile(skillPath, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to create skill file: %v", err)
		}
	}

	// Create a directory without SKILL.md
	nonSkillDir := filepath.Join(tmpDir, "no-skill")
	if err := os.MkdirAll(nonSkillDir, 0o755); err != nil {
		t.Fatalf("failed to create non-skill directory: %v", err)
	}

	tests := []struct {
		name    string
		dirs    []string
		wantLen int
	}{
		{
			name:    "discover all skills",
			dirs:    []string{tmpDir},
			wantLen: 3,
		},
		{
			name:    "discover from multiple directories",
			dirs:    []string{tmpDir, "/nonexistent"},
			wantLen: 3,
		},
		{
			name:    "discover from nonexistent directory",
			dirs:    []string{"/nonexistent"},
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skills := DiscoverSkills(tt.dirs)

			if len(skills) != tt.wantLen {
				t.Errorf("DiscoverSkills() returned %d skills, want %d", len(skills), tt.wantLen)
			}

			// Check that all skills have valid names
			for _, skill := range skills {
				if skill.Name == "" {
					t.Errorf("DiscoverSkills() returned skill with empty name")
				}
				if skill.Content == "" {
					t.Errorf("DiscoverSkills() returned skill with empty content")
				}
			}
		})
	}
}

// TestFilterSkills tests the FilterSkills function
func TestFilterSkills(t *testing.T) {
	skills := []*SkillDef{
		{Name: "skill-a", Description: "Skill A"},
		{Name: "skill-b", Description: "Skill B"},
		{Name: "skill-c", Description: "Skill C"},
		{Name: "skill-d", Description: "Skill D"},
	}

	tests := []struct {
		name     string
		skills   []*SkillDef
		include  []string
		exclude  []string
		wantLen  int
		wantNames []string
	}{
		{
			name:     "no filtering returns all skills",
			skills:   skills,
			include:  nil,
			exclude:  nil,
			wantLen:  4,
			wantNames: []string{"skill-a", "skill-b", "skill-c", "skill-d"},
		},
		{
			name:     "filter by include",
			skills:   skills,
			include:  []string{"skill-a", "skill-c"},
			exclude:  nil,
			wantLen:  2,
			wantNames: []string{"skill-a", "skill-c"},
		},
		{
			name:     "filter by exclude",
			skills:   skills,
			include:  nil,
			exclude:  []string{"skill-b", "skill-d"},
			wantLen:  2,
			wantNames: []string{"skill-a", "skill-c"},
		},
		{
			name:     "filter by both include and exclude",
			skills:   skills,
			include:  []string{"skill-a", "skill-b", "skill-c"},
			exclude:  []string{"skill-b"},
			wantLen:  2,
			wantNames: []string{"skill-a", "skill-c"},
		},
		{
			name:     "case-insensitive filtering",
			skills:   skills,
			include:  []string{"SKILL-A"},
			exclude:  nil,
			wantLen:  1,
			wantNames: []string{"skill-a"},
		},
		{
			name:     "filter with whitespace",
			skills:   skills,
			include:  []string{" skill-a "},
			exclude:  nil,
			wantLen:  1,
			wantNames: []string{"skill-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterSkills(tt.skills, tt.include, tt.exclude)

			if len(got) != tt.wantLen {
				t.Errorf("FilterSkills() returned %d skills, want %d", len(got), tt.wantLen)
			}

			gotNames := make([]string, len(got))
			for i, skill := range got {
				gotNames[i] = skill.Name
			}

			if len(gotNames) != len(tt.wantNames) {
				t.Errorf("FilterSkills() returned names %v, want %v", gotNames, tt.wantNames)
			}

			for i, wantName := range tt.wantNames {
				if gotNames[i] != wantName {
					t.Errorf("FilterSkills()[%d] = %q, want %q", i, gotNames[i], wantName)
				}
			}
		})
	}
}

// TestSkillsByName tests the SkillsByName function
func TestSkillsByName(t *testing.T) {
	skills := []*SkillDef{
		{Name: "skill-a", Description: "Skill A"},
		{Name: "skill-b", Description: "Skill B"},
		{Name: "skill-c", Description: "Skill C"},
	}

	tests := []struct {
		name     string
		skills   []*SkillDef
		names    []string
		wantLen  int
		wantNames []string
	}{
		{
			name:     "select by name",
			skills:   skills,
			names:    []string{"skill-a", "skill-c"},
			wantLen:  2,
			wantNames: []string{"skill-a", "skill-c"},
		},
		{
			name:     "select with case-insensitive names",
			skills:   skills,
			names:    []string{"SKILL-B"},
			wantLen:  1,
			wantNames: []string{"skill-b"},
		},
		{
			name:     "select with whitespace",
			skills:   skills,
			names:    []string{" skill-a "},
			wantLen:  1,
			wantNames: []string{"skill-a"},
		},
		{
			name:     "select nonexistent name returns empty",
			skills:   skills,
			names:    []string{"nonexistent"},
			wantLen:  0,
			wantNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SkillsByName(tt.skills, tt.names)

			if len(got) != tt.wantLen {
				t.Errorf("SkillsByName() returned %d skills, want %d", len(got), tt.wantLen)
			}

			gotNames := make([]string, len(got))
			for i, skill := range got {
				gotNames[i] = skill.Name
			}

			for i, wantName := range tt.wantNames {
				if gotNames[i] != wantName {
					t.Errorf("SkillsByName()[%d] = %q, want %q", i, gotNames[i], wantName)
				}
			}
		})
	}
}

// TestCopySkillsToWorkspace tests the CopySkillsToWorkspace function
func TestCopySkillsToWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	skills := []*SkillDef{
		{Name: "skill-a", Content: "Content A"},
		{Name: "skill-b", Content: "Content B"},
	}

	tests := []struct {
		name    string
		skills  []*SkillDef
		wantErr bool
	}{
		{
			name:    "copy skills successfully",
			skills:  skills,
			wantErr: false,
		},
		{
			name:    "copy empty skills list",
			skills:  []*SkillDef{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CopySkillsToWorkspace(tt.skills, tmpDir)

			if (err != nil) != tt.wantErr {
				t.Errorf("CopySkillsToWorkspace() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Check that skill files were created
				for _, skill := range tt.skills {
					skillPath := filepath.Join(tmpDir, "shared", "skills", strings.ToLower(skill.Name)+".md")
					if _, err := os.Stat(skillPath); os.IsNotExist(err) {
						t.Errorf("CopySkillsToWorkspace() did not create skill file: %s", skillPath)
					} else {
						content, readErr := os.ReadFile(skillPath)
						if readErr != nil {
							t.Errorf("failed to read skill file: %v", readErr)
						} else if string(content) != skill.Content {
							t.Errorf("CopySkillsToWorkspace() skill content mismatch: got %q, want %q", string(content), skill.Content)
						}
					}
				}
			}
		})
	}
}

// TestSkillWorkspacePath tests the SkillWorkspacePath function
func TestSkillWorkspacePath(t *testing.T) {
	tests := []struct {
		name     string
		workspace string
		skillName string
		expected string
	}{
		{
			name:     "basic path",
			workspace: "/tmp/workspace",
			skillName: "TestSkill",
			expected: "/tmp/workspace/shared/skills/testskill.md",
		},
		{
			name:     "lowercase skill name",
			workspace: "/tmp/workspace",
			skillName: "testskill",
			expected: "/tmp/workspace/shared/skills/testskill.md",
		},
		{
			name:     "skill name with spaces",
			workspace: "/tmp/workspace",
			skillName: "My Skill",
			expected: "/tmp/workspace/shared/skills/my skill.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SkillWorkspacePath(tt.workspace, tt.skillName)
			if got != tt.expected {
				t.Errorf("SkillWorkspacePath() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestParseSkillList tests the ParseSkillList function
func TestParseSkillList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "parse comma-separated list",
			input:    "skill-a,skill-b,skill-c",
			expected: []string{"skill-a", "skill-b", "skill-c"},
		},
		{
			name:     "parse with whitespace",
			input:    " skill-a , skill-b , skill-c ",
			expected: []string{"skill-a", "skill-b", "skill-c"},
		},
		{
			name:     "parse with empty entries",
			input:    "skill-a,,skill-b,,skill-c",
			expected: []string{"skill-a", "skill-b", "skill-c"},
		},
		{
			name:     "parse empty string returns nil",
			input:    "",
			expected: nil,
		},
		{
			name:     "parse single item",
			input:    "skill-a",
			expected: []string{"skill-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSkillList(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("ParseSkillList() length = %d, want %d", len(got), len(tt.expected))
			}
			for i, v := range tt.expected {
				if got[i] != v {
					t.Errorf("ParseSkillList()[%d] = %q, want %q", i, got[i], v)
				}
			}
		})
	}
}

// TestSkillDefFields tests that SkillDef has all expected fields
func TestSkillDefFields(t *testing.T) {
	skill := &SkillDef{
		Name:         "test-skill",
		Description:  "A test skill",
		AllowedTools: "bash,grep",
		Content:      "Skill content",
		Path:         "/path/to/skill.md",
		Summary:      "Test skill summary",
	}

	if skill.Name != "test-skill" {
		t.Errorf("Name = %q, want %q", skill.Name, "test-skill")
	}
	if skill.Description != "A test skill" {
		t.Errorf("Description = %q, want %q", skill.Description, "A test skill")
	}
	if skill.AllowedTools != "bash,grep" {
		t.Errorf("AllowedTools = %q, want %q", skill.AllowedTools, "bash,grep")
	}
	if skill.Content != "Skill content" {
		t.Errorf("Content = %q, want %q", skill.Content, "Skill content")
	}
	if skill.Path != "/path/to/skill.md" {
		t.Errorf("Path = %q, want %q", skill.Path, "/path/to/skill.md")
	}
	if skill.Summary != "Test skill summary" {
		t.Errorf("Summary = %q, want %q", skill.Summary, "Test skill summary")
	}
}
