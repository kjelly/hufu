package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	c.skillDetector = skill.NewSkillPatternDetector(1, 2, 2)
	c.checkSkillPatterns(t.Context())

	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(c.session.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(snapshot.Patterns) != 0 {
		t.Fatalf("snapshot = %#v, available = %v", snapshot, available)
	}
}

func TestSkillPatternAnalysisRequiresAutoSkills(t *testing.T) {
	workspace := t.TempDir()
	c := skillPatternSnapshotTestCoordinator(workspace)
	c.autoSkillsEnabled = false
	c.skillDetector = skill.NewSkillPatternDetector(1, 2, 2)

	c.checkSkillPatterns(t.Context())

	if _, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(workspace)); err != nil {
		t.Fatal(err)
	} else if available {
		t.Fatal("skill pattern snapshot written while auto-skills was disabled")
	}
}

func TestSkillPatternRecordingRequiresAutoSkillsAndIgnoresControlTools(t *testing.T) {
	c := skillPatternSnapshotTestCoordinator(t.TempDir())
	c.skillDetector = skill.NewSkillPatternDetector(1, 2, 2)
	c.autoSkillsEnabled = false
	c.recordSkillPatternToolCall("coder", "view", `{}`, "inspect")
	if got := c.skillDetector.GetToolCallCount(); got != 0 {
		t.Fatalf("disabled auto-skills recorded %d calls, want 0", got)
	}

	c.autoSkillsEnabled = true
	c.recordSkillPatternToolCall("coder", "submit_result", `{}`, "finish")
	if got := c.skillDetector.GetToolCallCount(); got != 0 {
		t.Fatalf("runtime control tool recorded %d calls, want 0", got)
	}
	c.recordSkillPatternToolCall("coder", "view", `{}`, "inspect")
	if got := c.skillDetector.GetToolCallCount(); got != 1 {
		t.Fatalf("ordinary tool calls recorded = %d, want 1", got)
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
	c.checkSkillPatterns(t.Context())
}

func TestSkillPatternEvaluationDoesNotReplaceSnapshotAfterCancellation(t *testing.T) {
	c := skillPatternSnapshotTestCoordinator(t.TempDir())
	candidate := teamSnapshotTestCandidate(time.Now().UTC())
	if err := c.persistSkillPatternSnapshot([]skill.PatternCandidate{candidate}, nil); err != nil {
		t.Fatal(err)
	}

	detector := skill.NewSkillPatternDetector(1, 2, 2)
	detector.RecordToolCall("coder", "view", `{"path":"file.go"}`, "inspect")
	detector.RecordToolCall("coder", "edit", `{"path":"file.go"}`, "change")
	detector.SetModelInvoker(failingIfCalledSkillInvoker{t: t})
	c.skillDetector = detector
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	c.checkSkillPatterns(canceled)

	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(c.session.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !available || len(snapshot.Patterns) != 1 {
		t.Fatalf("canceled evaluation replaced snapshot: %#v, available = %v", snapshot, available)
	}
}

func TestRunDirectAgentEvaluatesSkillPatternsAtInvocationBoundary(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.autoSkillsEnabled = true
	c.skillDetector = skill.NewSkillPatternDetector(1, 2, 2)

	if _, err := c.RunDirectAgent(t.Context(), "worker", "perform direct work"); err != nil {
		t.Fatalf("RunDirectAgent() error = %v", err)
	}

	snapshot, available, err := skill.LoadSkillPatternSnapshot(skill.SkillPatternSnapshotPath(c.session.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !available || snapshot.RunID == "" || snapshot.TeamName != "test" {
		t.Fatalf("direct-agent pattern snapshot = %#v, available = %v", snapshot, available)
	}
}

type failingIfCalledSkillInvoker struct{ t *testing.T }

func (i failingIfCalledSkillInvoker) Invoke(context.Context, string, string) (string, error) {
	i.t.Fatal("canceled skill-pattern evaluation invoked the sidecar")
	return "", context.Canceled
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

func TestSkillDraftReviewCommandTargetsNamedTeamDirectory(t *testing.T) {
	searchPath := filepath.Join(t.TempDir(), ".agent-teams")
	c := &Coordinator{session: &TeamSession{
		Dir:       filepath.Join(searchPath, "directory-name"),
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "display-name"},
	}}
	got := c.skillDraftReviewCommand("draft-view-edit")
	want := fmt.Sprintf(
		`hufu skill review "draft-view-edit" --team "directory-name" --agent-team-search-path %s`,
		strconv.Quote(searchPath),
	)
	if got != want {
		t.Fatalf("skillDraftReviewCommand() = %q, want %q", got, want)
	}
}

func skillPatternSnapshotTestCoordinator(workspace string) *Coordinator {
	return &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "test-team"},
		},
		executionRunID:    "run-test",
		autoSkillsEnabled: true,
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
