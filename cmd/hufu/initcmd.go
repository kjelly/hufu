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
calling agents immediately. Generates a team.yaml and a single worker agent.

Existing files are never overwritten.

Examples:
  hufu init dev-team
  hufu init dev-team --model ollama/qwen3:8b`,
	Args: cobra.ExactArgs(1),
	RunE: runInit,
}

const teamYAMLTemplate = `name: %[1]s
description: Scaffolded by hufu init
max-rounds: 10
timeout: 600
max-retries: 2
workspace: workspace
%[2]s`

const agentMDTemplate = `---
name: helper
description: General-purpose worker
role: worker
tools: view,write,edit,multiedit,grep,glob,ls,bash
---
You are a capable, careful worker on the %[1]s team.

Complete the task you are given end to end. Prefer reading existing files before
editing them, keep changes minimal and idiomatic, and report concisely what you
did. If a task is ambiguous, make the most reasonable assumption and state it.
`

func runInit(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return fmt.Errorf("team name required: hufu init <team-name>")
	}

	switch initTemplateName {
	case "", "default":
		// the only template currently supported
	default:
		return fmt.Errorf("unknown --template %q: supported templates: default", initTemplateName)
	}

	teamDir := filepath.Join(".agent-teams", name)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		return fmt.Errorf("failed to create %s: %w", teamDir, err)
	}

	modelLine := ""
	if modelOverride != "" {
		modelLine = "model: " + modelOverride + "\n"
	}

	created, err := writeIfAbsent(
		filepath.Join(teamDir, "team.yaml"),
		fmt.Sprintf(teamYAMLTemplate, name, modelLine),
	)
	if err != nil {
		return err
	}
	reportScaffold(filepath.Join(teamDir, "team.yaml"), created)

	created, err = writeIfAbsent(
		filepath.Join(teamDir, "helper.md"),
		fmt.Sprintf(agentMDTemplate, name),
	)
	if err != nil {
		return err
	}
	reportScaffold(filepath.Join(teamDir, "helper.md"), created)

	fmt.Fprintf(os.Stderr, "\n%s Team %s is ready.\n", doneStyle.Render("✓"), teamStyle.Render(name))
	fmt.Fprintf(os.Stderr, "  Try it:   hufu @%s \"say hello\"\n", name)
	fmt.Fprintf(os.Stderr, "  Inspect:  hufu list %s\n", name)
	fmt.Fprintf(os.Stderr, "  Preflight: hufu doctor\n")
	return nil
}

// writeIfAbsent writes content to path only if it does not already exist.
// Returns true if the file was created, false if it was left untouched.
func writeIfAbsent(path, content string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return true, nil
}

func reportScaffold(path string, created bool) {
	if created {
		fmt.Fprintf(os.Stderr, "  %s created %s\n", doneStyle.Render("+"), path)
	} else {
		fmt.Fprintf(os.Stderr, "  %s %s already exists, left unchanged\n", dimStyle.Render("·"), path)
	}
}
