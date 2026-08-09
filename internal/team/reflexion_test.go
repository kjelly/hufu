package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
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
		session:        &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}},
		executionRunID: "run-1",
	}

	lesson := formatReflexionLesson("coder", "fix bug", "verification failed", "create the file first", true, false)
	c.persistReflexionLesson(lesson)

	ltm := LoadLTM(ws, "test")
	if strings.Contains(ltm, "fix bug") {
		t.Fatalf("candidate lesson must not be promoted before acceptance:\n%s", ltm)
	}
	manifest := &EvidenceManifest{RunID: "run-1", Status: "accepted", ManifestHash: "manifest-1"}
	c.promoteCandidateLessons(manifest)
	if got := LoadLTM(ws, "test"); got != "" {
		t.Fatalf("candidate with blank manifest binding was promoted: %q", got)
	}
	if err := c.bindCandidateLessonsToManifest(manifest); err != nil {
		t.Fatal(err)
	}
	c.promoteCandidateLessons(manifest)
	ltm = LoadLTM(ws, "test")
	if !strings.Contains(ltm, "fix bug") {
		t.Fatalf("confirmed lesson not promoted to LTM:\n%s", ltm)
	}

	// A second identical write must deduplicate.
	c.persistReflexionLesson(lesson)
	if err := c.bindCandidateLessonsToManifest(manifest); err != nil {
		t.Fatal(err)
	}
	c.promoteCandidateLessons(manifest)
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

func TestKnowledgeCandidatesRemainHiddenUntilAcceptedPromotion(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: ws, Config: agent.TeamConfig{Name: "test"}}, executionRunID: "run-rejected"}
	c.persistKnowledgeCandidate("use the verified adapter", ltmSectionPatterns, "ltm_update")
	if got := LoadLTM(ws, "test"); got != "" {
		t.Fatalf("candidate knowledge leaked into LTM before acceptance: %q", got)
	}
	c.promoteCandidateLessons(&EvidenceManifest{RunID: "run-accepted", Status: "accepted", ManifestHash: "accepted-manifest"})
	if got := LoadLTM(ws, "test"); got != "" {
		t.Fatalf("rejected-run candidate promoted by unrelated run: %q", got)
	}
	c.promoteCandidateLessons(&EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "rejected-manifest"})
	got := LoadLTM(ws, "test")
	if got != "" {
		t.Fatalf("candidate with blank manifest binding was promoted: %q", got)
	}
	if err := c.bindCandidateLessonsToManifest(&EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "rejected-manifest"}); err != nil {
		t.Fatal(err)
	}
	c.promoteCandidateLessons(&EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "other-manifest"})
	if got := LoadLTM(ws, "test"); got != "" {
		t.Fatalf("candidate with mismatched manifest binding was promoted: %q", got)
	}
	c.promoteCandidateLessons(&EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "rejected-manifest"})
	got = LoadLTM(ws, "test")
	if !strings.Contains(got, ltmSectionPatterns) || !strings.Contains(got, "use the verified adapter") {
		t.Fatalf("bound candidate missing from expected section: %q", got)
	}
	c.promoteCandidateLessons(&EvidenceManifest{RunID: "run-rejected", Status: "accepted", ManifestHash: "rejected-manifest"})
	confirmed, err := os.ReadFile(filepath.Join(ws, logsDir, reflexionConfirmedFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(confirmed), "use the verified adapter"); got != 1 {
		t.Fatalf("confirmed candidate duplicated: count=%d", got)
	}
}
