package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

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
}

func runSkillReview(cmd *cobra.Command, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("skill name required\nUsage: hufu skill review <skill-name>")
	}

	skillName := args[0]
	workspace := getWorkspace()

	// Look for skill in workspace/skills/ directory
	skillDir := filepath.Join(workspace, "skills", skillName)
	skillPath := filepath.Join(skillDir, "SKILL.md")

	// Also check team directory
	teamDir := filepath.Join(workspace, "..")
	skillPathTeam := filepath.Join(teamDir, "skills", skillName, "SKILL.md")

	var foundPath string
	if _, err := os.Stat(skillPath); err == nil {
		foundPath = skillPath
	} else if _, err := os.Stat(skillPathTeam); err == nil {
		foundPath = skillPathTeam
	} else {
		return fmt.Errorf("skill draft not found: %s\n\nAvailable drafts:\n%s",
			skillName, listAvailableDrafts(workspace, teamDir))
	}

	// Read and display the skill
	content, err := os.ReadFile(foundPath)
	if err != nil {
		return fmt.Errorf("failed to read skill: %w", err)
	}

	fmt.Printf("Found skill draft: %s\n\n", foundPath)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println(string(content))
	fmt.Println(strings.Repeat("=", 80))
	fmt.Println()
	fmt.Println("To edit this skill:")
	fmt.Printf("  1. Open the file: %s\n", foundPath)
	fmt.Println("  2. Edit the content to refine the workflow")
	fmt.Println("  3. Save the file")
	fmt.Println()
	fmt.Println("The skill will be automatically available via load_skill once edited.")

	return nil
}

func runSkillList(cmd *cobra.Command, args []string) error {
	workspace := getWorkspace()
	teamDir := filepath.Join(workspace, "..")

	drafts := listAvailableDrafts(workspace, teamDir)
	if drafts == "" {
		fmt.Println("No skill drafts found.")
		return nil
	}

	fmt.Println("Available skill drafts:")
	fmt.Println(drafts)
	return nil
}

func listAvailableDrafts(workspace, teamDir string) string {
	var drafts []string

	// Check workspace/skills/
	skillsDir := filepath.Join(workspace, "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "draft-") {
				skillPath := filepath.Join(skillsDir, entry.Name(), "SKILL.md")
				if _, err := os.Stat(skillPath); err == nil {
					drafts = append(drafts, entry.Name())
				}
			}
		}
	}

	// Check team/skills/
	teamSkillsDir := filepath.Join(teamDir, "skills")
	if entries, err := os.ReadDir(teamSkillsDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "draft-") {
				skillPath := filepath.Join(teamSkillsDir, entry.Name(), "SKILL.md")
				if _, err := os.Stat(skillPath); err == nil {
					if !contains(drafts, entry.Name()) {
						drafts = append(drafts, entry.Name())
					}
				}
			}
		}
	}

	if len(drafts) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, draft := range drafts {
		sb.WriteString(fmt.Sprintf("  - %s\n", draft))
	}
	return sb.String()
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func getWorkspace() string {
	if workspace != "" {
		return workspace
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "workspace")
}
