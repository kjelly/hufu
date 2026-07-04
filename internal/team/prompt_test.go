package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestHasAtName(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"@team task", true},
		{"@team-name task", true},
		{"hello @agent do this", true},
		{"no at sign here", false},
		{"email@example.com", false}, // \B requires non-word boundary, 'l' is word char
		{"@123 invalid", true},       // 1 is a word character, so this matches
		{"@-invalid", false},         // doesn't start with letter/word
		{"@valid_agent", true},
		{"multiple @team1 and @team2", true},
		{"", false},
		{" @team after space", true}, // space is non-word boundary
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := HasAtName(tt.input)
			if got != tt.want {
				t.Errorf("HasAtName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParsePromptWithLazyAgents(t *testing.T) {
	registry := NewTeamRegistry([]string{".test-teams"})
	// Manually add test teams
	registry.teams = map[string]string{
		"delegate": "/path/to/delegate",
		"tether":   "/path/to/tether",
		"reviewer": "/path/to/reviewer",
	}

	tests := []struct {
		name        string
		prompt      string
		defaultTeam string
		want        []PromptSegment
		wantErr     bool
		errMsg      string
	}{
		{
			name:        "no team specified no default",
			prompt:      "just a task",
			defaultTeam: "",
			wantErr:     true,
			errMsg:      "no team specified",
		},
		{
			name:        "default team provided",
			prompt:      "do this task",
			defaultTeam: "delegate",
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "do this task"},
			},
		},
		{
			name:   "team in prompt",
			prompt: "@delegate research this",
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "research this"},
			},
		},
		{
			name:   "team with hyphen",
			prompt: "@my-team do something",
			wantErr: true,
			errMsg: "no team found",
		},
		{
			name:   "unknown team",
			prompt: "@unknown do task",
			wantErr: true,
			errMsg: "no team found",
		},
		{
			name:   "team name case insensitive",
			prompt: "@DELEGATE research",
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "research"},
			},
		},
		{
			name:   "team in middle of prompt",
			prompt: "first part @delegate research",
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "research"},
			},
		},
		{
			name:   "team typo corrected",
			prompt: "@delegat research",
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "research"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePromptWithLazyAgents(tt.prompt, registry, tt.defaultTeam)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParsePromptWithLazyAgents() expected error")
					return
				}
				if tt.errMsg != "" && !containsStr(err.Error(), tt.errMsg) {
					t.Errorf("ParsePromptWithLazyAgents() error = %v, want contains %q", err, tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Errorf("ParsePromptWithLazyAgents() unexpected error = %v", err)
				return
			}

			if len(got) != len(tt.want) {
				t.Errorf("ParsePromptWithLazyAgents() got %d segments, want %d", len(got), len(tt.want))
				return
			}

			for i, seg := range got {
				if seg.Type != tt.want[i].Type || seg.Name != tt.want[i].Name || seg.Content != tt.want[i].Content {
					t.Errorf("Segment %d: got %+v, want %+v", i, seg, tt.want[i])
				}
			}
		})
	}
}

func TestSplitSegmentByAgents(t *testing.T) {
	registry := NewTeamRegistry([]string{".test-teams"})
	registry.teams = map[string]string{
		"delegate": "/path/to/delegate",
		"tether":   "/path/to/tether",
	}

	agents := []*agent.AgentDef{
		{Name: "Researcher", Role: "worker", FileAlias: "researcher"},
		{Name: "Writer", Role: "worker", FileAlias: "writer"},
		{Name: "Code Reviewer", Role: "worker", FileAlias: "reviewer"},
		{Name: "Coordinator", Role: "coordinator", FileAlias: "coord"},
	}

	tests := []struct {
		name         string
		segment      PromptSegment
		want         []PromptSegment
		wantContains []PromptSegmentType
	}{
		{
			name: "no @ references",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "research this topic",
			},
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "delegate", Content: "research this topic"},
			},
		},
		{
			name: "agent reference in content",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "start @researcher find bugs",
			},
			wantContains: []PromptSegmentType{SegmentSwitchTeam, SegmentInvokeAgent},
		},
		{
			name: "team switch in content",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "research @tether also check",
			},
			wantContains: []PromptSegmentType{SegmentSwitchTeam, SegmentSwitchTeam},
		},
		{
			name: "multiple agent references",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "@researcher find bugs @writer write docs",
			},
			wantContains: []PromptSegmentType{SegmentSwitchTeam, SegmentInvokeAgent, SegmentInvokeAgent},
		},
		{
			name: "mixed team and agent",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "@researcher check @tether verify",
			},
			wantContains: []PromptSegmentType{SegmentSwitchTeam, SegmentInvokeAgent, SegmentSwitchTeam},
		},
		{
			name: "text before first at",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "please @researcher check this",
			},
			// Text before @ is combined with agent invocation
			wantContains: []PromptSegmentType{SegmentSwitchTeam, SegmentInvokeAgent},
		},
		{
			name: "coordinator skipped",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "delegate",
				Content: "@coordinator do task",
			},
			// Coordinator should not be invoked directly, treated as text
			wantContains: []PromptSegmentType{SegmentText},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SplitSegmentByAgents(tt.segment, registry, agents)
			if err != nil {
				t.Errorf("SplitSegmentByAgents() unexpected error = %v", err)
				return
			}

			if len(tt.want) > 0 {
				if len(got) != len(tt.want) {
					t.Errorf("SplitSegmentByAgents() got %d segments, want %d", len(got), len(tt.want))
				}
				for i, seg := range got {
					if i < len(tt.want) {
						if seg.Type != tt.want[i].Type || seg.Name != tt.want[i].Name || seg.Content != tt.want[i].Content {
							t.Errorf("Segment %d: got %+v, want %+v", i, seg, tt.want[i])
						}
					}
				}
			}

			if len(tt.wantContains) > 0 {
				if len(got) != len(tt.wantContains) {
					t.Errorf("SplitSegmentByAgents() got %d segments, want %d (by type count)", len(got), len(tt.wantContains))
				}
				for i, expectedType := range tt.wantContains {
					if i < len(got) && got[i].Type != expectedType {
						t.Errorf("Segment %d type: got %v, want %v", i, got[i].Type, expectedType)
					}
				}
			}
		})
	}
}

func TestExtractUntilNextAt(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"task until @next", "task until "},
		{"no at sign", "no at sign"},
		{"@immediate", ""},
		{"multiple @first and @second", "multiple "},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractUntilNextAt(tt.input)
			if got != tt.want {
				t.Errorf("extractUntilNextAt(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsAgentInList(t *testing.T) {
	agents := []*agent.AgentDef{
		{Name: "Researcher", Role: "worker", FileAlias: "researcher"},
		{Name: "Writer", Role: "worker", FileAlias: "writer"},
		{Name: "Code Reviewer", Role: "worker", FileAlias: "code-reviewer"},
		{Name: "Coordinator", Role: "coordinator", FileAlias: "coord"},
	}

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"exact name match", "researcher", true},
		{"case insensitive", "RESEARCHER", true},
		{"file alias match", "researcher", true},
		{"partial name match", "code", true},
		{"coordinator excluded", "coordinator", false},
		{"not in list", "unknown", false},
		{"writer match", "writer", true},
		{"alias with hyphen", "code-reviewer", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isAgentInList(tt.query, agents)
			if got != tt.want {
				t.Errorf("isAgentInList(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestParsePromptEdgeCases(t *testing.T) {
	registry := NewTeamRegistry([]string{".test-teams"})
	registry.teams = map[string]string{
		"a": "/path/a",
		"ab": "/path/ab",
	}

	tests := []struct {
		name    string
		prompt  string
		wantErr bool
	}{
		{"single char team", "@a task", false},
		{"substring team", "@ab task", false},
		{"email pattern", "contact @user@example.com", true},
		{"multiple spaces", "@a    task   with   spaces", false},
		{"tabs and newlines", "@a\ttask\nwith\tnewlines", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParsePromptWithLazyAgents(tt.prompt, registry, "")
			if tt.wantErr && err == nil {
				t.Errorf("ParsePromptWithLazyAgents() expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("ParsePromptWithLazyAgents() unexpected error = %v", err)
			}
		})
	}
}

func TestSplitSegmentByAgentsEdgeCases(t *testing.T) {
	registry := NewTeamRegistry([]string{".test-teams"})
	registry.teams = map[string]string{
		"team1": "/path/team1",
	}

	agents := []*agent.AgentDef{
		{Name: "Agent1", Role: "worker", FileAlias: "agent1"},
	}

	tests := []struct {
		name    string
		segment PromptSegment
	}{
		{
			name: "empty content",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "team1",
				Content: "",
			},
		},
		{
			name: "not switch team type",
			segment: PromptSegment{
				Type:    SegmentText,
				Content: "@agent1 do task",
			},
		},
		{
			name: "unknown agent treated as text",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "team1",
				Content: "@unknown do task",
			},
		},
		{
			name: "consecutive at references",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "team1",
				Content: "@agent1@team1 task",
			},
		},
		{
			name: "trailing text after last at",
			segment: PromptSegment{
				Type:    SegmentSwitchTeam,
				Name:    "team1",
				Content: "@agent1 do task and more",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SplitSegmentByAgents(tt.segment, registry, agents)
			if err != nil {
				t.Errorf("SplitSegmentByAgents() unexpected error = %v", err)
			}
		})
	}
}

func TestSplitSegmentsByPipe(t *testing.T) {
	tests := []struct {
		name  string
		input []PromptSegment
		want  []PromptSegment
	}{
		{
			name: "chains on pipe before an at-mention",
			input: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@generator propose | @auditor critique: {{PREV_RESULT}}"},
			},
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@generator propose", IsPiped: false},
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@auditor critique: {{PREV_RESULT}}", IsPiped: true},
			},
		},
		{
			name: "does not chain on a literal shell pipe in task text",
			input: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@coder run: find . -name '*.go' | xargs gofmt"},
			},
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@coder run: find . -name '*.go' | xargs gofmt", IsPiped: false},
			},
		},
		{
			name: "mixed: literal pipe followed by a real chain step",
			input: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@coder run: ls | grep foo | @reviewer check the output: {{PREV_RESULT}}"},
			},
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@coder run: ls | grep foo", IsPiped: false},
				{Type: SegmentSwitchTeam, Name: "team1", Content: "@reviewer check the output: {{PREV_RESULT}}", IsPiped: true},
			},
		},
		{
			name: "empty content segment passes through unchanged",
			input: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: ""},
			},
			want: []PromptSegment{
				{Type: SegmentSwitchTeam, Name: "team1", Content: ""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitSegmentsByPipe(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("SplitSegmentsByPipe() returned %d segments, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].Content != tt.want[i].Content || got[i].IsPiped != tt.want[i].IsPiped {
					t.Errorf("segment %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Helper function
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && findSubstr(s, substr)
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
