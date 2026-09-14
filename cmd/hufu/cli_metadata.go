package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type commandGroupMetadata struct {
	ID, Title string
	Commands  []string
}

var commandGroups = []commandGroupMetadata{
	{ID: "execution", Title: "Execution", Commands: []string{"run", "chat"}},
	{ID: "team", Title: "Teams and readiness", Commands: []string{"team", "doctor", "init", "list"}},
	{ID: "progress", Title: "Progress and recovery", Commands: []string{"session", "inspect", "status", "resume", "retry", "reconcile", "history"}},
	{ID: "experience", Title: "Context and skills", Commands: []string{"context", "skill", "install"}},
	{ID: "environment", Title: "Environment", Commands: []string{"config", "models", "version"}},
	{ID: "advanced", Title: "Advanced and diagnostics", Commands: []string{"audit", "decision", "improve", "eval", "debug", "terminal", "migrate", "completion", "__complete", "examples", "help-flags"}},
}

type exampleMetadata struct {
	Section string
	Argv    string
}

var canonicalExamples = []exampleMetadata{
	{Section: "Quick start", Argv: "hufu doctor"},
	{Section: "Quick start", Argv: "hufu init dev-team --template dev"},
	{Section: "Quick start", Argv: `hufu run --team dev-team -- "review this codebase"`},
	{Section: "Interactive", Argv: "hufu chat --agent-team dev-team"},
	{Section: "Interactive", Argv: `hufu run --team dev-team -- "implement a feature"`},
	{Section: "Automation", Argv: `hufu run --team dev-team --quiet --output json --event-format jsonl -- "run checks"`},
	{Section: "Automation", Argv: `hufu run --team ops-team --unattended -- "check service health"`},
}

var cliFlagGroups = map[string][]string{
	"core":       {"team", "agent-team", "model", "coordinator-model", "default", "workspace", "workspace-root"},
	"execution":  {"route", "plan", "auto-skills", "steps", "dry-run", "timeout", "max-rounds", "input", "input-file"},
	"output":     {"verbose", "quiet", "output", "event-format", "report", "think"},
	"safety":     {"rbash", "no-net", "force-mcp", "allow-path", "unattended", "auto-approve"},
	"security":   {"rbash", "no-net", "force-mcp", "allow-path", "direnv"},
	"unattended": {"unattended", "max-duration", "max-total-tokens", "auto-approve"},
	"display":    {"tui", "tui-compact", "display-mode", "theme", "display-preset", "no-color", "no-spinner"},
	"advanced":   {"provider-url", "provider-api-key", "profile", "var", "var-file", "memory", "template"},
}

// flagGroups is retained as the characterized package seam; both names point
// at the single metadata registry above.
var flagGroups = cliFlagGroups

func configureCommandDiscovery(root *cobra.Command) {
	for _, group := range commandGroups {
		root.AddGroup(&cobra.Group{ID: group.ID, Title: group.Title + ":"})
		for _, name := range group.Commands {
			if command, _, err := root.Find([]string{name}); err == nil && command != root {
				command.GroupID = group.ID
			}
		}
	}
	root.InitDefaultHelpCmd()
	showAll := false
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(command *cobra.Command, args []string) {
		if command == root && !showAll {
			_ = renderCompactRootHelp(command.OutOrStdout())
			return
		}
		defaultHelp(command, args)
	})
	if help, _, err := root.Find([]string{"help"}); err == nil && help != root && help.Flags().Lookup("all") == nil {
		help.Flags().BoolVar(&showAll, "all", false, "Show every command group, including advanced commands")
	}
}

func renderCompactRootHelp(writer io.Writer) error {
	if _, err := fmt.Fprint(writer, "hufu discovers and runs agent teams that collaborate on tasks.\nTeam selection: --team / --agent-team\n\n"); err != nil {
		return err
	}
	for _, group := range commandGroups {
		if group.ID == "advanced" {
			continue
		}
		if _, err := fmt.Fprintf(writer, "%-24s %s\n", group.Title, strings.Join(group.Commands, "  ")); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(writer, "\nAdvanced tools: audit / decision / improve / eval / debug / terminal / migrate\nAll commands: hufu help --all\nExamples: hufu examples\nFlag groups: hufu help-flags <group>\n")
	return err
}
