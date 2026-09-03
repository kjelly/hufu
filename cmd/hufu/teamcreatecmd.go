package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var (
	teamCreatePreset   string
	teamCreateFrom     string
	teamCreateModel    string
	teamCreateForce    bool
	teamCreateExpanded bool
)

var teamCreateCmd = &cobra.Command{
	Use:   "create NAME",
	Short: "Create the smallest runnable agent team",
	Long: `Create a new team under .agent-teams/<name>/.

With no flags, scaffolds a single generic worker (the coding-single team
preset). --preset selects a deterministic team preset (coding-single,
coding-reviewed, research, safe-ops) or a single-worker agent preset
(readonly, coding, review, research, writer, ops). --from generates a
task-specific team from a natural-language description using the same
deterministic classifier as ` + "`hufu team generate`" + ` (no model call).

A small team.yaml is written only when --model is given, since every
other setting already has a built-in default; --expanded additionally
pins the built-in defaults for users who prefer configuration pinning.

The generated team is compiled and validated before anything is reported
as ready. Existing directories are never overwritten without --force.

Examples:
  hufu team create dev
  hufu team create dev --preset coding-reviewed
  hufu team create dev --preset coding-reviewed --model ollama/qwen3.5:27b
  hufu team create oauth-fix --from "Fix OAuth callback bugs and add regression tests"`,
	Args: cobra.ExactArgs(1),
	RunE: runTeamCreate,
}

func init() {
	teamCmd.AddCommand(teamCreateCmd)
	teamCreateCmd.Flags().StringVar(&teamCreatePreset, "preset", "", "Team or agent preset name")
	teamCreateCmd.Flags().StringVar(&teamCreateFrom, "from", "", "Generate a task-specific team from a natural-language description")
	teamCreateCmd.Flags().StringVar(&teamCreateModel, "model", "", "Pin a model in team.yaml")
	teamCreateCmd.Flags().BoolVar(&teamCreateForce, "force", false, "Overwrite an existing team directory")
	teamCreateCmd.Flags().BoolVar(&teamCreateExpanded, "expanded", false, "Write a fully explicit team.yaml pinning built-in defaults")
}

func runTeamCreate(_ *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu team create <name>")
	}
	if strings.TrimSpace(teamCreatePreset) != "" && strings.TrimSpace(teamCreateFrom) != "" {
		return fmt.Errorf("--preset and --from cannot be used together")
	}

	var files map[string]string
	if strings.TrimSpace(teamCreateFrom) != "" {
		normalized, err := normalizeGeneratedTeamName(name)
		if err != nil {
			return err
		}
		name = normalized
		generated := buildGeneratedTeam(name, teamCreateFrom, teamCreateModel)
		if err := validateGeneratedTeam(generated); err != nil {
			return fmt.Errorf("generated team is invalid: %w", err)
		}
		files = generated.Files
	} else {
		presetName := teamCreatePreset
		if strings.TrimSpace(presetName) == "" {
			presetName = "coding-single"
		}
		resolved, err := resolvePresetFiles(presetName)
		if err != nil {
			return err
		}
		files = resolved
		if strings.TrimSpace(teamCreateModel) != "" {
			files["team.yaml"] = "model: " + teamCreateModel + "\n"
		}
	}

	if teamCreateExpanded {
		files["team.yaml"] = applyExpandedDefaults(files["team.yaml"])
	}

	teamDir := filepath.Join(".agent-teams", name)
	if err := writeTeamFilesWithValidation(teamDir, files, teamCreateForce); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n%s Team created: %s\n", doneStyle.Render("✓"), teamStyle.Render(teamDir))
	fmt.Fprintf(os.Stderr, "\nTry it:\n  hufu @%s \"say hello\"\n", name)
	fmt.Fprintf(os.Stderr, "\nInspect:\n  hufu team explain %s\n", teamDir)
	fmt.Fprintf(os.Stderr, "\nValidate:\n  hufu team validate %s\n", teamDir)
	return nil
}
