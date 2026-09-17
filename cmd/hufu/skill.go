package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/skill"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

var (
	draftsOnly    bool
	skillTeamName string
)

var (
	skillReviewCmd = &cobra.Command{
		Use:   "review <skill-name>",
		Short: "Display an auto-generated skill draft for review",
		Long: `Display an auto-generated skill draft for review.

This command prints the draft path and content. It does not open an editor or
promote the draft; use 'hufu skill promote <draft-name>' after review.

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
	skillCmd.AddCommand(skillGraphCmd)
	skillListCmd.Flags().BoolVar(&draftsOnly, "drafts-only", false, "Show only draft skills")
	skillGraphCmd.Flags().StringVar(&skillGraphFormat, "format", "text", "Output format: text, json, or mermaid")
	skillGraphCmd.Flags().StringVar(&skillGraphAgent, "agent", "", "Include only patterns attributed to this agent")
	skillGraphCmd.Flags().Int64Var(&skillGraphMinFrequency, "min-frequency", 0, "Include only patterns with at least this count")
	for _, lifecycleCmd := range []*cobra.Command{skillReviewCmd, skillListCmd, skillPromoteCmd, skillCleanCmd} {
		lifecycleCmd.Flags().StringVar(&skillTeamName, "team", "", "Manage skills for this discoverable team")
		lifecycleCmd.Flags().StringVar(&opts.agentTeamSearchPath, "agent-team-search-path", "", "Comma-separated paths to search for teams")
	}

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
	target, err := resolveSkillLifecycleTarget()
	if err != nil {
		return err
	}
	skills := skill.DiscoverSkills(target.discoveryDirs, true)
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

	var output strings.Builder
	fmt.Fprintf(&output, "Found skill: %s\n\n", found.Path)
	fmt.Fprintln(&output, strings.Repeat("=", 80))
	fmt.Fprintln(&output, found.Content)
	fmt.Fprintln(&output, strings.Repeat("=", 80))
	return writeSkillCommandOutput(cmd, output.String())
}

func runSkillList(cmd *cobra.Command, args []string) error {
	target, err := resolveSkillLifecycleTarget()
	if err != nil {
		return err
	}
	output := listAvailableSkills(target.discoveryDirs, draftsOnly)
	if output == "" {
		return writeSkillCommandOutput(cmd, "No skills found.\n")
	}

	heading := "Available skills:\n"
	if draftsOnly {
		heading = "Available draft skills:\n"
	}
	return writeSkillCommandOutput(cmd, heading+output+"\n")
}

func runSkillPromote(cmd *cobra.Command, args []string) error {
	draftName := args[0]
	target, err := resolveSkillLifecycleTarget()
	if err != nil {
		return err
	}

	newPath, err := skill.PromoteDraft(target.skillsDir, draftName)
	if err != nil {
		return err
	}
	return writeSkillCommandOutput(cmd, fmt.Sprintf("Promoted: %s -> %s\n", draftName, newPath))
}

func runSkillClean(cmd *cobra.Command, args []string) error {
	target, err := resolveSkillLifecycleTarget()
	if err != nil {
		return err
	}

	var olderThan time.Duration
	if skillCleanOlderThan != "" {
		d, err := time.ParseDuration(skillCleanOlderThan)
		if err != nil {
			return fmt.Errorf("invalid --older-than: %w", err)
		}
		olderThan = d
	}

	result, err := skill.CleanDrafts(target.skillsDir, skill.CleanOpts{
		OlderThan:  olderThan,
		UnusedOnly: skillCleanUnused,
		DryRun:     !skillCleanApply,
		UsageDir:   target.usageDir,
	})
	if err != nil {
		return err
	}

	if len(result.Deleted) == 0 {
		return writeSkillCommandOutput(cmd, "No drafts match the criteria.\n")
	}

	var output strings.Builder
	if skillCleanApply {
		fmt.Fprintf(&output, "Deleted %d drafts:\n", len(result.Deleted))
	} else {
		fmt.Fprintf(&output, "Would delete %d drafts (dry-run; use --apply to delete):\n", len(result.Deleted))
	}
	for _, name := range result.Deleted {
		fmt.Fprintf(&output, "  - %s\n", name)
	}
	if err := writeSkillCommandOutput(cmd, output.String()); err != nil {
		return err
	}
	if !skillCleanApply && !skillCleanYes {
		prompt := promptui.Prompt{
			Label:     "Apply",
			IsConfirm: true,
		}
		_, err := prompt.Run()
		if err != nil {
			return writeSkillCommandOutput(cmd, "Aborted.\n")
		}
		result, err = skill.CleanDrafts(target.skillsDir, skill.CleanOpts{
			OlderThan:  olderThan,
			UnusedOnly: skillCleanUnused,
			DryRun:     false,
			UsageDir:   target.usageDir,
		})
		if err != nil {
			return err
		}
		return writeSkillCommandOutput(cmd, fmt.Sprintf("Deleted %d drafts.\n", len(result.Deleted)))
	}
	return nil
}

func writeSkillCommandOutput(cmd *cobra.Command, output string) error {
	if _, err := fmt.Fprint(cmd.OutOrStdout(), output); err != nil {
		return fmt.Errorf("write skill command output: %w", err)
	}
	return nil
}

func listAvailableSkills(skillDirs []string, draftsOnly bool) string {
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

type skillLifecycleTarget struct {
	skillsDir     string
	discoveryDirs []string
	usageDir      string
}

func resolveSkillLifecycleTarget() (skillLifecycleTarget, error) {
	if strings.TrimSpace(skillTeamName) != "" {
		teamDir, err := resolveTeamDirArg(nil, skillTeamName)
		if err != nil {
			return skillLifecycleTarget{}, fmt.Errorf("resolve skill team: %w", err)
		}
		skillsDir := filepath.Join(teamDir, "skills")
		teamName := strings.ToLower(strings.TrimSpace(skillTeamName))
		usageDir := ""
		if strings.TrimSpace(opts.workspace) != "" {
			usageDir = filepath.Join(opts.workspace, teamName)
		} else {
			usageDir, err = resolveSkillWorkspace(teamName)
		}
		if err != nil {
			return skillLifecycleTarget{}, err
		}
		return skillLifecycleTarget{skillsDir: skillsDir, discoveryDirs: []string{skillsDir}, usageDir: usageDir}, nil
	}

	workspace, err := resolveSkillWorkspace("default")
	if err != nil {
		return skillLifecycleTarget{}, err
	}
	teamDir := filepath.Join(workspace, "..")
	return skillLifecycleTarget{
		skillsDir:     filepath.Join(workspace, "skills"),
		discoveryDirs: buildSkillDirs(workspace, teamDir),
		usageDir:      workspace,
	}, nil
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
	teamName := strings.ToLower(strings.TrimSpace(opts.agentTeamName))
	if teamName == "" {
		teamName = "default"
	}
	workspace, err := resolveExistingManagedWorkspacePath(context.Background(), runtimeStartDir(), teamName)
	if err != nil {
		return ""
	}
	return workspace
}

func resolveSkillWorkspace(teamName string) (string, error) {
	if strings.TrimSpace(opts.workspace) != "" {
		return workspacepkg.CanonicalExistingDirectory(opts.workspace)
	}
	workspace, err := resolveExistingManagedWorkspacePath(context.Background(), runtimeStartDir(), teamName)
	if err != nil {
		return "", fmt.Errorf("resolve managed skill workspace: %w", err)
	}
	return workspace, nil
}
