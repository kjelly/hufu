package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		name        string
		prompt      string
		defaultTeam string
		agentNames  []string
		expectError bool
		checkSegs   func(t *testing.T, lazy []PromptSegment, expanded []PromptSegment)
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
			agentNames:  []string{"researcher", "writer", "checker"},
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
			agentNames:  []string{"researcher", "writer", "checker"},
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
			name:         "@bad-agent in team",
			prompt:       "@nonexistent find bugs",
			defaultTeam:  "delegate",
			agentNames:   []string{"researcher", "writer", "checker"},
			expectError:  false,
			expectExpErr: true,
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
					subSegs, err := SplitSegmentByAgents(seg, registry, tc.agentNames)
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
		name       string
		segment   PromptSegment
		agents    []string
		expectLen int
		expectErr bool
	}{
		{
			name:     "single agent at start",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@researcher find bugs"},
			agents:   []string{"researcher", "writer"},
			expectLen: 2,
		},
		{
			name:     "text before agent",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "first do X @researcher find bugs"},
			agents:   []string{"researcher", "writer"},
			expectLen: 2,
		},
		{
			name:     "multiple agents",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@researcher find bugs @writer write docs"},
			agents:   []string{"researcher", "writer"},
			expectLen: 3,
		},
		{
			name:     "no agents in content",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "just plain text"},
			agents:   []string{"researcher"},
			expectLen: 1,
		},
		{
			name:     "unknown agent",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@unknown find bugs"},
			agents:   []string{"researcher"},
			expectErr: true,
		},
		{
			name:     "team name in content",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: "@tether do something"},
			agents:   []string{"researcher"},
			expectLen: 2,
		},
		{
			name:     "empty content",
			segment:  PromptSegment{Type: SegmentSwitchTeam, Name: "delegate", Content: ""},
			agents:   []string{"researcher"},
			expectLen: 1,
		},
		{
			name:     "non-switch-team segment",
			segment:  PromptSegment{Type: SegmentText, Content: "hello"},
			agents:   []string{"researcher"},
			expectLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			segs, err := SplitSegmentByAgents(tc.segment, registry, tc.agents)
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
		{"email@example.com", true},
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