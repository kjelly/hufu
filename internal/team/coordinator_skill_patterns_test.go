package team

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestSkillPatternSnapshotEmptyEvaluationReplacesPreviousData(t *testing.T) {
	c := skillPatternSnapshotTestCoordinator(t.TempDir())
	candidate := teamSnapshotTestCandidate(time.Now().UTC())
	if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{}, nil); err != nil {
		t.Fatal(err)
	}

	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(c.session.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(snapshot.Patterns) != 0 {
		t.Fatalf("snapshot = %#v, available = %v", snapshot, available)
	}
}

func TestSkillPatternSnapshotAtomicWriteFailureDoesNotFailRun(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "skills"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := skillPatternSnapshotTestCoordinator(workspace)
	c.skillDetector = skill.NewSkillPatternDetector(1, 2, 2)

	// checkSkillPatterns intentionally returns no error. Projection failures are
	// warnings and cannot change the coordinator run outcome.
	c.checkSkillPatterns()
}

func TestSkillPatternSnapshotReusesDraftNameByPatternID(t *testing.T) {
	c := skillPatternSnapshotTestCoordinator(t.TempDir())
	candidate := teamSnapshotTestCandidate(time.Now().UTC())
	patternID, err := skill.PatternCandidateID(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{candidate}, []skill.SavedPatternDraft{{
		PatternID: patternID,
		Name:      "draft-reused",
		Path:      filepath.Join(t.TempDir(), "draft-reused", "SKILL.md"),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}

	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(c.session.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(snapshot.Patterns) != 1 || snapshot.Patterns[0].DraftName != "draft-reused" {
		t.Fatalf("snapshot = %#v, available = %v", snapshot, available)
	}
}

func TestSavedPatternDraftStoresNameWithoutPersistingPath(t *testing.T) {
	candidate := teamSnapshotTestCandidate(time.Now().UTC())
	draft, err := savedPatternDraft(candidate, filepath.Join("private", "team", "skills", "drafts", "draft-safe", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if draft.Name != "draft-safe" || draft.Path == "" || draft.PatternID == "" {
		t.Fatalf("saved draft = %#v", draft)
	}
}

func skillPatternSnapshotTestCoordinator(workspace string) *Coordinator {
	return &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "test-team"},
		},
		executionRunID: "run-test",
	}
}

func teamSnapshotTestCandidate(now time.Time) skill.PatternCandidate {
	sum := sha256.Sum256([]byte("team-snapshot-source"))
	sourceID := hex.EncodeToString(sum[:])
	return skill.PatternCandidate{
		Sequence: &skill.ToolSequence{
			Tools:       []string{"view", "edit"},
			Params:      []string{"*.go", "*.go"},
			Hash:        sourceID,
			Count:       3,
			FirstSeen:   now.Add(-time.Minute),
			LastSeen:    now,
			Agent:       "coder",
			AgentCounts: map[string]int{"coder": 3},
		},
		SourcePatternIDs: []string{sourceID},
	}
}
