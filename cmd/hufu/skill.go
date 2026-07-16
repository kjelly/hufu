package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/skill"
)

var draftsOnly bool

var (
	skillReviewCmd = &cobra.Command{
		Use:   "review <skill-name>",
		Short: "Review and edit an auto-generated skill draft",
		Long: `Review and edit an auto-generated skill draft.

This command opens a skill draft file for review. You can edit the file
with your editor and then confirm to save it as a final skill.

Usage:
  hufu skill review <skill-name>

Examples:
  hufu skill review draft-view-edit-bash
  hufu skill review draft-code-modification`,
		RunE: runSkillReview,
	}

	skillListCmd = &cobra.Command{
		Use:   "list",
		Short: "List detected skill patterns and drafts",
		Long: `List all detected skill patterns and draft skills.

This shows patterns that were detected during execution and any
draft skills that were auto-generated.

Usage:
  hufu skill list`,
		RunE: runSkillList,
	}

	skillPromoteCmd = &cobra.Command{
		Use:   "promote <draft-name>",
		Short: "Promote a draft skill to a real skill",
		Long: `Move a draft skill from skills/drafts/<name>/ to skills/<name>/.

The "draft-" prefix is stripped from the directory name. After promotion,
the skill becomes a regular skill available to all agents.

Examples:
  hufu skill promote draft-view-edit-bash`,
		Args: cobra.ExactArgs(1),
		RunE: runSkillPromote,
	}

	skillCleanCmd = &cobra.Command{
		Use:   "clean",
		Short: "Clean up stale or unused draft skills",
		Long: `Remove draft skills that match the given criteria.

By default, this runs in dry-run mode and prints what would be deleted.
Use --apply to actually delete. Use --yes to skip the final confirmation.

Examples:
  hufu skill clean --older-than 30d --unused
  hufu skill clean --older-than 7d --apply --yes`,
		RunE: runSkillClean,
	}

	skillCmd = &cobra.Command{
		Use:   "skill",
		Short: "Manage auto-generated skills",
		Long:  `Manage auto-generated skill drafts and detected patterns.`,
	}
)

var (
	skillCleanOlderThan string
	skillCleanUnused    bool
	skillCleanApply     bool
	skillCleanYes       bool
)

func init() {
	skillCmd.AddCommand(skillReviewCmd)
	skillCmd.AddCommand(skillListCmd)
	skillCmd.AddCommand(skillPromoteCmd)
	skillCmd.AddCommand(skillCleanCmd)
	skillListCmd.Flags().BoolVar(&draftsOnly, "drafts-only", false, "Show only draft skills")

	skillCleanCmd.Flags().StringVar(&skillCleanOlderThan, "older-than", "", "Delete drafts older than this duration (e.g. 30d, 24h)")
	skillCleanCmd.Flags().BoolVar(&skillCleanUnused, "unused", false, "Only delete drafts that have never been used")
	skillCleanCmd.Flags().BoolVar(&skillCleanApply, "apply", false, "Actually delete (default is dry-run)")
	skillCleanCmd.Flags().BoolVar(&skillCleanYes, "yes", false, "Skip the final confirmation prompt")
}

func runSkillReview(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("skill name required\nUsage: hufu skill review <skill-name>")
	}

	skillName := args[0]
	workspace := getWorkspace()
	teamDir := filepath.Join(workspace, "..")

	skillDirs := buildSkillDirs(workspace, teamDir)
	skills := skill.DiscoverSkills(skillDirs, true)
	var found *skill.SkillDef
	for _, s := range skills {
		if strings.EqualFold(s.Name, skillName) {
			found = s
			break
		}
	}
	if found == nil {
		return fmt.Errorf("skill not found: %s\n  Run 'hufu skill list' to see available skills", skillName)
	}

	fmt.Printf("Found skill: %s\n\n", found.Path)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println(found.Content)
	fmt.Println(strings.Repeat("=", 80))
	return nil
}

func runSkillList(cmd *cobra.Command, args []string) error {
	workspace := getWorkspace()
	teamDir := filepath.Join(workspace, "..")

	output := listAvailableSkills(workspace, teamDir, draftsOnly)
	if output == "" {
		fmt.Println("No skills found.")
		return nil
	}

	if draftsOnly {
		fmt.Println("Available draft skills:")
	} else {
		fmt.Println("Available skills:")
	}
	fmt.Println(output)
	return nil
}

func runSkillPromote(cmd *cobra.Command, args []string) error {
	draftName := args[0]
	skillsDir := filepath.Join(getWorkspace(), "skills")

	newPath, err := skill.PromoteDraft(skillsDir, draftName)
	if err != nil {
		return err
	}
	fmt.Printf("Promoted: %s -> %s\n", draftName, newPath)
	return nil
}

func runSkillClean(cmd *cobra.Command, args []string) error {
	skillsDir := filepath.Join(getWorkspace(), "skills")

	var olderThan time.Duration
	if skillCleanOlderThan != "" {
		d, err := time.ParseDuration(skillCleanOlderThan)
		if err != nil {
			return fmt.Errorf("invalid --older-than: %w", err)
		}
		olderThan = d
	}

	result, err := skill.CleanDrafts(skillsDir, skill.CleanOpts{
		OlderThan:  olderThan,
		UnusedOnly: skillCleanUnused,
		DryRun:     !skillCleanApply,
	})
	if err != nil {
		return err
	}

	if len(result.Deleted) == 0 {
		fmt.Println("No drafts match the criteria.")
		return nil
	}

	if skillCleanApply {
		fmt.Printf("Deleted %d drafts:\n", len(result.Deleted))
	} else {
		fmt.Printf("Would delete %d drafts (dry-run; use --apply to delete):\n", len(result.Deleted))
	}
	for _, name := range result.Deleted {
		fmt.Printf("  - %s\n", name)
	}
	if !skillCleanApply && !skillCleanYes {
		prompt := promptui.Prompt{
			Label:     "Apply",
			IsConfirm: true,
		}
		_, err := prompt.Run()
		if err != nil {
			fmt.Println("Aborted.")
			return nil
		}
		result, err = skill.CleanDrafts(skillsDir, skill.CleanOpts{
			OlderThan:  olderThan,
			UnusedOnly: skillCleanUnused,
			DryRun:     false,
		})
		if err != nil {
			return err
		}
		fmt.Printf("Deleted %d drafts.\n", len(result.Deleted))
	}
	return nil
}

func listAvailableSkills(workspace, teamDir string, draftsOnly bool) string {
	skillDirs := buildSkillDirs(workspace, teamDir)
	skills := skill.DiscoverSkills(skillDirs, true)
	if len(skills) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, s := range skills {
		if draftsOnly && !s.Draft {
			continue
		}
		if !draftsOnly && s.Draft {
			fmt.Fprintf(&sb, "  [draft] %s\n", s.Name)
		} else {
			fmt.Fprintf(&sb, "  - %s\n", s.Name)
		}
	}
	return sb.String()
}

func buildSkillDirs(workspace, teamDir string) []string {
	dirs := []string{
		filepath.Join(workspace, "skills"),
		filepath.Join(teamDir, "skills"),
	}
	if abs, err := filepath.Abs(teamDir); err == nil {
		dirs = append(dirs, filepath.Join(abs, "skills"))
	}
	return dirs
}

func getWorkspace() string {
	if opts.workspace != "" {
		return opts.workspace
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "workspace")
}
