package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init <team-name>",
	Short: "Scaffold a minimal runnable agent team",
	Long: `Create a minimal team under .agent-teams/<team-name>/ so you can start
calling agents immediately. This is a compatibility alias for
"hufu team create": --template maps onto an equivalent team preset
(spec.md Specification 01 §8a). team.yaml is written only when --model is
given, since every other setting already has a built-in default.

Existing files are never overwritten.

Examples:
  hufu init dev-team
  hufu init dev-team --model local-model
  hufu init dev-team --template dev`,
	Args: cobra.ExactArgs(1),
	RunE: runInit,
}

func runInit(_ *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu init <team-name>")
	}

	templateName := strings.ToLower(strings.TrimSpace(opts.initTemplateName))
	if templateName == "" {
		templateName = "default"
	}
	presetName, ok := templateToTeamPreset[templateName]
	if !ok {
		return fmt.Errorf("unknown --template %q: supported templates: %s", opts.initTemplateName, strings.Join(templateNames(), ", "))
	}

	files, err := resolvePresetFiles(presetName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(opts.modelOverride) != "" {
		files["team.yaml"] = "model: " + opts.modelOverride + "\n"
	}

	teamDir := filepath.Join(".agent-teams", name)
	if err := writeTeamFilesWithValidation(teamDir, files, false); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\n%s Team %s is ready.\n", doneStyle.Render("✓"), teamStyle.Render(name))
	fmt.Fprintf(os.Stderr, "  Try it:   hufu @%s \"say hello\"\n", name)
	fmt.Fprintf(os.Stderr, "  Inspect:  hufu list %s\n", name)
	fmt.Fprintf(os.Stderr, "  Preflight: hufu doctor\n")
	return nil
}
