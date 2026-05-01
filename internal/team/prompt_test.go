package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func newTestRegistry(t *testing.T) *TeamRegistry {
	t.Helper()
	searchPaths := []string{"../../.agent-teams"}
	home, err := os.UserHomeDir()
	if err == nil {
		searchPaths = append(searchPaths, filepath.Join(home, ".agent-teams"))
	}
	registry := NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		t.Fatalf("discover error: %v", err)
	}
	if registry.TeamCount() == 0 {
		t.Fatal("no teams discovered")
	}
	t.Logf("Teams: %v", registry.ListTeams())
	return registry
}

func TestParsePromptWithLazyAgents(t *testing.T) {
	registry := newTestRegistry(t)

	tests := []struct {
		name         string
		prompt       string
		defaultTeam  string
		agentDefs    []*agent.AgentDef
		expectError  bool
		checkSegs    func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment)
		expectExpErr bool
	}{
		{
			name:        "@team only",
			prompt:      "@delegate do something",
			defaultTeam: "",
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				if len(lazy) != 1 || lazy[0].Type != SegmentSwitchTeam || lazy[0].Name != "delegate" {
					t.Errorf("lazy: expected [switch_team:delegate], got %v", lazy)
				}
			},
		},
		{
			name:        "@team @agent",
			prompt:      "@delegate @researcher find bugs",
			defaultTeam: "",
			agentDefs:   []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}, {Name: "checker", Role: "worker"}},
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				hasTeamSwitch := false
				hasAgentInvoke := false
				for _, s := range expanded {
					if s.Type == SegmentSwitchTeam && s.Name == "delegate" {
						hasTeamSwitch = true
					}
					if s.Type == SegmentInvokeAgent && s.Name == "researcher" {
						hasAgentInvoke = true
					}
				}
				if !hasTeamSwitch {
					t.Errorf("expanded: missing switch_team for delegate, got %v", expanded)
				}
				if !hasAgentInvoke {
					t.Errorf("expanded: missing invoke_agent for researcher, got %v", expanded)
				}
			},
		},
		{
			name:        "default-team + text",
			prompt:      "do something",
			defaultTeam: "delegate",
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				if len(lazy) != 1 || lazy[0].Type != SegmentSwitchTeam || lazy[0].Name != "delegate" {
					t.Errorf("lazy: expected [switch_team:delegate], got %v", lazy)
				}
				if len(expanded) != 1 || expanded[0].Name != "delegate" {
					t.Errorf("expanded: expected [switch_team:delegate], got %v", expanded)
				}
			},
		},
		{
			name:        "default-team + @agent",
			prompt:      "@researcher find bugs",
			defaultTeam: "delegate",
			agentDefs:   []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}, {Name: "checker", Role: "worker"}},
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				hasTeamSwitch := false
				hasAgentInvoke := false
				for _, s := range expanded {
					if s.Type == SegmentSwitchTeam && s.Name == "delegate" {
						hasTeamSwitch = true
					}
					if s.Type == SegmentInvokeAgent && s.Name == "researcher" {
						hasAgentInvoke = true
					}
				}
				if !hasTeamSwitch {
					t.Errorf("expanded: missing switch_team for delegate, got %v", expanded)
				}
				if !hasAgentInvoke {
					t.Errorf("expanded: missing invoke_agent for researcher, got %v", expanded)
				}
			},
		},
		{
			name:        "text only no team",
			prompt:      "do something",
			defaultTeam: "",
			expectError: true,
		},
		{
			name:        "@bad-team",
			prompt:      "@nonexistent do something",
			defaultTeam: "",
			expectError: true,
		},
		{
			name:        "@bad-agent in team",
			prompt:      "@nonexistent find bugs",
			defaultTeam: "delegate",
			agentDefs:   []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}, {Name: "checker", Role: "worker"}},
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				hasUnknownText := false
				for _, s := range expanded {
					if s.Type == SegmentText && strings.Contains(s.Content, "@nonexistent") {
						hasUnknownText = true
					}
				}
				if !hasUnknownText {
					t.Errorf("expanded: expected @nonexistent to be treated as text, got %v", expanded)
				}
			},
		},
		{
			name:        "@teamA then @teamB",
			prompt:      "@delegate a @tether b",
			defaultTeam: "",
			expectError: false,
			checkSegs: func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment) {
				if len(lazy) != 1 || lazy[0].Type != SegmentSwitchTeam || lazy[0].Name != "delegate" {
					t.Errorf("lazy: expected [switch_team:delegate], got %v", lazy)
				}
				delegateCount := 0
				tetherCount := 0
				for _, s := range expanded {
					if s.Type == SegmentSwitchTeam && s.Name == "delegate" {
						delegateCount++
					}
					if s.Type == SegmentSwitchTeam && s.Name == "tether" {
						tetherCount++
					}
				}
				if delegateCount < 1 {
					t.Errorf("expanded: missing switch_team for delegate, got %v", expanded)
				}
				if tetherCount < 1 {
					t.Errorf("expanded: missing switch_team for tether, got %v", expanded)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lazy, err := ParsePromptWithLazyAgents(tc.prompt, registry, tc.defaultTeam)
			if err != nil {
				if tc.expectError {
					t.Logf("OK (expected error): %v", err)
					return
				}
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.expectError {
				t.Fatalf("expected error but got segments: %v", lazy)
			}

			t.Logf("Lazy segments:")
			for i, s := range lazy {
				t.Logf("  [%d] type=%s name=%q content=%q", i, s.Type, s.Name, s.Content)
			}

			var expanded []PromptSegment
			var expErr error
			for _, seg := range lazy {
				if seg.Type == SegmentSwitchTeam && seg.Content != "" {
					subSegs, err := SplitSegmentByAgents(seg, registry, tc.agentDefs)
					if err != nil {
						expErr = err
						break
					}
					expanded = append(expanded, subSegs...)
				} else {
					expanded = append(expanded, seg)
				}
			}

			if expErr != nil {
				if tc.expectExpErr {
					t.Logf("OK (expected expansion error): %v", expErr)
					return
				}
				t.Fatalf("unexpected expansion error: %v", expErr)
			}
			if tc.expectExpErr {
				t.Fatalf("expected expansion error but got segments: %v", expanded)
			}

			t.Logf("Expanded segments:")
			for i, s := range expanded {
				t.Logf("  [%d] type=%s name=%q content=%q", i, s.Type, s.Name, s.Content)
			}

			if tc.checkSegs != nil {
				tc.checkSegs(t, lazy, expanded)
			}
		})
	}
}

func TestSplitSegmentByAgents(t *testing.T) {
	registry := newTestRegistry(t)

	tests := []struct {
		name      string
		segment   PromptSegment
		agentDefs []*agent.AgentDef
		expectLen int
		expectErr bool
	}{
		{
			name:      "single agent at start",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@researcher find bugs"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}},
			expectLen: 2,
		},
		{
			name:      "text before agent",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "first do X @researcher find bugs"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}},
			expectLen: 2,
		},
		{
			name:      "multiple agents",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@researcher find bugs @writer write docs"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}, {Name: "writer", Role: "worker"}},
			expectLen: 3,
		},
		{
			name:      "no agents in content",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "just plain text"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}},
			expectLen: 1,
		},
		{
			name:      "unknown agent",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@unknown find bugs"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}},
			expectLen: 1,
		},
		{
			name:      "team name in content",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@tether do something"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}},
			expectLen: 2,
		},
		{
			name:      "empty content",
			segment:   PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: ""},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}},
			expectLen: 1,
		},
		{
			name:      "non-switch-team segment",
			segment:   PromptSegment{Type: SegmentText, Content: "hello"},
			agentDefs: []*agent.AgentDef{{Name: "researcher", Role: "worker"}},
			expectLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			segs, err := SplitSegmentByAgents(tc.segment, registry, tc.agentDefs)
			if err != nil {
				if tc.expectErr {
					t.Logf("OK (expected error): %v", err)
					return
				}
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.expectErr {
				t.Fatalf("expected error but got: %v", segs)
			}

			t.Logf("Segments (%d):", len(segs))
			for i, s := range segs {
				t.Logf("  [%d] type=%s name=%q content=%q", i, s.Type, s.Name, s.Content)
			}

			if len(segs) != tc.expectLen {
				t.Errorf("expected %d segments, got %d: %v", tc.expectLen, len(segs), segs)
			}
		})
	}
}

func TestExtractUntilNextAt(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"find bugs", "find bugs"},
		{"find bugs @writer write", "find bugs "},
		{"", ""},
		{"  @next", "  "},
	}

	for _, tc := range tests {
		result := extractUntilNextAt(tc.input)
		if strings.TrimSpace(result) != strings.TrimSpace(tc.expected) {
			t.Errorf("extractUntilNextAt(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestHasAtName(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"hello @teamA", true},
		{"@teamA@agent1", true},
		{"hello world", false},
		{"email@example.com", false},
		{"", false},
		{"no at-refs here", false},
		{"@-leading", false},
	}

	for _, tc := range tests {
		got := HasAtName(tc.input)
		if got != tc.want {
			t.Errorf("HasAtName(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestIsAgentInListFuzzyMatch(t *testing.T) {
	agents := []*agent.AgentDef{
		{Name: "Senior Developer", FileAlias: "engineering-senior-developer", Role: "worker"},
		{Name: "Code Reviewer", FileAlias: "engineering-code-reviewer", Role: "worker"},
		{Name: "Software Architect", FileAlias: "engineering-software-architect", Role: "worker"},
	}

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"exact match", "Senior Developer", true},
		{"exact match case insensitive", "senior developer", true},
		{"word match", "developer", true},
		{"word match reviewer", "reviewer", true},
		{"segment match architect", "architect", true},
		{"file alias exact match", "engineering-software-architect", true},
		{"partial word no match", "devel", false},
		{"no match", "designer", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAgentInList(tc.input, agents)
			if got != tc.want {
				t.Errorf("isAgentInList(%q, ...) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
