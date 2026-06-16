package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	skillCmd = &cobra.Command{
		Use:   "skill",
		Short: "Manage auto-generated skills",
		Long:  `Manage auto-generated skill drafts and detected patterns.`,
	}
)

func init() {
	skillCmd.AddCommand(skillReviewCmd)
	skillCmd.AddCommand(skillListCmd)
	skillListCmd.Flags().BoolVar(&draftsOnly, "drafts-only", false, "Show only draft skills")
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
		return fmt.Errorf("skill not found: %s", skillName)
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
			sb.WriteString(fmt.Sprintf("  [draft] %s\n", s.Name))
		} else {
			sb.WriteString(fmt.Sprintf("  - %s\n", s.Name))
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
	if workspace != "" {
		return workspace
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "workspace")
}
