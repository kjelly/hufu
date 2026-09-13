package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/skill"
)

func TestSkillLifecycleCommandsRegisterNamedTeamFlags(t *testing.T) {
	for _, command := range []*cobra.Command{skillReviewCmd, skillListCmd, skillPromoteCmd, skillCleanCmd} {
		if command.Flags().Lookup("team") == nil {
			t.Errorf("%s does not register --team", command.CommandPath())
		}
		if command.Flags().Lookup("agent-team-search-path") == nil {
			t.Errorf("%s does not register --agent-team-search-path", command.CommandPath())
		}
	}
}

func TestSkillLifecycleTargetsNamedTeam(t *testing.T) {
	searchRoot := t.TempDir()
	teamDir := filepath.Join(searchRoot, "named-team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: display-name\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"draft-review", "draft-promote", "draft-clean"} {
		writeSkillLifecycleDraft(t, teamDir, name)
	}

	previousOptions := opts
	previousTeamName := skillTeamName
	previousDraftsOnly := draftsOnly
	previousCleanApply := skillCleanApply
	previousCleanYes := skillCleanYes
	previousCleanUnused := skillCleanUnused
	previousCleanOlderThan := skillCleanOlderThan
	t.Cleanup(func() {
		opts = previousOptions
		skillTeamName = previousTeamName
		draftsOnly = previousDraftsOnly
		skillCleanApply = previousCleanApply
		skillCleanYes = previousCleanYes
		skillCleanUnused = previousCleanUnused
		skillCleanOlderThan = previousCleanOlderThan
	})
	opts.agentTeamSearchPath = searchRoot
	baseWorkspace := t.TempDir()
	opts.workspace = baseWorkspace
	skillTeamName = "named-team"
	usageDir := filepath.Join(baseWorkspace, "named-team")
	if err := os.MkdirAll(usageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skill.RecordUsage(usageDir, "draft-review", "worker"); err != nil {
		t.Fatal(err)
	}

	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runSkillList(command, nil); err != nil {
		t.Fatalf("runSkillList() error = %v", err)
	}
	if !strings.Contains(output.String(), "[draft] draft-review") {
		t.Fatalf("named-team skill list output = %q", output.String())
	}

	output.Reset()
	if err := runSkillReview(command, []string{"draft-review"}); err != nil {
		t.Fatalf("runSkillReview() error = %v", err)
	}
	if !strings.Contains(output.String(), filepath.Join(teamDir, "skills", "drafts", "draft-review", "SKILL.md")) {
		t.Fatalf("named-team review output = %q", output.String())
	}

	output.Reset()
	if err := runSkillPromote(command, []string{"draft-promote"}); err != nil {
		t.Fatalf("runSkillPromote() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "skills", "promote", "SKILL.md")); err != nil {
		t.Fatalf("promoted named-team skill: %v", err)
	}

	skillCleanApply = true
	skillCleanYes = true
	skillCleanUnused = true
	output.Reset()
	if err := runSkillClean(command, nil); err != nil {
		t.Fatalf("runSkillClean() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "skills", "drafts", "draft-clean")); !os.IsNotExist(err) {
		t.Fatalf("named-team draft was not cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "skills", "drafts", "draft-review", "SKILL.md")); err != nil {
		t.Fatalf("used named-team draft was cleaned: %v", err)
	}
}

func writeSkillLifecycleDraft(t *testing.T, teamDir, name string) {
	t.Helper()
	draftDir := filepath.Join(teamDir, "skills", "drafts", name)
	if err := os.MkdirAll(draftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: lifecycle test\n---\n\n# %s\n", name, name)
	if err := os.WriteFile(filepath.Join(draftDir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
