package team

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestFormatReflexionLesson(t *testing.T) {
	long := strings.Repeat("x", 300)

	cases := []struct {
		name              string
		agent             string
		goal              string
		failure           string
		hint              string
		rescued           bool
		verifyPolarityBug bool
		contains          []string
		absent            []string
	}{
		{
			name:     "rescued includes fix",
			agent:    "coder",
			goal:     "fix the bug",
			failure:  "test failed",
			hint:     "add nil check",
			rescued:  true,
			contains: []string{"agent coder", "fix the bug", "test failed", "fixed by: add nil check"},
		},
		{
			name:     "final failure warns to avoid",
			agent:    "coder",
			goal:     "fix the bug",
			failure:  "timeout",
			hint:     "",
			rescued:  false,
			contains: []string{"agent coder", "fails: timeout", "avoid this approach"},
			absent:   []string{"fixed by"},
		},
		{
			name:     "newlines collapsed",
			agent:    "a",
			goal:     "line1\nline2\n\tline3",
			failure:  "err\nmore",
			hint:     "h",
			rescued:  true,
			contains: []string{"line1 line2 line3", "err more"},
			absent:   []string{"\n"},
		},
		{
			name:    "long parts truncated",
			agent:   "a",
			goal:    long,
			failure: long,
			hint:    long,
			rescued: true,
		},
		{
			name:              "verify polarity bug points at the verify command, not the task",
			agent:             "deployer",
			goal:              "Phase 5: cleanup",
			failure:           `deliverable verification failed (command "grep -c br-verify"): exit status 1: 0 — wrong polarity`,
			rescued:           false,
			verifyPolarityBug: true,
			contains:          []string{"agent deployer", "Phase 5: cleanup", "wrong exit-code polarity", "the task itself is fine"},
			absent:            []string{"avoid this approach", "fixed by"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatReflexionLesson(tc.agent, tc.goal, tc.failure, tc.hint, tc.rescued, tc.verifyPolarityBug)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("lesson %q missing %q", got, want)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("lesson %q should not contain %q", got, bad)
				}
			}
			if n := len([]rune(got)); n > 220 {
				t.Errorf("lesson too long: %d runes", n)
			}
		})
	}
}

func TestPersistReflexionLesson(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}},
	}

	lesson := formatReflexionLesson("coder", "fix bug", "verification failed", "create the file first", true, false)
	c.persistReflexionLesson(lesson)

	ltm := LoadLTM(ws, "test")
	if !strings.Contains(ltm, "fix bug") {
		t.Fatalf("lesson not persisted to LTM:\n%s", ltm)
	}

	// A second identical write must deduplicate.
	c.persistReflexionLesson(lesson)
	ltm2 := LoadLTM(ws, "test")
	if strings.Count(ltm2, "fix bug") != 1 {
		t.Errorf("duplicate lesson not deduplicated:\n%s", ltm2)
	}

	// Empty lessons are ignored.
	c.persistReflexionLesson("   ")
	if LoadLTM(ws, "test") != ltm2 {
		t.Errorf("empty lesson modified LTM")
	}
}
